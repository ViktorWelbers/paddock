// paddock-gateway is the data plane: the model-API metering proxy and the
// server-side MCP mux. Sandboxes can reach this process and nothing else.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/viktorwelbers/paddock/internal/api"
	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/budget"
	"github.com/viktorwelbers/paddock/internal/gateway"
	"github.com/viktorwelbers/paddock/internal/mcpgw"
	"github.com/viktorwelbers/paddock/internal/policy"
)

func main() {
	addr := flag.String("addr", ":8081", "listen address")
	dbPath := flag.String("db", "paddock.db", "SQLite database path (shared with the server)")
	upstream := flag.String("upstream-anthropic", "https://api.anthropic.com", "Anthropic API upstream")
	upstreamOpenAI := flag.String("upstream-openai", "", "OpenAI-compatible upstream, e.g. https://vllm.example.com (empty = disabled)")
	upstreamOpenAICA := flag.String("upstream-openai-ca", "", "PEM file with an extra CA to trust for the OpenAI upstream (private CAs)")
	policiesDir := flag.String("policies", "policies", "directory of .rego policies")
	mcpRegistry := flag.String("mcp-registry", "", "JSON file of allowlisted MCP servers (empty = MCP disabled)")
	flag.Parse()

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		log.Fatal("ANTHROPIC_API_KEY must be set on the gateway (sandboxes never see it)")
	}

	// WAL + busy_timeout: the server writes from another process, and the
	// egress proxy appends audit rows from many goroutines — without these,
	// concurrent writers surface as SQLITE_BUSY and dropped audit events.
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
	upstreamURL, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("bad upstream URL: %v", err)
	}

	engine, err := policy.NewEngine(context.Background(), *policiesDir)
	if err != nil {
		log.Fatalf("load policies: %v", err)
	}

	backends := gateway.Backends{Sessions: sessions, Ledger: ledger, Audit: auditStore}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("/anthropic/", &gateway.AnthropicProxy{
		Backends: backends,
		Upstream: upstreamURL,
		APIKey:   apiKey,
	})

	if *upstreamOpenAI != "" {
		openaiURL, err := url.Parse(*upstreamOpenAI)
		if err != nil {
			log.Fatalf("bad OpenAI upstream URL: %v", err)
		}
		transport, err := transportWithCA(*upstreamOpenAICA)
		if err != nil {
			log.Fatalf("OpenAI upstream CA: %v", err)
		}
		mux.Handle("/openai/", &gateway.OpenAIProxy{
			Backends:  backends,
			Upstream:  openaiURL,
			APIKey:    os.Getenv("OPENAI_API_KEY"), // optional: self-hosted upstreams are often keyless
			Transport: transport,
		})
		log.Printf("OpenAI-compatible proxy enabled (upstream %s)", *upstreamOpenAI)
	}

	if *mcpRegistry != "" {
		registry, err := mcpgw.LoadRegistry(*mcpRegistry)
		if err != nil {
			log.Fatalf("load MCP registry: %v", err)
		}
		mux.Handle("/mcp/", &mcpgw.Mux{
			Registry: registry,
			Broker:   mcpgw.EnvBroker{},
			Policy:   engine.Evaluate,
			Audit:    auditStore,
			SessionFromRequest: func(r *http.Request) (string, string, error) {
				sess, err := sessions.ByToken(r.Header.Get("x-paddock-session"))
				if err != nil {
					return "", "", err
				}
				return sess.ID, sess.User, nil
			},
		})
		log.Printf("MCP mux enabled with %d allowlisted servers", len(registry.Servers))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		log.Printf("paddock-gateway listening on %s (upstream %s)", *addr, *upstream)
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

// transportWithCA returns a transport that additionally trusts the CA in
// pemPath (for upstreams behind a private CA), or nil (the default
// transport) when pemPath is empty.
func transportWithCA(pemPath string) (http.RoundTripper, error) {
	if pemPath == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(pemPath)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in %s", pemPath)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	return transport, nil
}
