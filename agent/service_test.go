package agent

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamble/dcr402/agent/approve"
	"github.com/karamble/dcr402/agent/payclient"
	"github.com/karamble/dcr402/agent/policy"
	"github.com/karamble/dcr402/agent/store"
)

// fakePay implements PayClient and returns a canned result/error, standing in
// for a merchant that may settle then error.
type fakePay struct {
	res *payclient.Result
	err error
}

func (f *fakePay) FetchPaid(context.Context, string, payclient.FetchOptions) (*payclient.Result, error) {
	return f.res, f.err
}

func (f *fakePay) Estimate(context.Context, string) (*payclient.Estimate, error) {
	return nil, errors.New("not implemented")
}

// TestSettledPaymentRecordedOnMerchantError checks that a payment that settled
// but whose merchant then returned an HTTP error is still written to the
// ledger, so the ledger-derived budget/velocity caps count it.
func TestSettledPaymentRecordedOnMerchantError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte("mode: default-allow\nmemo_required: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng, err := policy.NewEngine(path)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	reg := approve.NewRegistry(st, time.Now, approve.WebApprover{})
	pay := &fakePay{
		res: &payclient.Result{Rail: payclient.RailX402, PaidAtoms: 10_000_000, Preimage: "aa", Invoice: "lnsdcr1..."},
		err: errors.New("payclient: paid but service answered 500"),
	}
	svc := NewService(ServiceDeps{
		Policy: eng, Store: st, LN: &fakeLN{amountAtoms: 10_000_000}, Pay: pay, Approval: reg, Now: time.Now,
	})

	if _, err := svc.FetchPaid(context.Background(), "https://merchant.example/x",
		payclient.FetchOptions{Memo: "m"}); err == nil {
		t.Fatal("expected the merchant error to surface")
	}
	u, _ := st.Usage(context.Background(), time.Now())
	if u.DayAtoms != 10_000_000 {
		t.Fatalf("settled-then-errored payment not recorded: day spend %d, want 10000000", u.DayAtoms)
	}
}

// fakeLN implements LNRail: it "pays" any invoice for a fixed amount.
type fakeLN struct {
	amountAtoms int64
	mu          sync.Mutex
	paid        int
}

func (f *fakeLN) DecodeInvoice(string) (LNInvoice, error) {
	return LNInvoice{AmountAtoms: f.amountAtoms, Destination: "03deadbeef"}, nil
}
func (f *fakeLN) Pay(context.Context, string, int64, time.Duration) (string, int64, error) {
	f.mu.Lock()
	f.paid++
	f.mu.Unlock()
	return hex.EncodeToString(make([]byte, 32)), 1, nil
}
func (f *fakeLN) SpendableAtoms(context.Context) (int64, error) { return 1 << 40, nil }
func (f *fakeLN) Info(context.Context) (string, bool, error)    { return "03node", true, nil }
func (f *fakeLN) CreateInvoice(context.Context, int64, string, time.Duration) (string, string, error) {
	return "lnsdcr1...", "hash", nil
}

func testService(t *testing.T, policyYAML string) (*Service, *fakeLN, store.Store) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(policyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	eng, err := policy.NewEngine(path)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	ln := &fakeLN{amountAtoms: 10_000_000} // 0.1 DCR per payment
	reg := approve.NewRegistry(st, time.Now, approve.WebApprover{})
	svc := NewService(ServiceDeps{
		Policy: eng, Store: st, LN: ln, Approval: reg, Now: time.Now,
	})
	return svc, ln, st
}

// TestFreezeDeniesSpending checks the kill-switch: once frozen, every spend
// path returns FrozenError until the owner unfreezes.
func TestFreezeDeniesSpending(t *testing.T) {
	svc, _, _ := testService(t, "mode: default-allow\nmemo_required: false\n")
	ctx := context.Background()

	if _, err := svc.PayLNInvoice(ctx, "lnsdcr1...", "ok", 0); err != nil {
		t.Fatalf("baseline payment should succeed: %v", err)
	}

	if err := svc.Freeze(ctx, "test", "kill switch"); err != nil {
		t.Fatal(err)
	}
	var fe *FrozenError
	if _, err := svc.PayLNInvoice(ctx, "lnsdcr1...", "blocked", 0); !errors.As(err, &fe) {
		t.Fatalf("a frozen payment must return FrozenError, got %v", err)
	}

	if err := svc.Unfreeze(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PayLNInvoice(ctx, "lnsdcr1...", "resumed", 0); err != nil {
		t.Fatalf("payment should resume after unfreeze: %v", err)
	}
}

// TestConcurrentBudgetNotOvershot fires more concurrent payments than the
// daily budget allows and asserts the ledger never exceeds it — the payMu
// serialization closing the check-then-act race.
func TestConcurrentBudgetNotOvershot(t *testing.T) {
	// budget 0.35 DCR, payments 0.1 DCR each → at most 3 may settle.
	svc, ln, st := testService(t, `
mode: default-allow
limits: {daily_budget: "0.35"}
memo_required: false
`)
	const workers = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.PayLNInvoice(context.Background(), "lnsdcr1...", "concurrent", 0)
			if err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if ok != 3 {
		t.Fatalf("%d payments succeeded, want exactly 3 (budget 0.35 / 0.1 each)", ok)
	}
	if ln.paid != 3 {
		t.Fatalf("rail paid %d times, want 3", ln.paid)
	}
	u, _ := st.Usage(context.Background(), time.Now())
	if u.DayAtoms > 35_000_000 {
		t.Fatalf("day spend %d exceeds the 0.35 DCR budget", u.DayAtoms)
	}
}

// TestPreApprovedAmountCeiling checks that the gate refuses a pre-approved
// payment whose (re-quoted) amount exceeds what the owner approved.
func TestPreApprovedAmountCeiling(t *testing.T) {
	svc, _, _ := testService(t, `
mode: default-allow
approval: {threshold: "0.1", ttl: 2m, channel: web}
memo_required: false
`)
	ctx := context.Background()
	att := policy.Attempt{
		Tool: "fetch_paid", Rail: "ln", DestKind: policy.DestDomain,
		Dest: "sat.example.com", AmountAtoms: 30_000_000, Memo: "x",
	}

	// Within the approved ceiling → allowed.
	within := context.WithValue(ctx, preApprovedKey, preApproved{maxAtoms: 30_000_000})
	if _, err := svc.gate(within, att); err != nil {
		t.Fatalf("at-ceiling payment rejected: %v", err)
	}

	// Above the approved ceiling → denied challenge_changed.
	over := context.WithValue(ctx, preApprovedKey, preApproved{maxAtoms: 20_000_000})
	_, err := svc.gate(over, att)
	var pol *PolicyError
	if !errors.As(err, &pol) {
		t.Fatalf("expected PolicyError, got %v", err)
	}
	if !strings.Contains(pol.Decision.Reason, "challenge_changed") || pol.Decision.Outcome != policy.Deny {
		t.Fatalf("expected challenge_changed denial, got %+v", pol.Decision)
	}
}
