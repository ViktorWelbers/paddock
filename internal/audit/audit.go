// Package audit is the append-only event store behind Paddock's compliance
// story. OSS keeps the schema identical to the enterprise tier so evidence
// remains portable; the enterprise tier adds hash-chaining and SIEM export.
package audit

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Event kinds. New kinds may be added; consumers must ignore unknown ones.
const (
	KindSessionCreated  = "session.created"
	KindSessionDeleted  = "session.deleted"
	KindModelCall       = "model.call"
	KindBudgetWarn      = "budget.warn"
	KindBudgetExhausted = "budget.exhausted"
	KindMCPCall         = "mcp.call"
	KindPolicyDenied    = "policy.denied"
)

type Event struct {
	ID        int64          `json:"id"`
	TS        time.Time      `json:"ts"`
	SessionID string         `json:"session_id"`
	Actor     string         `json:"actor"`
	Kind      string         `json:"kind"`
	Payload   map[string]any `json:"payload,omitempty"`
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) (*Store, error) {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS audit_events (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			ts         TEXT NOT NULL,
			session_id TEXT NOT NULL,
			actor      TEXT NOT NULL,
			kind       TEXT NOT NULL,
			payload    TEXT NOT NULL DEFAULT '{}'
		);
		CREATE INDEX IF NOT EXISTS idx_audit_session ON audit_events (session_id, id);`)
	if err != nil {
		return nil, fmt.Errorf("migrate audit schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Append(e Event) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("marshal audit payload: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO audit_events (ts, session_id, actor, kind, payload) VALUES (?, ?, ?, ?, ?)`,
		e.TS.Format(time.RFC3339Nano), e.SessionID, e.Actor, e.Kind, string(payload),
	)
	return err
}

// BySession returns a session's events in insertion order.
func (s *Store) BySession(sessionID string) ([]Event, error) {
	rows, err := s.db.Query(
		`SELECT id, ts, session_id, actor, kind, payload FROM audit_events WHERE session_id = ? ORDER BY id`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var ts, payload string
		if err := rows.Scan(&e.ID, &ts, &e.SessionID, &e.Actor, &e.Kind, &payload); err != nil {
			return nil, err
		}
		if e.TS, err = time.Parse(time.RFC3339Nano, ts); err != nil {
			return nil, fmt.Errorf("event %d: bad timestamp %q: %w", e.ID, ts, err)
		}
		if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil {
			return nil, fmt.Errorf("event %d: bad payload: %w", e.ID, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
