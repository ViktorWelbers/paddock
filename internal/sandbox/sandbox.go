// Package sandbox renders and provisions the per-session isolation set: the
// agent Pod and a NetworkPolicy that only allows egress to the Paddock
// gateway (plus DNS).
//
// Sessions share the control plane's namespace rather than getting one each.
// The isolation that matters is per-pod — the NetworkPolicy selects the
// session's own pod, the container carries its own CPU/memory limits, drops
// all capabilities, and gets no service-account token — none of which a
// namespace boundary was adding. What the namespace did add was a
// requirement for cluster-scoped RBAC to create and delete namespaces, which
// put paddock out of reach of anyone who can't grant it.
//
// Everything is rendered in one place so isolation upgrades (gVisor
// runtimeClass, Kata) are a config change, not a rearchitecture.
package sandbox

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// DefaultAgent is the agent kind whose provider env contract agentEnv falls
// back to, and the one --agent-image names.
const DefaultAgent = "claude"

const (
	labelSession   = "paddock.dev/session"
	labelManagedBy = "app.kubernetes.io/managed-by"

	// gatewayComponentLabel selects the gateway pods that sandboxes are
	// allowed to reach; the helm chart sets this label on the gateway.
	gatewayComponentLabel = "paddock.dev/component"
	gatewayComponentValue = "gateway"
)

// Spec describes one sandbox session.
type Spec struct {
	SessionID    string
	Namespace    string // where the sandbox runs: the control plane's own namespace
	User         string
	Agent        string // "claude" (default), "pi", ...; selects the env contract
	AgentImage   string // e.g. an image with Claude Code preinstalled
	GatewayURL   string // Anthropic-path gateway URL (ANTHROPIC_BASE_URL for claude)
	OpenAIURL    string // OpenAI-path gateway URL (for agents speaking openai-completions)
	Model        string // model id for agents that need one pinned (pi against vLLM)
	SessionToken string // session-scoped credential; never a real provider key
	// ClaudeOAuthSecret names a Secret holding CLAUDE_CODE_OAUTH_TOKEN. When
	// set, the claude agent runs in subscription/direct mode: it authenticates
	// to api.anthropic.com with the developer's Claude subscription token
	// instead of the gateway's metered API-key path (a subscription OAuth token
	// is a different auth scheme the gateway can't proxy). Empty = gateway mode.
	// The token is injected by reference (secretKeyRef), so it never lands in
	// the pod spec or etcd — same posture as the gateway's own provider key.
	ClaudeOAuthSecret string
	CPULimit          string // ceiling a session may burst to, e.g. "2"
	MemLimit          string // e.g. "4Gi"
	// Reserved floor, deliberately well under the limit: a coding agent is
	// bursty and idle most of the time, so reserving the whole limit would
	// strand cores it rarely uses and (as it did on a two-node cluster) leave
	// a sandbox unschedulable while the node sat mostly free.
	CPURequest string // e.g. "500m"
	MemRequest string // e.g. "2Gi"

	// EgressProxyURL is the gateway's CONNECT proxy for governed internet
	// access (package registries, git hosts — allowlisted and audited).
	// Empty disables the proxy env: the sandbox then has no route out at all.
	EgressProxyURL string
	// WorkspaceSizeLimit caps the /workspace emptyDir (default 2Gi).
	WorkspaceSizeLimit string
	// Placement decides which nodes sandboxes are allowed to land on.
	Placement Placement
}

// Placement is the operator's control over where agent workloads run. The
// zero value schedules sandboxes anywhere, which is fine for a homelab and
// wrong for a company: a sandbox runs code written by a model on behalf of
// whoever asked, and platform teams keep that off the nodes carrying
// everything else. NodeSelector plus Tolerations is how a cluster expresses
// "agents belong on the tainted pool"; RuntimeClassName is the seam for
// gVisor or Kata, where the container boundary itself gets stronger.
type Placement struct {
	NodeSelector      map[string]string
	Tolerations       []corev1.Toleration
	RuntimeClassName  string
	PriorityClassName string
}

// agentEnv renders the provider env contract for the agent kind. The
// session token always doubles as the API key: the gateway authenticates
// it and swaps in the real provider key (if the upstream has one at all).
func agentEnv(spec Spec) []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: "PADDOCK_SESSION", Value: spec.SessionID}}
	switch spec.Agent {
	case "pi":
		// The image's launch wrapper renders ~/.pi/agent/models.json from
		// these; models.json can't interpolate env into baseUrl itself.
		env = append(env,
			corev1.EnvVar{Name: "PADDOCK_OPENAI_BASE_URL", Value: spec.OpenAIURL},
			corev1.EnvVar{Name: "PADDOCK_MODEL", Value: spec.Model},
			corev1.EnvVar{Name: "PI_API_KEY", Value: spec.SessionToken},
		)
	default: // claude
		if spec.ClaudeOAuthSecret != "" {
			// Subscription/direct mode. Claude Code authenticates to
			// api.anthropic.com with the developer's Claude subscription token,
			// pulled by reference so the token never enters the pod spec. No
			// ANTHROPIC_BASE_URL: Claude Code uses its default endpoint instead
			// of the gateway (which speaks API keys, not subscription OAuth).
			// The call still leaves only through the governed egress proxy set
			// up by proxyEnv below, so it stays allowlisted and audited.
			env = append(env, corev1.EnvVar{
				Name: "CLAUDE_CODE_OAUTH_TOKEN",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: spec.ClaudeOAuthSecret},
						Key:                  "CLAUDE_CODE_OAUTH_TOKEN",
					},
				},
			})
		} else {
			env = append(env,
				corev1.EnvVar{Name: "ANTHROPIC_BASE_URL", Value: spec.GatewayURL},
				corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: spec.SessionToken},
			)
		}
	}
	env = append(env, proxyEnv(spec)...)
	return env
}

// proxyEnv renders the HTTP(S)_PROXY contract for governed egress: package
// managers and git tunnel through the gateway's CONNECT proxy, authenticated
// with the session token (the token is already in the env as the API key, so
// the proxy URL adds no new exposure). NO_PROXY keeps model-API traffic
// going straight to the gateway instead of looping through the proxy.
// Both cases are set: curl and friends only read the lowercase variants.
func proxyEnv(spec Spec) []corev1.EnvVar {
	if spec.EgressProxyURL == "" {
		return nil
	}
	u, err := url.Parse(spec.EgressProxyURL)
	if err != nil || u.Host == "" {
		return nil
	}
	u.User = url.UserPassword("paddock", spec.SessionToken)
	proxy := u.String()

	noProxy := []string{"localhost", "127.0.0.1"}
	for _, raw := range []string{spec.GatewayURL, spec.OpenAIURL} {
		if h := urlHostname(raw); h != "" && !slices.Contains(noProxy, h) {
			noProxy = append(noProxy, h)
		}
	}
	np := strings.Join(noProxy, ",")

	return []corev1.EnvVar{
		{Name: "HTTP_PROXY", Value: proxy},
		{Name: "HTTPS_PROXY", Value: proxy},
		{Name: "http_proxy", Value: proxy},
		{Name: "https_proxy", Value: proxy},
		{Name: "NO_PROXY", Value: np},
		{Name: "no_proxy", Value: np},
	}
}

func urlHostname(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// gatewayPorts collects the TCP ports of every gateway-side URL the sandbox
// legitimately talks to. They scope the netpol's gateway egress rule.
func gatewayPorts(spec Spec) []netv1.NetworkPolicyPort {
	tcp := corev1.ProtocolTCP
	var out []netv1.NetworkPolicyPort
	seen := map[int32]bool{}
	for _, raw := range []string{spec.GatewayURL, spec.OpenAIURL, spec.EgressProxyURL} {
		p, ok := urlPort(raw)
		if !ok || seen[p] {
			continue
		}
		seen[p] = true
		port := intstr.FromInt32(p)
		out = append(out, netv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}
	return out
}

func urlPort(raw string) (int32, bool) {
	if raw == "" {
		return 0, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return 0, false
	}
	if p := u.Port(); p != "" {
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n <= 0 || n > 65535 {
			return 0, false
		}
		return int32(n), true
	}
	switch u.Scheme {
	case "https":
		return 443, true
	default:
		return 80, true
	}
}

// runtimeClass turns the configured name into the optional field the pod
// spec wants: unset means "the cluster default", not "a class called """.
func runtimeClass(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}

// ResourceName returns the name shared by a session's pod and NetworkPolicy.
// Sessions share a namespace, so the name has to carry the session id.
func ResourceName(sessionID string) string {
	return "paddock-ses-" + sessionID
}

// Resources is the rendered isolation set for one session.
type Resources struct {
	Pod           *corev1.Pod
	NetworkPolicy *netv1.NetworkPolicy
}

// Render builds the isolation set without touching a cluster, so it can be
// unit-tested and dry-run.
func Render(spec Spec) (Resources, error) {
	if spec.SessionID == "" || spec.AgentImage == "" || spec.Namespace == "" {
		return Resources{}, fmt.Errorf("sandbox spec requires SessionID, AgentImage and Namespace")
	}
	if spec.Agent == "pi" {
		if spec.OpenAIURL == "" || spec.Model == "" {
			return Resources{}, fmt.Errorf("agent %q requires OpenAIURL and Model", spec.Agent)
		}
	} else if spec.GatewayURL == "" {
		return Resources{}, fmt.Errorf("sandbox spec requires GatewayURL")
	}
	if spec.CPULimit == "" {
		spec.CPULimit = "2"
	}
	if spec.MemLimit == "" {
		spec.MemLimit = "4Gi"
	}
	if spec.CPURequest == "" {
		spec.CPURequest = "500m"
	}
	if spec.MemRequest == "" {
		spec.MemRequest = "2Gi"
	}
	if spec.WorkspaceSizeLimit == "" {
		spec.WorkspaceSizeLimit = "2Gi"
	}
	workspaceLimit := resource.MustParse(spec.WorkspaceSizeLimit)
	ns := spec.Namespace
	name := ResourceName(spec.SessionID)
	labels := map[string]string{
		labelSession:   spec.SessionID,
		labelManagedBy: "paddock",
	}
	falseVal := false
	trueVal := true

	fsGroup := int64(10001)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			// The default token would let the agent talk to the k8s API.
			AutomountServiceAccountToken: &falseVal,
			RestartPolicy:                corev1.RestartPolicyNever,
			EnableServiceLinks:           &falseVal,
			NodeSelector:                 spec.Placement.NodeSelector,
			Tolerations:                  spec.Placement.Tolerations,
			PriorityClassName:            spec.Placement.PriorityClassName,
			RuntimeClassName:             runtimeClass(spec.Placement.RuntimeClassName),
			// fsGroup makes the workspace emptyDir writable for the
			// non-root agent uid (emptyDir mounts root:root otherwise).
			// The rest satisfies Pod Security Admission's "restricted"
			// profile, which is the posture a cluster running other
			// people's agents should be on: seccomp is the one control
			// PSA demands at the pod level, and without it the API server
			// rejects the sandbox outright.
			SecurityContext: &corev1.PodSecurityContext{
				FSGroup:        &fsGroup,
				RunAsNonRoot:   &trueVal,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Volumes: []corev1.Volume{{
				Name: "workspace",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &workspaceLimit},
				},
			}},
			Containers: []corev1.Container{{
				Name:  "agent",
				Image: spec.AgentImage,
				// The image's entrypoint holds the pod (tini + sleep);
				// `paddock attach` execs the agent with a TTY. Stdin/TTY stay
				// enabled so `kubectl attach` works as a fallback.
				Stdin:      true,
				TTY:        true,
				WorkingDir: "/workspace",
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "workspace",
					MountPath: "/workspace",
				}},
				Env: agentEnv(spec),
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(spec.CPURequest),
						corev1.ResourceMemory: resource.MustParse(spec.MemRequest),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(spec.CPULimit),
						corev1.ResourceMemory: resource.MustParse(spec.MemLimit),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &falseVal,
					RunAsNonRoot:             &trueVal,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}

	dnsPort := intstr.FromInt32(53)
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	netpol := &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: netv1.NetworkPolicySpec{
			// Only this session's pod. The namespace is shared with the
			// control plane, so an empty selector here would firewall the
			// server and gateway too.
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{labelSession: spec.SessionID}},
			PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeIngress, netv1.PolicyTypeEgress},
			// No ingress rules: nothing may connect in.
			Egress: []netv1.NetworkPolicyEgressRule{
				{
					// Only the Paddock gateway, and only its gateway ports:
					// the server shares the gateway pod (and its label), so
					// without the port list sandboxes could reach the
					// control-plane API.
					To: []netv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{gatewayComponentLabel: gatewayComponentValue},
						},
					}},
					Ports: gatewayPorts(spec),
				},
				{
					// DNS.
					Ports: []netv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dnsPort},
						{Protocol: &tcp, Port: &dnsPort},
					},
				},
			},
		},
	}

	// No ResourceQuota: it is namespace-scoped, and the namespace now holds
	// the control plane, so a per-session quota would meter the server and
	// gateway too. Its caps are covered elsewhere — cpu/memory by the
	// container limits above, and the pod/service/secret counts by the fact
	// that the agent has no service-account token and so cannot ask the API
	// server for anything at all.
	return Resources{Pod: pod, NetworkPolicy: netpol}, nil
}

// Provisioner creates and destroys sandboxes. The server depends on this
// interface so it can run without a cluster (local dev, tests).
type Provisioner interface {
	Create(ctx context.Context, spec Spec) error
	Delete(ctx context.Context, sessionID string) error
	// List reports the sessions that still have resources in the cluster,
	// which is not the same set the session store believes in — see
	// api.Handler.Reconcile.
	List(ctx context.Context) ([]string, error)
}

// Noop is used when no kubeconfig is configured: sessions exist in the
// control plane only. Useful for local development of the API surface.
type Noop struct{}

func (Noop) Create(context.Context, Spec) error     { return nil }
func (Noop) Delete(context.Context, string) error   { return nil }
func (Noop) List(context.Context) ([]string, error) { return nil, nil }

// K8s provisions sandboxes on a real cluster, into Namespace (its own).
type K8s struct {
	Client    kubernetes.Interface
	Namespace string
	// RESTConfig backs pods/exec (workspace transfer); nil disables it.
	RESTConfig *rest.Config
}

func (k *K8s) Create(ctx context.Context, spec Spec) error {
	spec.Namespace = k.Namespace
	res, err := Render(spec)
	if err != nil {
		return err
	}
	if _, err := k.Client.NetworkingV1().NetworkPolicies(k.Namespace).Create(ctx, res.NetworkPolicy, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create networkpolicy: %w", err)
	}
	// The pod goes last: it must never exist without its NetworkPolicy.
	if _, err := k.Client.CoreV1().Pods(k.Namespace).Create(ctx, res.Pod, metav1.CreateOptions{}); err != nil {
		// Don't leave the policy behind for a pod that never came up.
		_ = k.Client.NetworkingV1().NetworkPolicies(k.Namespace).Delete(ctx, res.NetworkPolicy.Name, metav1.DeleteOptions{})
		return fmt.Errorf("create pod: %w", err)
	}
	return nil
}

// List reports every session with resources still in the namespace, whether
// or not the control plane remembers it. Both object kinds are consulted:
// a half-finished delete can leave a NetworkPolicy behind, and a policy with
// no pod is still litter someone has to clean up by hand.
func (k *K8s) List(ctx context.Context) ([]string, error) {
	opts := metav1.ListOptions{LabelSelector: labelSession}
	seen := map[string]bool{}

	pods, err := k.Client.CoreV1().Pods(k.Namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list sandbox pods: %w", err)
	}
	for _, p := range pods.Items {
		seen[p.Labels[labelSession]] = true
	}
	policies, err := k.Client.NetworkingV1().NetworkPolicies(k.Namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list sandbox networkpolicies: %w", err)
	}
	for _, np := range policies.Items {
		seen[np.Labels[labelSession]] = true
	}

	out := make([]string, 0, len(seen))
	for id := range seen {
		if id != "" {
			out = append(out, id)
		}
	}
	slices.Sort(out) // deterministic, so logs and tests read the same twice
	return out, nil
}

// Delete removes the session's pod and policy. Deleting a namespace used to
// cascade for us; now each object goes explicitly, and the pod goes first so
// a sandbox is never left running without its NetworkPolicy.
func (k *K8s) Delete(ctx context.Context, sessionID string) error {
	name := ResourceName(sessionID)
	if err := k.Client.CoreV1().Pods(k.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod: %w", err)
	}
	if err := k.Client.NetworkingV1().NetworkPolicies(k.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete networkpolicy: %w", err)
	}
	return nil
}
