package sandbox

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func testSpec() Spec {
	return Spec{
		SessionID:    "abc123",
		User:         "viktor",
		AgentImage:   "ghcr.io/paddock/agent-claude:latest",
		GatewayURL:   "http://paddock-gateway.paddock.svc:8081/anthropic",
		SessionToken: "pdk_test",
	}
}

func TestRenderIsolationInvariants(t *testing.T) {
	res, err := Render(testSpec())
	if err != nil {
		t.Fatal(err)
	}

	if res.Namespace.Name != "paddock-ses-abc123" {
		t.Errorf("namespace = %q", res.Namespace.Name)
	}

	pod := res.Pod
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("service account token must not be mounted into sandboxes")
	}
	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("sandbox container must run as non-root")
	}
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("privilege escalation must be disabled")
	}

	agent := pod.Spec.Containers[0]
	if !agent.Stdin || !agent.TTY {
		t.Error("sandbox container must have Stdin and TTY for interactive attach")
	}
	if agent.WorkingDir != "/workspace" {
		t.Errorf("working dir = %q, want /workspace", agent.WorkingDir)
	}

	var baseURL, apiKey string
	for _, env := range pod.Spec.Containers[0].Env {
		switch env.Name {
		case "ANTHROPIC_BASE_URL":
			baseURL = env.Value
		case "ANTHROPIC_API_KEY":
			apiKey = env.Value
		}
	}
	if baseURL != testSpec().GatewayURL {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the gateway", baseURL)
	}
	if apiKey != "pdk_test" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the session token (never a real key)", apiKey)
	}

	np := res.NetworkPolicy
	if len(np.Spec.Ingress) != 0 {
		t.Error("sandboxes must not accept ingress")
	}
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("egress rules = %d, want 2 (gateway + DNS)", len(np.Spec.Egress))
	}
	gw := np.Spec.Egress[0].To[0].PodSelector.MatchLabels["paddock.dev/component"]
	if gw != "gateway" {
		t.Errorf("egress peer selector = %q, want the gateway component", gw)
	}

	secrets := res.ResourceQuota.Spec.Hard[corev1.ResourceSecrets]
	if secrets.Value() != 0 {
		t.Error("session namespaces must not allow secrets")
	}
}

func TestRenderRejectsIncompleteSpec(t *testing.T) {
	spec := testSpec()
	spec.GatewayURL = ""
	if _, err := Render(spec); err == nil {
		t.Fatal("expected an error for a spec without a gateway URL")
	}
}

func TestRenderPiAgentEnv(t *testing.T) {
	spec := testSpec()
	spec.Agent = "pi"
	spec.AgentImage = "ghcr.io/paddock/agent-pi:latest"
	spec.OpenAIURL = "http://paddock-gateway.paddock.svc:8081/openai/v1"
	spec.Model = "some/model"

	res, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range res.Pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["PADDOCK_OPENAI_BASE_URL"] != spec.OpenAIURL {
		t.Errorf("PADDOCK_OPENAI_BASE_URL = %q, want the gateway's openai path", env["PADDOCK_OPENAI_BASE_URL"])
	}
	if env["PADDOCK_MODEL"] != "some/model" {
		t.Errorf("PADDOCK_MODEL = %q", env["PADDOCK_MODEL"])
	}
	if env["PI_API_KEY"] != "pdk_test" {
		t.Errorf("PI_API_KEY = %q, want the session token (never a real key)", env["PI_API_KEY"])
	}
	if _, leaked := env["ANTHROPIC_API_KEY"]; leaked {
		t.Error("pi sandboxes must not carry Anthropic env")
	}
}

func TestRenderPiRequiresOpenAIConfig(t *testing.T) {
	spec := testSpec()
	spec.Agent = "pi"
	if _, err := Render(spec); err == nil {
		t.Fatal("expected an error for a pi spec without OpenAIURL/Model")
	}
}
