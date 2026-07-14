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
	return env
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			// The default token would let the agent talk to the k8s API.
			AutomountServiceAccountToken: &falseVal,
			RestartPolicy:                corev1.RestartPolicyNever,
			EnableServiceLinks:           &falseVal,
			Containers: []corev1.Container{{
				Name:  "agent",
				Image: spec.AgentImage,
				// The image's entrypoint holds the pod (tini + sleep);
				// `paddock attach` execs the agent with a TTY. Stdin/TTY stay
				// enabled so `kubectl attach` works as a fallback.
				Stdin:      true,
				TTY:        true,
				WorkingDir: "/workspace",
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
					// Only the Paddock gateway.
					To: []netv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{gatewayComponentLabel: gatewayComponentValue},
						},
					}},
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
