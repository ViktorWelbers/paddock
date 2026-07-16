// paddock-server is the control plane: session CRUD, budget ledger, audit
// store, sandbox provisioning.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	_ "modernc.org/sqlite"

	"github.com/viktorwelbers/paddock/internal/api"
	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/budget"
	"github.com/viktorwelbers/paddock/internal/sandbox"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "paddock.db", "SQLite database path (shared with the gateway)")
	agentImage := flag.String("agent-image", "ghcr.io/paddock/agent-claude:latest", "fallback image spawned for sessions")
	agentImages := flag.String("agent-images", "", `per-agent images, e.g. "claude=reg/agent-claude:v1,pi=reg/agent-pi:v1"`)
	gatewayURL := flag.String("gateway-url", "http://paddock-gateway.paddock.svc:8081/anthropic", "ANTHROPIC_BASE_URL value inside sandboxes")
	openaiURL := flag.String("openai-gateway-url", "http://paddock-gateway.paddock.svc:8081/openai/v1", "gateway base URL for openai-completions agents (pi)")
	openaiModel := flag.String("openai-model", "", "model id served by the gateway's OpenAI upstream (required to run the pi agent)")
	egressProxyURL := flag.String("egress-proxy-url", "", "gateway CONNECT proxy URL injected as HTTP(S)_PROXY into sandboxes (empty = sandboxes get no egress)")
	workspaceSize := flag.String("workspace-size-limit", "2Gi", "size limit of the per-session /workspace volume")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path; empty = in-cluster config if available, else no-op provisioner")
	namespace := flag.String("sandbox-namespace", "", "namespace sandboxes run in (empty = this pod's own, which is what the chart's Role grants; only set this if you bound the provisioner Role elsewhere)")
	seedBudgetUSD := flag.Float64("seed-budget-usd", 25, "create a 'default' budget with this limit if none exists (dev convenience, 0 disables)")
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

	ns := sandboxNamespace(*namespace)
	h := &api.Handler{
		Sessions:    sessions,
		Ledger:      ledger,
		Audit:       auditStore,
		Provisioner: newProvisioner(*kubeconfig, ns),
		Config: api.Config{
			Namespace:      ns,
			AgentImage:     *agentImage,
			AgentImages:    parseAgentImages(*agentImages),
			GatewayURL:     *gatewayURL,
			OpenAIURL:      *openaiURL,
			OpenAIModel:    *openaiModel,
			EgressProxyURL: *egressProxyURL,
			WorkspaceSize:  *workspaceSize,
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: *addr, Handler: h.Routes()}
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
	return &sandbox.K8s{Client: client, Namespace: namespace}
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
