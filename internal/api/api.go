// Package api is the control-plane surface: session CRUD backed by SQLite,
// wired to the sandbox provisioner, budget ledger, and audit store.
package api

import (
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/auth"
	"github.com/viktorwelbers/paddock/internal/budget"
	"github.com/viktorwelbers/paddock/internal/sandbox"
)

// Session statuses. A session is running until something ends it: the user
// (deleted), the sandbox going away underneath it (failed), or it outliving
// its TTL (expired). Every terminal status drops out of ByToken, so ending a
// session — however it ends — invalidates its sandbox token at once.
const (
	statusRunning = "running"
	statusDeleted = "deleted"
	statusFailed  = "failed"
	statusExpired = "expired"
)

type Session struct {
	ID        string    `json:"id"`
	User      string    `json:"user"`
	Agent     string    `json:"agent"` // "claude", "opencode", ...
	BudgetID  string    `json:"budget_id"`
	Token     string    `json:"token,omitempty"` // returned once on create
	Status    string    `json:"status"`          // running | deleted | failed | expired
	CreatedAt time.Time `json:"created_at"`

	// Where the sandbox runs. Derived from server config rather than stored,
	// and reported so clients never have to guess the cluster layout.
	Namespace string `json:"namespace,omitempty"`
	Pod       string `json:"pod,omitempty"`
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) (*Store, error) {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id         TEXT PRIMARY KEY,
			user       TEXT NOT NULL,
			agent      TEXT NOT NULL,
			budget_id  TEXT NOT NULL,
			token      TEXT NOT NULL,
			status     TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_token ON sessions (token);`)
	if err != nil {
		return nil, fmt.Errorf("migrate sessions schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) insert(sess Session) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, user, agent, budget_id, token, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.User, sess.Agent, sess.BudgetID, sess.Token, sess.Status,
		sess.CreatedAt.Format(time.RFC3339Nano),
	)
	return err
}

func (s *Store) get(id string) (Session, error) {
	return s.scanOne(`SELECT id, user, agent, budget_id, status, created_at FROM sessions WHERE id = ?`, id)
}

// ByToken authenticates a sandbox session token; the gateway uses this on
// every proxied call.
func (s *Store) ByToken(token string) (Session, error) {
	return s.scanOne(`SELECT id, user, agent, budget_id, status, created_at FROM sessions WHERE token = ? AND status = 'running'`, token)
}

func (s *Store) scanOne(query, arg string) (Session, error) {
	var sess Session
	var created string
	err := s.db.QueryRow(query, arg).Scan(
		&sess.ID, &sess.User, &sess.Agent, &sess.BudgetID, &sess.Status, &created)
	if err != nil {
		return sess, err
	}
	sess.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return sess, err
}

func (s *Store) list() ([]Session, error) {
	rows, err := s.db.Query(`SELECT id, user, agent, budget_id, status, created_at FROM sessions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		var created string
		if err := rows.Scan(&sess.ID, &sess.User, &sess.Agent, &sess.BudgetID, &sess.Status, &created); err != nil {
			return nil, err
		}
		if sess.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) setStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE sessions SET status = ? WHERE id = ?`, status, id)
	return err
}

// Config carries the server's sandbox defaults.
type Config struct {
	AgentImages    map[string]string // agent name → image, e.g. {"claude": ..., "pi": ...}
	GatewayURL     string            // Anthropic-path gateway URL (ANTHROPIC_BASE_URL for claude)
	OpenAIURL      string            // OpenAI-path gateway URL for openai-completions agents
	OpenAIModel    string            // model id pinned for those agents (empty = not configured)
	EgressProxyURL string            // gateway CONNECT proxy for governed egress (empty = no egress)
	// ClaudeOAuthSecret names a Secret holding CLAUDE_CODE_OAUTH_TOKEN. Set it
	// to run the claude agent on a Claude subscription (direct to
	// api.anthropic.com through the egress proxy) instead of the gateway's
	// metered API-key path. Empty = gateway mode.
	ClaudeOAuthSecret string
	WorkspaceSize     string // /workspace emptyDir size limit (empty = sandbox default)
	Namespace         string // namespace the sandboxes run in (the server's own)
	// Placement pins sandboxes to the nodes the operator set aside for them.
	Placement sandbox.Placement
}

// locate stamps a session with where its sandbox lives, for clients that need
// to reach the pod directly (`paddock attach`).
func (c Config) locate(sess Session) Session {
	if sess.Status == statusRunning {
		sess.Namespace = c.Namespace
		sess.Pod = sandbox.ResourceName(sess.ID)
	}
	return sess
}

// imageFor picks the sandbox image for an agent; empty means unsupported.
// An agent the operator never configured has no image and no env contract,
// so it is rejected rather than quietly served the default one.
func (c Config) imageFor(agent string) string {
	return c.AgentImages[agent]
}

// Handler wires the HTTP surface.
type Handler struct {
	Sessions    *Store
	Ledger      *budget.Ledger
	Audit       *audit.Store
	Provisioner sandbox.Provisioner
	// Exec streams workspaces in and out of sandboxes. Nil (no cluster)
	// makes the workspace endpoints report 501 rather than fail obscurely.
	Exec sandbox.Execer
	// Auth establishes who is calling. Nil means every caller is anonymous
	// and omnipotent, which is only correct on a laptop — Routes refuses to
	// leave it implicit and cmd/paddock-server names the choice.
	Auth   auth.Authenticator
	Config Config
}

// publicPaths need no identity: a health check has none to give, and the
// dashboard is markup — its data arrives through the authenticated API.
var publicPaths = []string{"/healthz", "/readyz", "/"}

// Handler returns the fully wired HTTP handler: routes behind the
// authentication middleware.
func (h *Handler) Handler() http.Handler {
	a := h.Auth
	if a == nil {
		a = auth.Anonymous{}
	}
	// requestLogging sits inside auth so every logged request already carries
	// its subject; the access log is the operator's view, the audit store the
	// compliance one.
	return auth.Middleware(a, publicPaths, requestLogging(h.Routes()))
}

// caller is the authenticated identity for this request. Requests always
// pass through the middleware, so a missing identity is a wiring bug — fail
// closed rather than invent an anonymous one.
func caller(r *http.Request) (auth.Identity, bool) {
	return auth.FromContext(r.Context())
}

// mayAccess reports whether the caller owns this session (or is an admin),
// having already written the response if not.
//
// The refusal is a 404, not a 403: a 403 would confirm that a session id
// exists and belongs to someone else, and session ids are the only thing
// standing between a curious colleague and another team's workspace.
func (h *Handler) mayAccess(w http.ResponseWriter, r *http.Request, sess Session) bool {
	id, ok := caller(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return false
	}
	if sess.User == id.Subject || id.IsAdmin() {
		return true
	}
	http.NotFound(w, r)
	return false
}

// dashboardHTML is the read-only web dashboard (sessions, budgets, audit
// trail). Self-contained: no external assets, no build step.
//
//go:embed ui/index.html
var dashboardHTML []byte

func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(dashboardHTML)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /readyz", h.readyz)
	mux.HandleFunc("GET /metrics", h.metrics)
	mux.HandleFunc("POST /v1/sessions", h.createSession)
	mux.HandleFunc("GET /v1/sessions", h.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", h.getSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", h.deleteSession)
	mux.HandleFunc("GET /v1/sessions/{id}/events", h.sessionEvents)
	mux.HandleFunc("POST /v1/sessions/{id}/workspace", h.pushWorkspace)
	mux.HandleFunc("GET /v1/sessions/{id}/workspace", h.pullWorkspace)
	mux.HandleFunc("GET /v1/sessions/{id}/git-recipient", h.gitRecipient)
	mux.HandleFunc("POST /v1/sessions/{id}/git-credentials", h.injectGitCredentials)
	mux.HandleFunc("POST /v1/sessions/{id}/git-signing", h.configureGitSigning)
	mux.HandleFunc("GET /v1/budgets", h.listBudgets)
	mux.HandleFunc("GET /v1/budgets/{id}", h.getBudget)
	return mux
}

func randomID(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return hex.EncodeToString(b)
}

func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	id, ok := caller(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var req struct {
		// User is accepted and ignored: it is what older clients sent, and
		// honouring it would let anyone create a session — and spend a
		// budget — under someone else's name. The owner is the caller.
		User     string `json:"user"`
		Agent    string `json:"agent"`
		BudgetID string `json:"budget_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Agent == "" || req.BudgetID == "" {
		http.Error(w, "agent and budget_id are required", http.StatusBadRequest)
		return
	}
	image := h.Config.imageFor(req.Agent)
	if image == "" {
		http.Error(w, fmt.Sprintf("agent %q is not configured on this server", req.Agent), http.StatusBadRequest)
		return
	}
	if req.Agent == "pi" && (h.Config.OpenAIURL == "" || h.Config.OpenAIModel == "") {
		http.Error(w, `agent "pi" needs the gateway's OpenAI upstream configured (openai gateway URL + model)`, http.StatusBadRequest)
		return
	}
	// No headroom, no sandbox.
	remaining, err := h.Ledger.Remaining(req.BudgetID)
	if err != nil {
		http.Error(w, fmt.Sprintf("budget %q not found", req.BudgetID), http.StatusBadRequest)
		return
	}
	if remaining <= 0 {
		http.Error(w, "budget exhausted", http.StatusPaymentRequired)
		return
	}

	sess := Session{
		ID:        randomID(6),
		User:      id.Subject,
		Agent:     req.Agent,
		BudgetID:  req.BudgetID,
		Token:     "pdk_" + randomID(24),
		Status:    statusRunning,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.Sessions.insert(sess); err != nil {
		http.Error(w, "store session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	err = h.Provisioner.Create(r.Context(), sandbox.Spec{
		SessionID:          sess.ID,
		Namespace:          h.Config.Namespace,
		User:               sess.User,
		Agent:              sess.Agent,
		AgentImage:         image,
		GatewayURL:         h.Config.GatewayURL,
		OpenAIURL:          h.Config.OpenAIURL,
		Model:              h.Config.OpenAIModel,
		SessionToken:       sess.Token,
		ClaudeOAuthSecret:  h.Config.ClaudeOAuthSecret,
		EgressProxyURL:     h.Config.EgressProxyURL,
		WorkspaceSizeLimit: h.Config.WorkspaceSize,
		Placement:          h.Config.Placement,
	})
	if err != nil {
		_ = h.Sessions.setStatus(sess.ID, statusFailed)
		http.Error(w, "provision sandbox: "+err.Error(), http.StatusBadGateway)
		return
	}
	h.Audit.Record(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindSessionCreated,
		Payload: map[string]any{"agent": sess.Agent, "budget_id": sess.BudgetID},
	})
	writeJSON(w, http.StatusCreated, h.Config.locate(sess))
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	id, ok := caller(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	all, err := h.Sessions.list()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sessions := []Session{}
	for _, s := range all {
		// `paddock ls` is a developer's view of their own work; an admin
		// gets the whole cluster.
		if s.User != id.Subject && !id.IsAdmin() {
			continue
		}
		sessions = append(sessions, h.Config.locate(s))
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (h *Handler) getSession(w http.ResponseWriter, r *http.Request) {
	sess, err := h.Sessions.get(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.mayAccess(w, r, sess) {
		return
	}
	writeJSON(w, http.StatusOK, h.Config.locate(sess))
}

func (h *Handler) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := h.Sessions.get(id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.mayAccess(w, r, sess) {
		return
	}
	if err := h.Provisioner.Delete(r.Context(), id); err != nil {
		http.Error(w, "teardown sandbox: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := h.Sessions.setStatus(id, statusDeleted); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.Audit.Record(audit.Event{
		SessionID: id, Actor: sess.User, Kind: audit.KindSessionDeleted,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) sessionEvents(w http.ResponseWriter, r *http.Request) {
	// The events are the session's story — what it spent, where it
	// connected, what it was refused. Same owner check as the session.
	sess, err := h.Sessions.get(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.mayAccess(w, r, sess) {
		return
	}
	events, err := h.Audit.BySession(sess.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []audit.Event{}
	}
	writeJSON(w, http.StatusOK, events)
}

func (h *Handler) listBudgets(w http.ResponseWriter, _ *http.Request) {
	budgets, err := h.Ledger.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(budgets))
	for _, b := range budgets {
		out = append(out, map[string]any{
			"id": b.ID, "name": b.Name, "parent_id": b.ParentID,
			"limit_usd": b.LimitUSD, "spent_usd": b.SpentUSD,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) getBudget(w http.ResponseWriter, r *http.Request) {
	b, err := h.Ledger.Get(r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": b.ID, "name": b.Name, "parent_id": b.ParentID,
		"limit_usd": b.LimitUSD, "spent_usd": b.SpentUSD,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
