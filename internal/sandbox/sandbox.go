// Package sandbox renders and provisions the per-session isolation set: a
// dedicated Namespace, the agent Pod, a NetworkPolicy that only allows
// egress to the Paddock gateway (plus DNS), and a ResourceQuota.
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
)

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
	User         string
	Agent        string // "claude" (default), "pi", ...; selects the env contract
	AgentImage   string // e.g. an image with Claude Code preinstalled
	GatewayURL   string // Anthropic-path gateway URL (ANTHROPIC_BASE_URL for claude)
	OpenAIURL    string // OpenAI-path gateway URL (for agents speaking openai-completions)
	Model        string // model id for agents that need one pinned (pi against vLLM)
	SessionToken string // session-scoped credential; never a real provider key
	CPULimit     string // e.g. "2"
	MemLimit     string // e.g. "4Gi"

	// EgressProxyURL is the gateway's CONNECT proxy for governed internet
	// access (package registries, git hosts — allowlisted and audited).
	// Empty disables the proxy env: the sandbox then has no route out at all.
	EgressProxyURL string
	// WorkspaceSizeLimit caps the /workspace emptyDir (default 2Gi).
	WorkspaceSizeLimit string
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
		env = append(env,
			corev1.EnvVar{Name: "ANTHROPIC_BASE_URL", Value: spec.GatewayURL},
			corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: spec.SessionToken},
		)
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

// NamespaceName returns the per-session namespace.
func NamespaceName(sessionID string) string {
	return "paddock-ses-" + sessionID
}

// Resources is the rendered isolation set for one session.
type Resources struct {
	Namespace     *corev1.Namespace
	Pod           *corev1.Pod
	NetworkPolicy *netv1.NetworkPolicy
	ResourceQuota *corev1.ResourceQuota
}

// Render builds the isolation set without touching a cluster, so it can be
// unit-tested and dry-run.
func Render(spec Spec) (Resources, error) {
	if spec.SessionID == "" || spec.AgentImage == "" {
		return Resources{}, fmt.Errorf("sandbox spec requires SessionID and AgentImage")
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
	if spec.WorkspaceSizeLimit == "" {
		spec.WorkspaceSizeLimit = "2Gi"
	}
	workspaceLimit := resource.MustParse(spec.WorkspaceSizeLimit)
	ns := NamespaceName(spec.SessionID)
	labels := map[string]string{
		labelSession:   spec.SessionID,
		labelManagedBy: "paddock",
	}
	falseVal := false
	trueVal := true

	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: labels},
	}

	fsGroup := int64(10001)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			// The default token would let the agent talk to the k8s API.
			AutomountServiceAccountToken: &falseVal,
			RestartPolicy:                corev1.RestartPolicyNever,
			EnableServiceLinks:           &falseVal,
			// fsGroup makes the workspace emptyDir writable for the
			// non-root agent uid (emptyDir mounts root:root otherwise).
			SecurityContext: &corev1.PodSecurityContext{FSGroup: &fsGroup},
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
		ObjectMeta: metav1.ObjectMeta{Name: "egress-gateway-only", Namespace: ns, Labels: labels},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // every pod in the session namespace
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

	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "session-quota", Namespace: ns, Labels: labels},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourcePods:          resource.MustParse("2"),
				corev1.ResourceLimitsCPU:     resource.MustParse(spec.CPULimit),
				corev1.ResourceLimitsMemory:  resource.MustParse(spec.MemLimit),
				corev1.ResourceServices:      resource.MustParse("0"),
				corev1.ResourceSecrets:       resource.MustParse("0"),
			},
		},
	}

	return Resources{Namespace: namespace, Pod: pod, NetworkPolicy: netpol, ResourceQuota: quota}, nil
}

// Provisioner creates and destroys sandboxes. The server depends on this
// interface so it can run without a cluster (local dev, tests).
type Provisioner interface {
	Create(ctx context.Context, spec Spec) error
	Delete(ctx context.Context, sessionID string) error
}

// Noop is used when no kubeconfig is configured: sessions exist in the
// control plane only. Useful for local development of the API surface.
type Noop struct{}

func (Noop) Create(context.Context, Spec) error  { return nil }
func (Noop) Delete(context.Context, string) error { return nil }

// K8s provisions sandboxes on a real cluster.
type K8s struct {
	Client kubernetes.Interface
}

func (k *K8s) Create(ctx context.Context, spec Spec) error {
	res, err := Render(spec)
	if err != nil {
		return err
	}
	if _, err := k.Client.CoreV1().Namespaces().Create(ctx, res.Namespace, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create namespace: %w", err)
	}
	ns := res.Namespace.Name
	if _, err := k.Client.CoreV1().ResourceQuotas(ns).Create(ctx, res.ResourceQuota, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create quota: %w", err)
	}
	if _, err := k.Client.NetworkingV1().NetworkPolicies(ns).Create(ctx, res.NetworkPolicy, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create networkpolicy: %w", err)
	}
	// The pod goes last: it must never exist without its NetworkPolicy.
	if _, err := k.Client.CoreV1().Pods(ns).Create(ctx, res.Pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create pod: %w", err)
	}
	return nil
}

func (k *K8s) Delete(ctx context.Context, sessionID string) error {
	err := k.Client.CoreV1().Namespaces().Delete(ctx, NamespaceName(sessionID), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
