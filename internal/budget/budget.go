// Package budget implements the hierarchical spend ledger: org → team →
// user → session. Every model call is priced and debited against a budget
// node; a debit fails if the node or any ancestor is exhausted.
package budget

import (
	"database/sql"
	"fmt"
	"time"
)

// WarnRatio is the fraction of a budget's limit at which soft warnings are
// emitted alongside successful debits.
const WarnRatio = 0.8

// Price is the cost per million tokens for one model, in USD.
type Price struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// PriceTable maps model IDs to prices. List prices drift, so deployments can
// override this from config; these defaults keep the ledger useful out of
// the box.
type PriceTable map[string]Price

// DefaultPrices covers the models the gateway proxies most often. Unknown
// models fall back to the most expensive known price so metering errs on the
// side of overcounting, never undercounting.
var DefaultPrices = PriceTable{
	"claude-opus-4-8":            {InputPerMTok: 15, OutputPerMTok: 75},
	"claude-sonnet-5":            {InputPerMTok: 3, OutputPerMTok: 15},
	"claude-haiku-4-5-20251001":  {InputPerMTok: 1, OutputPerMTok: 5},
}

// Cost prices a call. Unknown models are billed at the table's most
// expensive entry.
func (t PriceTable) Cost(model string, inputTokens, outputTokens int64) float64 {
	p, ok := t[model]
	if !ok {
		for _, q := range t {
			if q.InputPerMTok > p.InputPerMTok {
				p = q
			}
		}
	}
	return float64(inputTokens)/1e6*p.InputPerMTok + float64(outputTokens)/1e6*p.OutputPerMTok
}

// Budget is one node in the hierarchy.
type Budget struct {
	ID       string
	ParentID string // empty for the root (org) node
	Name     string
	LimitUSD float64
	SpentUSD float64
}

// DebitResult reports the outcome of a debit.
type DebitResult struct {
	CostUSD  float64
	Warnings []string // soft-threshold warnings for this node or ancestors
	// Exceeded is true when the debit pushed any node in the chain over its
	// limit. Debits are post-paid records of tokens already consumed, so the
	// ledger records them regardless; enforcement is the pre-call headroom
	// check (Remaining <= 0 → hard stop).
	Exceeded bool
}

// Ledger stores budgets and entries in SQLite.
type Ledger struct {
	db     *sql.DB
	prices PriceTable
}

func NewLedger(db *sql.DB, prices PriceTable) (*Ledger, error) {
	if prices == nil {
		prices = DefaultPrices
	}
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS budgets (
			id        TEXT PRIMARY KEY,
			parent_id TEXT NOT NULL DEFAULT '',
			name      TEXT NOT NULL,
			limit_usd REAL NOT NULL,
			spent_usd REAL NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS ledger_entries (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			budget_id     TEXT NOT NULL,
			ts            TEXT NOT NULL,
			model         TEXT NOT NULL,
			input_tokens  INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			cost_usd      REAL NOT NULL
		);`)
	if err != nil {
		return nil, fmt.Errorf("migrate budget schema: %w", err)
	}
	return &Ledger{db: db, prices: prices}, nil
}

func (l *Ledger) Create(b Budget) error {
	_, err := l.db.Exec(
		`INSERT INTO budgets (id, parent_id, name, limit_usd, spent_usd) VALUES (?, ?, ?, ?, ?)`,
		b.ID, b.ParentID, b.Name, b.LimitUSD, b.SpentUSD,
	)
	return err
}

func (l *Ledger) Get(id string) (Budget, error) {
	var b Budget
	err := l.db.QueryRow(
		`SELECT id, parent_id, name, limit_usd, spent_usd FROM budgets WHERE id = ?`, id,
	).Scan(&b.ID, &b.ParentID, &b.Name, &b.LimitUSD, &b.SpentUSD)
	return b, err
}

// chain returns the budget and all its ancestors, leaf first.
func (l *Ledger) chain(tx *sql.Tx, id string) ([]Budget, error) {
	var out []Budget
	for id != "" {
		var b Budget
		err := tx.QueryRow(
			`SELECT id, parent_id, name, limit_usd, spent_usd FROM budgets WHERE id = ?`, id,
		).Scan(&b.ID, &b.ParentID, &b.Name, &b.LimitUSD, &b.SpentUSD)
		if err != nil {
			return nil, fmt.Errorf("budget %q: %w", id, err)
		}
		out = append(out, b)
		if len(out) > 16 {
			return nil, fmt.Errorf("budget hierarchy too deep or cyclic at %q", id)
		}
		id = b.ParentID
	}
	return out, nil
}

// Debit prices the call and atomically debits the budget and every ancestor.
// It never refuses on breach — the tokens were already consumed upstream —
// but flags Exceeded so callers can hard-stop the session's next call.
func (l *Ledger) Debit(budgetID, model string, inputTokens, outputTokens int64) (DebitResult, error) {
	cost := l.prices.Cost(model, inputTokens, outputTokens)
	res := DebitResult{CostUSD: cost}

	tx, err := l.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	nodes, err := l.chain(tx, budgetID)
	if err != nil {
		return res, err
	}
	for _, b := range nodes {
		if b.SpentUSD+cost > b.LimitUSD {
			res.Exceeded = true
		}
		if _, err := tx.Exec(`UPDATE budgets SET spent_usd = spent_usd + ? WHERE id = ?`, cost, b.ID); err != nil {
			return res, err
		}
		if (b.SpentUSD+cost)/b.LimitUSD >= WarnRatio {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"budget %q (%s) at %.0f%% of limit", b.ID, b.Name, (b.SpentUSD+cost)/b.LimitUSD*100))
		}
	}
	_, err = tx.Exec(
		`INSERT INTO ledger_entries (budget_id, ts, model, input_tokens, output_tokens, cost_usd) VALUES (?, ?, ?, ?, ?, ?)`,
		budgetID, time.Now().UTC().Format(time.RFC3339Nano), model, inputTokens, outputTokens, cost,
	)
	if err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// Remaining reports headroom on the tightest node in the chain, which is the
// number the gateway checks before letting a call through.
func (l *Ledger) Remaining(budgetID string) (float64, error) {
	tx, err := l.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	nodes, err := l.chain(tx, budgetID)
	if err != nil {
		return 0, err
	}
	remaining := nodes[0].LimitUSD - nodes[0].SpentUSD
	for _, b := range nodes[1:] {
		if r := b.LimitUSD - b.SpentUSD; r < remaining {
			remaining = r
		}
	}
	return remaining, nil
}
