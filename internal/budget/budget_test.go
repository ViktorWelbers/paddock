package budget

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func testLedger(t *testing.T) *Ledger {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	l, err := NewLedger(db, PriceTable{
		"test-model": {InputPerMTok: 10, OutputPerMTok: 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func seedHierarchy(t *testing.T, l *Ledger) {
	t.Helper()
	for _, b := range []Budget{
		{ID: "org", Name: "org", LimitUSD: 100},
		{ID: "team", ParentID: "org", Name: "team", LimitUSD: 50},
		{ID: "user", ParentID: "team", Name: "user", LimitUSD: 10},
	} {
		if err := l.Create(b); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDebitWalksHierarchy(t *testing.T) {
	l := testLedger(t)
	seedHierarchy(t, l)

	// 100k in + 100k out = 100k/1M*10 + 100k/1M*20 = 1 + 2 = 3 USD
	res, err := l.Debit("user", "test-model", 100_000, 100_000)
	if err != nil {
		t.Fatalf("debit: %v", err)
	}
	if res.CostUSD != 3 {
		t.Fatalf("cost = %v, want 3", res.CostUSD)
	}
	for _, id := range []string{"user", "team", "org"} {
		b, err := l.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if b.SpentUSD != 3 {
			t.Errorf("budget %s spent = %v, want 3", id, b.SpentUSD)
		}
	}
}

func TestDebitFlagsBreachOnLeaf(t *testing.T) {
	l := testLedger(t)
	seedHierarchy(t, l)

	// 400k out = 8 USD, fits the 10 USD user budget once but not twice.
	res, err := l.Debit("user", "test-model", 0, 400_000)
	if err != nil || res.Exceeded {
		t.Fatalf("first debit: err=%v exceeded=%v", err, res.Exceeded)
	}
	// Post-paid: the second debit is recorded (tokens were consumed) but
	// flagged, and Remaining goes negative so the gateway hard-stops next.
	res, err = l.Debit("user", "test-model", 0, 400_000)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Exceeded {
		t.Fatal("second debit should be flagged Exceeded")
	}
	rem, err := l.Remaining("user")
	if err != nil {
		t.Fatal(err)
	}
	if rem >= 0 {
		t.Fatalf("remaining = %v, want negative after overshoot", rem)
	}
}

func TestDebitFlagsBreachOnAncestor(t *testing.T) {
	l := testLedger(t)
	seedHierarchy(t, l)
	// Exhaust the team budget through a sibling user.
	if err := l.Create(Budget{ID: "user2", ParentID: "team", Name: "user2", LimitUSD: 50}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Debit("user2", "test-model", 0, 2_400_000); err != nil { // 48 USD
		t.Fatal(err)
	}
	// user has personal headroom (10) but team only has 2 left.
	res, err := l.Debit("user", "test-model", 0, 400_000) // 8 USD
	if err != nil {
		t.Fatal(err)
	}
	if !res.Exceeded {
		t.Fatal("debit should be flagged Exceeded via the team ancestor")
	}
}

func TestWarnThreshold(t *testing.T) {
	l := testLedger(t)
	seedHierarchy(t, l)
	res, err := l.Debit("user", "test-model", 0, 450_000) // 9 USD = 90% of user limit
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a soft-threshold warning at 90% of limit")
	}
}

func TestRemainingIsTightestNode(t *testing.T) {
	l := testLedger(t)
	seedHierarchy(t, l)
	rem, err := l.Remaining("user")
	if err != nil {
		t.Fatal(err)
	}
	if rem != 10 {
		t.Fatalf("remaining = %v, want 10 (user is tightest)", rem)
	}
}

func TestUnknownModelBillsAtMostExpensive(t *testing.T) {
	pt := PriceTable{
		"cheap":  {InputPerMTok: 1, OutputPerMTok: 2},
		"pricey": {InputPerMTok: 10, OutputPerMTok: 20},
	}
	if got := pt.Cost("never-heard-of-it", 1_000_000, 0); got != 10 {
		t.Fatalf("unknown model cost = %v, want 10 (priciest input rate)", got)
	}
}
