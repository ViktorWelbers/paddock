package sandbox

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func testSpec() Spec {
	return Spec{
		SessionID:    "abc123",
		Namespace:    "paddock",
		User:         "viktor",
		AgentImage:   "ghcr.io/paddock/agent-claude:latest",
		GatewayURL:   "http://paddock-gateway.paddock.svc:8081/anthropic",
		SessionToken: "pdk_test",
	}
}

func TestRenderReservesLessThanItLimits(t *testing.T) {
	res, err := Render(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	r := res.Pod.Spec.Containers[0].Resources
	if r.Requests.Cpu().IsZero() || r.Requests.Memory().IsZero() {
		t.Fatal("requests must be set explicitly, or kube copies the limits and a sandbox reserves its whole ceiling")
	}
	// Bursty and mostly idle: reserving the whole limit strands cores and
	// leaves a sandbox unschedulable on a busy node while it sits mostly free.
	if r.Requests.Cpu().Cmp(*r.Limits.Cpu()) != -1 {
		t.Errorf("cpu request %s must be below limit %s", r.Requests.Cpu(), r.Limits.Cpu())
	}
	if r.Requests.Memory().Cmp(*r.Limits.Memory()) != -1 {
		t.Errorf("memory request %s must be below limit %s", r.Requests.Memory(), r.Limits.Memory())
	}
}

func TestRenderIsolationInvariants(t *testing.T) {
	res, err := Render(testSpec())
	if err != nil {
		t.Fatal(err)
	}

	pod := res.Pod
	if pod.Namespace != "paddock" || pod.Name != "paddock-ses-abc123" {
		t.Errorf("pod = %s/%s, want paddock/paddock-ses-abc123", pod.Namespace, pod.Name)
	}

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

}

// The sandbox shares a namespace with the control plane, so a NetworkPolicy
// that selected broadly would firewall the server and gateway themselves.
func TestRenderNetpolSelectsOnlyItsOwnPod(t *testing.T) {
	res, err := Render(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	sel := res.NetworkPolicy.Spec.PodSelector
	if len(sel.MatchLabels) == 0 {
		t.Fatal("netpol pod selector is empty: it would select every pod in the namespace, control plane included")
	}
	if sel.MatchLabels["paddock.dev/session"] != "abc123" {
		t.Errorf("netpol selects %v, want only session abc123's pod", sel.MatchLabels)
	}
	if res.Pod.Labels["paddock.dev/session"] != "abc123" {
		t.Error("the pod must carry the label its NetworkPolicy selects, or it gets no policy at all")
	}
	if res.NetworkPolicy.Name != res.Pod.Name {
		t.Errorf("netpol %q and pod %q must both be session-scoped to coexist in a shared namespace", res.NetworkPolicy.Name, res.Pod.Name)
	}
}

// Sandboxes sit next to the control plane, whose Services select on
// app.kubernetes.io/name. A sandbox carrying those labels would quietly
// become an endpoint of the gateway Service.
func TestRenderPodIsNotAControlPlaneServiceEndpoint(t *testing.T) {
	res, err := Render(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"app.kubernetes.io/name", "app.kubernetes.io/instance", "paddock.dev/component"} {
		if v, ok := res.Pod.Labels[label]; ok {
			t.Errorf("sandbox pod carries %s=%q: the control plane's Services select on it and would route to sandboxes", label, v)
		}
	}
}

// A cluster that runs other people's agents should have the sandbox
// namespace on Pod Security Admission's "restricted" profile. If the pod
// misses any of these, the API server refuses to create it and every
// `paddock run` fails at admission — so the controls are asserted here
// rather than discovered on someone else's cluster.
func TestRenderMeetsPodSecurityRestricted(t *testing.T) {
	res, err := Render(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	pod := res.Pod.Spec

	if pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil ||
		pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("restricted requires seccompProfile RuntimeDefault; without it admission rejects the pod")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("restricted requires runAsNonRoot at the pod level")
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC {
		t.Error("restricted forbids host namespaces")
	}
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			t.Errorf("volume %q is a hostPath, which restricted forbids", v.Name)
		}
	}
	for _, c := range pod.Containers {
		sc := c.SecurityContext
		if sc == nil {
			t.Fatalf("container %q has no securityContext", c.Name)
		}
		if sc.Privileged != nil && *sc.Privileged {
			t.Errorf("container %q is privileged", c.Name)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("container %q must set allowPrivilegeEscalation: false", c.Name)
		}
		if sc.Capabilities == nil || !slices.Contains(sc.Capabilities.Drop, corev1.Capability("ALL")) {
			t.Errorf("container %q must drop ALL capabilities", c.Name)
		}
	}
}

func TestRenderWorkspaceVolume(t *testing.T) {
	res, err := Render(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	pod := res.Pod
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.FSGroup == nil || *pod.Spec.SecurityContext.FSGroup != 10001 {
		t.Error("pod needs fsGroup 10001 so the non-root agent can write the workspace volume")
	}
	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].EmptyDir == nil {
		t.Fatal("workspace must be an emptyDir volume")
	}
	if limit := pod.Spec.Volumes[0].EmptyDir.SizeLimit; limit == nil || limit.String() != "2Gi" {
		t.Errorf("workspace size limit = %v, want the 2Gi default", limit)
	}
	mounts := pod.Spec.Containers[0].VolumeMounts
	if len(mounts) != 1 || mounts[0].MountPath != "/workspace" {
		t.Errorf("workspace mount = %+v, want /workspace", mounts)
	}
}

func TestRenderEgressProxyEnv(t *testing.T) {
	spec := testSpec()
	spec.EgressProxyURL = "http://paddock-gateway.paddock.svc:8082"
	res, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, e := range res.Pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	want := "http://paddock:pdk_test@paddock-gateway.paddock.svc:8082"
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if env[name] != want {
			t.Errorf("%s = %q, want session-token proxy URL", name, env[name])
		}
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		if env[name] != "localhost,127.0.0.1,paddock-gateway.paddock.svc" {
			t.Errorf("%s = %q: LLM traffic must bypass the CONNECT proxy", name, env[name])
		}
	}
}

func TestRenderNoProxyEnvWithoutEgressProxy(t *testing.T) {
	res, err := Render(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Pod.Spec.Containers[0].Env {
		if e.Name == "HTTP_PROXY" || e.Name == "HTTPS_PROXY" {
			t.Errorf("proxy env %s rendered without an egress proxy configured", e.Name)
		}
	}
}

func TestRenderNetpolPortScoping(t *testing.T) {
	spec := testSpec()
	spec.EgressProxyURL = "http://paddock-gateway.paddock.svc:8082"
	res, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	ports := res.NetworkPolicy.Spec.Egress[0].Ports
	if len(ports) != 2 {
		t.Fatalf("gateway egress ports = %v, want exactly [8081 8082] — an open port list would expose the control-plane API", ports)
	}
	got := map[int32]bool{}
	for _, p := range ports {
		got[p.Port.IntVal] = true
	}
	if !got[8081] || !got[8082] {
		t.Errorf("gateway egress ports = %v, want 8081 and 8082", ports)
	}
}

func TestRenderPlacement(t *testing.T) {
	spec := testSpec()
	spec.Placement = Placement{
		NodeSelector: map[string]string{"paddock.dev/agents": "true"},
		Tolerations: []corev1.Toleration{{
			Key: "paddock.dev/agents", Operator: corev1.TolerationOpExists,
			Effect: corev1.TaintEffectNoSchedule,
		}},
		RuntimeClassName:  "gvisor",
		PriorityClassName: "paddock-sandbox",
	}
	res, err := Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	pod := res.Pod.Spec
	if pod.NodeSelector["paddock.dev/agents"] != "true" {
		t.Errorf("node selector = %v, want the agent pool", pod.NodeSelector)
	}
	if len(pod.Tolerations) != 1 || pod.Tolerations[0].Key != "paddock.dev/agents" {
		t.Errorf("tolerations = %v: without them the agent pool's taint repels the sandbox it was created for", pod.Tolerations)
	}
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "gvisor" {
		t.Errorf("runtimeClassName = %v, want gvisor", pod.RuntimeClassName)
	}
	if pod.PriorityClassName != "paddock-sandbox" {
		t.Errorf("priorityClassName = %q", pod.PriorityClassName)
	}
}

// The zero Placement must leave the fields absent rather than set to empty
// values: a runtimeClassName of "" is a class named "", and the API server
// rejects the pod for it.
func TestRenderWithoutPlacementLeavesSchedulingToTheCluster(t *testing.T) {
	res, err := Render(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	pod := res.Pod.Spec
	if pod.RuntimeClassName != nil {
		t.Errorf("runtimeClassName = %v, want unset", *pod.RuntimeClassName)
	}
	if pod.NodeSelector != nil || pod.Tolerations != nil || pod.PriorityClassName != "" {
		t.Errorf("unconfigured placement leaked into the pod: selector=%v tolerations=%v priority=%q",
			pod.NodeSelector, pod.Tolerations, pod.PriorityClassName)
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
