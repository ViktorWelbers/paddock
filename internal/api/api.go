// Package api is the control-plane surface: session CRUD backed by SQLite,
// wired to the sandbox provisioner, budget ledger, and audit store.
package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/viktorwelbers/paddock/internal/audit"
	"github.com/viktorwelbers/paddock/internal/budget"
	"github.com/viktorwelbers/paddock/internal/sandbox"
)

type Session struct {
	ID        string    `json:"id"`
	User      string    `json:"user"`
	Agent     string    `json:"agent"` // "claude", "opencode", ...
	BudgetID  string    `json:"budget_id"`
	Token     string    `json:"token,omitempty"` // returned once on create
	Status    string    `json:"status"`          // "running" | "deleted"
	CreatedAt time.Time `json:"created_at"`
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
	AgentImage  string            // fallback image when the agent has no entry in AgentImages
	AgentImages map[string]string // agent name → image, e.g. {"claude": ..., "pi": ...}
	GatewayURL  string            // Anthropic-path gateway URL (ANTHROPIC_BASE_URL for claude)
	OpenAIURL   string            // OpenAI-path gateway URL for openai-completions agents
	OpenAIModel string            // model id pinned for those agents (empty = not configured)
}

// imageFor picks the sandbox image for an agent; empty means unsupported.
func (c Config) imageFor(agent string) string {
	if img, ok := c.AgentImages[agent]; ok {
		return img
	}
	return c.AgentImage
}

// Handler wires the HTTP surface.
type Handler struct {
	Sessions    *Store
	Ledger      *budget.Ledger
	Audit       *audit.Store
	Provisioner sandbox.Provisioner
	Config      Config
}

func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("POST /v1/sessions", h.createSession)
	mux.HandleFunc("GET /v1/sessions", h.listSessions)
	mux.HandleFunc("GET /v1/sessions/{id}", h.getSession)
	mux.HandleFunc("DELETE /v1/sessions/{id}", h.deleteSession)
	mux.HandleFunc("GET /v1/sessions/{id}/events", h.sessionEvents)
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
	var req struct {
		User     string `json:"user"`
		Agent    string `json:"agent"`
		BudgetID string `json:"budget_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.User == "" || req.Agent == "" || req.BudgetID == "" {
		http.Error(w, "user, agent and budget_id are required", http.StatusBadRequest)
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
		User:      req.User,
		Agent:     req.Agent,
		BudgetID:  req.BudgetID,
		Token:     "pdk_" + randomID(24),
		Status:    "running",
		CreatedAt: time.Now().UTC(),
	}
	if err := h.Sessions.insert(sess); err != nil {
		http.Error(w, "store session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	err = h.Provisioner.Create(r.Context(), sandbox.Spec{
		SessionID:    sess.ID,
		User:         sess.User,
		Agent:        sess.Agent,
		AgentImage:   image,
		GatewayURL:   h.Config.GatewayURL,
		OpenAIURL:    h.Config.OpenAIURL,
		Model:        h.Config.OpenAIModel,
		SessionToken: sess.Token,
	})
	if err != nil {
		_ = h.Sessions.setStatus(sess.ID, "failed")
		http.Error(w, "provision sandbox: "+err.Error(), http.StatusBadGateway)
		return
	}
	_ = h.Audit.Append(audit.Event{
		SessionID: sess.ID, Actor: sess.User, Kind: audit.KindSessionCreated,
		Payload: map[string]any{"agent": sess.Agent, "budget_id": sess.BudgetID},
	})
	writeJSON(w, http.StatusCreated, sess)
}

func (h *Handler) listSessions(w http.ResponseWriter, _ *http.Request) {
	sessions, err := h.Sessions.list()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []Session{}
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
	writeJSON(w, http.StatusOK, sess)
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
	if err := h.Provisioner.Delete(r.Context(), id); err != nil {
		http.Error(w, "teardown sandbox: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := h.Sessions.setStatus(id, "deleted"); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = h.Audit.Append(audit.Event{
		SessionID: id, Actor: sess.User, Kind: audit.KindSessionDeleted,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) sessionEvents(w http.ResponseWriter, r *http.Request) {
	events, err := h.Audit.BySession(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []audit.Event{}
	}
	writeJSON(w, http.StatusOK, events)
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
