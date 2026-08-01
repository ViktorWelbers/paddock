// paddock-server is the control plane: session CRUD, budget ledger, audit
// store, sandbox provisioning.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	_ "modernc.org/sqlite"

	"github.com/viktorwelbers/paddock/internal/api"
	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/auth"
	"github.com/viktorwelbers/paddock/internal/budget"
	"github.com/viktorwelbers/paddock/internal/sandbox"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "paddock.db", "SQLite database path (shared with the gateway)")
	agentImage := flag.String("agent-image", "ghcr.io/paddock/agent-claude:latest", "image for the default agent ("+sandbox.DefaultAgent+")")
	agentImages := flag.String("agent-images", "", `per-agent images, e.g. "claude=reg/agent-claude:v1,pi=reg/agent-pi:v1"`)
	gatewayURL := flag.String("gateway-url", "http://paddock-gateway.paddock.svc:8081/anthropic", "ANTHROPIC_BASE_URL value inside sandboxes")
	openaiURL := flag.String("openai-gateway-url", "http://paddock-gateway.paddock.svc:8081/openai/v1", "gateway base URL for openai-completions agents (pi)")
	openaiModel := flag.String("openai-model", "", "model id served by the gateway's OpenAI upstream (required to run the pi agent)")
	egressProxyURL := flag.String("egress-proxy-url", "", "gateway CONNECT proxy URL injected as HTTP(S)_PROXY into sandboxes (empty = sandboxes get no egress)")
	claudeOAuthSecret := flag.String("claude-oauth-secret", "", "name of a Secret holding CLAUDE_CODE_OAUTH_TOKEN; set it to run the claude agent on a Claude subscription (direct to api.anthropic.com through the egress proxy) instead of the gateway's metered API-key path")
	workspaceSize := flag.String("workspace-size-limit", "2Gi", "size limit of the per-session /workspace volume")
	nodeSelector := flag.String("sandbox-node-selector", "", `JSON object pinning sandboxes to nodes, e.g. {"paddock.dev/agents":"true"}`)
	tolerations := flag.String("sandbox-tolerations", "", `JSON array of tolerations, e.g. [{"key":"paddock.dev/agents","operator":"Exists","effect":"NoSchedule"}]`)
	runtimeClass := flag.String("sandbox-runtime-class", "", "runtimeClassName for sandbox pods (e.g. gvisor); empty = the cluster default")
	priorityClass := flag.String("sandbox-priority-class", "", "priorityClassName for sandbox pods; empty = the cluster default")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path; empty = in-cluster config if available, else no-op provisioner")
	namespace := flag.String("sandbox-namespace", "", "namespace sandboxes run in (empty = this pod's own, which is what the chart's Role grants; only set this if you bound the provisioner Role elsewhere)")
	seedBudgetUSD := flag.Float64("seed-budget-usd", 25, "create a 'default' budget with this limit if none exists (dev convenience, 0 disables)")
	maxSessionAge := flag.Duration("max-session-age", 0, "tear down sessions older than this and invalidate their tokens (0 = never; a sandbox then lives until `paddock rm`)")
	maxSessionIdle := flag.Duration("max-session-idle", 0, "tear down sessions with no activity for this long (0 = never; complements --max-session-age for the local-harness mode where a forgotten sandbox should self-reap)")
	maxSessionsPerUser := flag.Int("max-sessions-per-user", 0, "cap concurrent running sessions per user (0 = unlimited); a rejected create gets 429")
	maxSessionsTotal := flag.Int("max-sessions-total", 0, "cap concurrent running sessions across the whole server (0 = unlimited); a rejected create gets 429")
	authTokens := flag.String("auth-tokens", "", "JSON file of bearer tokens identifying API callers")
	authDisabled := flag.Bool("auth-disabled", false, "serve the API with no authentication at all — every caller owns every session (laptops and CI only)")
	flag.Parse()

	// WAL + busy_timeout: the gateway writes from another process sharing
	// this file; without these, concurrent writers surface as SQLITE_BUSY.
	db, err := sql.Open("sqlite", *dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}

	ledger, err := budget.NewLedger(db, nil)
	if err != nil {
		log.Fatal(err)
	}
	auditStore, err := audit.NewStore(db)
	if err != nil {
		log.Fatal(err)
	}
	sessions, err := api.NewStore(db)
	if err != nil {
		log.Fatal(err)
	}

	if *seedBudgetUSD > 0 {
		if _, err := ledger.Get("default"); err != nil {
			if err := ledger.Create(budget.Budget{ID: "default", Name: "default", LimitUSD: *seedBudgetUSD}); err != nil {
				log.Fatalf("seed default budget: %v", err)
			}
			log.Printf("seeded 'default' budget with %.2f USD", *seedBudgetUSD)
		}
	}

	authenticator := newAuthenticator(*authTokens, *authDisabled)
	ns := sandboxNamespace(*namespace)
	provisioner := newProvisioner(*kubeconfig, ns)
	// Only a real cluster can stream workspaces; the no-op provisioner leaves
	// Exec nil and the workspace endpoints say so.
	execer, _ := provisioner.(sandbox.Execer)
	h := &api.Handler{
		Sessions:    sessions,
		Ledger:      ledger,
		Audit:       auditStore,
		Provisioner: provisioner,
		Exec:        execer,
		Auth:        authenticator,
		Config: api.Config{
			Namespace:          ns,
			AgentImages:        agentImageMap(*agentImage, *agentImages),
			GatewayURL:         *gatewayURL,
			OpenAIURL:          *openaiURL,
			OpenAIModel:        *openaiModel,
			EgressProxyURL:     *egressProxyURL,
			ClaudeOAuthSecret:  *claudeOAuthSecret,
			WorkspaceSize:      *workspaceSize,
			MaxSessionsPerUser: *maxSessionsPerUser,
			MaxSessionsTotal:   *maxSessionsTotal,
			Placement: sandbox.Placement{
				NodeSelector:      parseJSONFlag[map[string]string]("sandbox-node-selector", *nodeSelector),
				Tolerations:       parseJSONFlag[[]corev1.Toleration]("sandbox-tolerations", *tolerations),
				RuntimeClassName:  *runtimeClass,
				PriorityClassName: *priorityClass,
			},
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Before serving, not after: once requests are in flight, a session with
	// no pod yet is indistinguishable from a session whose pod is gone.
	// A cluster we cannot read is not a reason to refuse to start — the
	// drift is already there and staying down does not fix it.
	reconcileCtx, cancelReconcile := context.WithTimeout(ctx, 2*time.Minute)
	if err := h.Reconcile(reconcileCtx); err != nil {
		log.Printf("reconcile sandboxes: %v", err)
	}
	cancelReconcile()

	// Enforce the session TTLs for as long as the server runs — absolute age
	// and/or idleness. Tied to ctx, so it stops with the rest on shutdown.
	if *maxSessionAge > 0 || *maxSessionIdle > 0 {
		go reapLoop(ctx, h, *maxSessionAge, *maxSessionIdle)
	}

	srv := &http.Server{Addr: *addr, Handler: h.Handler()}
	go func() {
		log.Printf("paddock-server listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if err := db.Close(); err != nil {
		log.Printf("close db: %v", err)
	}
}

// reapLoop enforces the session TTLs (absolute age and/or idleness) until ctx
// is cancelled. The sweep runs often enough that a session dies close to its
// deadline, but not so often it polls the store for nothing — a tenth of the
// smaller active timeout, clamped to a sane band. A blocked provisioner cannot
// wedge the loop: each sweep is bounded.
func reapLoop(ctx context.Context, h *api.Handler, maxAge, maxIdle time.Duration) {
	// Pace off whichever timeout is active and shorter, so idle reaping is not
	// starved by a long absolute cap.
	pace := maxAge
	if maxIdle > 0 && (pace <= 0 || maxIdle < pace) {
		pace = maxIdle
	}
	interval := pace / 10
	if interval < time.Minute {
		interval = time.Minute
	}
	if interval > 15*time.Minute {
		interval = 15 * time.Minute
	}
	log.Printf("session reaper: max-age=%s max-idle=%s (sweeping every %s)", maxAge, maxIdle, interval)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweepCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			if _, err := h.ReapExpired(sweepCtx, maxAge); err != nil {
				log.Printf("reap expired sessions: %v", err)
			}
			if _, err := h.ReapIdle(sweepCtx, maxIdle); err != nil {
				log.Printf("reap idle sessions: %v", err)
			}
			cancel()
		}
	}
}

// newAuthenticator picks how callers are identified. There is no default:
// paddock's whole claim is that it can say who did what, and a server that
// quietly accepts anyone cannot. Running without authentication stays
// possible — a laptop has no IdP and needs none — but it has to be asked
// for by name, and the server says so on every start.
func newAuthenticator(tokenFile string, disabled bool) auth.Authenticator {
	switch {
	case tokenFile != "" && disabled:
		log.Fatal("--auth-tokens and --auth-disabled contradict each other; pick one")
	case tokenFile != "":
		tokens, err := auth.LoadTokens(tokenFile)
		if err != nil {
			// Fatal: an operator who asked for auth and got a typo'd path
			// must not be handed an open server as the consolation prize.
			log.Fatalf("load auth tokens: %v", err)
		}
		log.Printf("API authentication: %s", tokens.Describe())
		return tokens
	case disabled:
		a := auth.Anonymous{As: "anonymous"}
		log.Printf("API authentication: %s", a.Describe())
		return a
	}
	log.Fatal("no API authentication configured: pass --auth-tokens <file>, " +
		"or --auth-disabled to serve the API to anyone who can reach it")
	return nil
}

// parseJSONFlag decodes a flag whose value is a JSON document — node
// selectors and tolerations are structured, and inventing a mini-language
// for them would only mean re-learning Kubernetes' own. A bad value is
// fatal: silently scheduling agents anywhere is exactly what the operator
// set the flag to prevent.
func parseJSONFlag[T any](name, raw string) T {
	var v T
	if raw == "" {
		return v
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		log.Fatalf("--%s: %v", name, err)
	}
	return v
}

// agentImageMap resolves the agent → image table. --agent-image names the
// image for the default agent; --agent-images registers the rest. An agent
// that appears in neither is unsupported on this server, which is the whole
// point of building the map up front: `paddock run typo` should be a 400,
// not a Claude sandbox wearing the wrong name.
func agentImageMap(defaultImage, pairs string) map[string]string {
	m := parseAgentImages(pairs)
	if m == nil {
		m = map[string]string{}
	}
	if _, ok := m[sandbox.DefaultAgent]; !ok && defaultImage != "" {
		m[sandbox.DefaultAgent] = defaultImage
	}
	return m
}

// parseAgentImages parses "agent=image,agent=image" into a map.
func parseAgentImages(s string) map[string]string {
	if s == "" {
		return nil
	}
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		agent, image, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || agent == "" || image == "" {
			log.Fatalf("bad --agent-images entry %q, want agent=image", pair)
		}
		m[agent] = image
	}
	return m
}

// newProvisioner picks the sandbox provisioner: an explicit kubeconfig wins,
// then in-cluster config when running inside Kubernetes, else no-op.
// Sandboxes are created in namespace, which is the server's own.
func newProvisioner(kubeconfig, namespace string) sandbox.Provisioner {
	var cfg *rest.Config
	var err error
	switch {
	case kubeconfig != "":
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Fatalf("load kubeconfig: %v", err)
		}
		log.Printf("provisioning sandboxes via kubeconfig %s", kubeconfig)
	case os.Getenv("KUBERNETES_SERVICE_HOST") != "":
		cfg, err = rest.InClusterConfig()
		if err != nil {
			log.Fatalf("in-cluster config: %v", err)
		}
		log.Print("provisioning sandboxes via in-cluster config")
	default:
		log.Print("no kubeconfig and not in-cluster: running with the no-op provisioner (control plane only)")
		return sandbox.Noop{}
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("k8s client: %v", err)
	}
	log.Printf("sandboxes will run in namespace %q", namespace)
	return &sandbox.K8s{Client: client, Namespace: namespace, RESTConfig: cfg}
}

// sandboxNamespace resolves where sandboxes run: the flag wins, otherwise the
// namespace this pod is in (the chart projects it via the downward API), and
// finally "paddock" for out-of-cluster development.
func sandboxNamespace(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if raw, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(raw)); ns != "" {
			return ns
		}
	}
	return "paddock"
}
