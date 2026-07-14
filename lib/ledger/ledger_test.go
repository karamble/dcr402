package ledger

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func implementations(t *testing.T) map[string]Ledger {
	t.Helper()
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { sq.Close() })
	return map[string]Ledger{"memory": NewMemory(), "sqlite": sq}
}

func TestGrantIdempotent(t *testing.T) {
	for name, l := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Unix(1750000000, 0)

			bal, already, err := l.Grant(ctx, "acct", "onchain", 1000, "ref-1", []byte(`{}`), now)
			if err != nil || already || bal != 1000 {
				t.Fatalf("first grant: bal=%d already=%v err=%v", bal, already, err)
			}
			// Same ref again — even with a different amount/account — must
			// not double-credit and answers with the original account.
			bal, already, err = l.Grant(ctx, "other", "onchain", 5000, "ref-1", nil, now)
			if err != nil || !already || bal != 1000 {
				t.Fatalf("replayed grant: bal=%d already=%v err=%v", bal, already, err)
			}
			if b, _ := l.Balance(ctx, "other"); b != 0 {
				t.Fatalf("replayed grant credited the wrong account: %d", b)
			}
			// A new ref accumulates.
			bal, _, err = l.Grant(ctx, "acct", "lightning", 500, "ref-2", nil, now)
			if err != nil || bal != 1500 {
				t.Fatalf("second grant: bal=%d err=%v", bal, err)
			}
		})
	}
}

func TestChargeAtomic(t *testing.T) {
	for name, l := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Unix(1750000000, 0)
			if _, _, err := l.Grant(ctx, "acct", "onchain", 1000, "ref", nil, now); err != nil {
				t.Fatal(err)
			}

			bal, err := l.Charge(ctx, "acct", "tool", 400, "r1", "", now)
			if err != nil || bal != 600 {
				t.Fatalf("charge: bal=%d err=%v", bal, err)
			}
			// Overdraft rejected with exact shortfall; balance untouched.
			_, err = l.Charge(ctx, "acct", "tool", 700, "r2", "", now)
			var insuff *InsufficientBalanceError
			if !errors.As(err, &insuff) {
				t.Fatalf("expected InsufficientBalanceError, got %v", err)
			}
			if insuff.Balance != 600 || insuff.Required != 700 || insuff.Shortfall() != 100 {
				t.Fatalf("shortfall wrong: %+v", insuff)
			}
			if b, _ := l.Balance(ctx, "acct"); b != 600 {
				t.Fatalf("failed charge moved the balance: %d", b)
			}
			// Unknown account charges are insufficient, not errors.
			_, err = l.Charge(ctx, "ghost", "tool", 1, "r3", "", now)
			if !errors.As(err, &insuff) || insuff.Balance != 0 {
				t.Fatalf("ghost charge: %v", err)
			}
		})
	}
}

// TestChargeIdempotent: a repeated non-empty idempotency key charges at most
// once; distinct keys each charge; an empty key always charges (no dedup).
func TestChargeIdempotent(t *testing.T) {
	for name, l := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Unix(1750000000, 0)
			if _, _, err := l.Grant(ctx, "acct", "onchain", 1000, "idem-grant", nil, now); err != nil {
				t.Fatal(err)
			}
			// Same key twice: exactly one debit, second returns the balance.
			b1, err := l.Charge(ctx, "acct", "tool", 300, "ref", "k1", now)
			if err != nil || b1 != 700 {
				t.Fatalf("first charge: bal=%d err=%v", b1, err)
			}
			b2, err := l.Charge(ctx, "acct", "tool", 300, "ref", "k1", now)
			if err != nil || b2 != 700 {
				t.Fatalf("idempotent replay must not debit again: bal=%d err=%v", b2, err)
			}
			// A different key debits.
			if b3, err := l.Charge(ctx, "acct", "tool", 300, "ref", "k2", now); err != nil || b3 != 400 {
				t.Fatalf("distinct key: bal=%d err=%v", b3, err)
			}
			// Empty key always charges (no dedup).
			if _, err := l.Charge(ctx, "acct", "tool", 100, "ref", "", now); err != nil {
				t.Fatalf("empty-key charge 1: %v", err)
			}
			if b5, err := l.Charge(ctx, "acct", "tool", 100, "ref", "", now); err != nil || b5 != 200 {
				t.Fatalf("empty key must charge every call: bal=%d err=%v", b5, err)
			}
		})
	}
}

// TestChargeRace: concurrent charges must never overspend.
func TestChargeRace(t *testing.T) {
	for name, l := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Unix(1750000000, 0)
			if _, _, err := l.Grant(ctx, "acct", "onchain", 1000, "race-ref", nil, now); err != nil {
				t.Fatal(err)
			}
			const workers = 16 // 16 × 100 = 1600 attempted > 1000 available
			var wg sync.WaitGroup
			var mu sync.Mutex
			succeeded := 0
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := l.Charge(ctx, "acct", "tool", 100, "", "", now); err == nil {
						mu.Lock()
						succeeded++
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
			if succeeded != 10 {
				t.Fatalf("%d charges succeeded, want exactly 10", succeeded)
			}
			if b, _ := l.Balance(ctx, "acct"); b != 0 {
				t.Fatalf("balance %d after race, want 0", b)
			}
		})
	}
}

func TestSQLitePersists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "persist.db")
	l, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Grant(ctx, "acct", "onchain", 777, "ref", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	l.Close()

	l2, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if b, _ := l2.Balance(ctx, "acct"); b != 777 {
		t.Fatalf("balance not durable: %d", b)
	}
	// The grant ref survives too.
	if _, already, _ := l2.Grant(ctx, "acct", "onchain", 777, "ref", nil, time.Now()); !already {
		t.Fatal("grant ref not durable")
	}
}
