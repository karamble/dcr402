package approve

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/karamble/dcr402/agent/store"
)

// recordApprover records notifications and can be made unreachable.
type recordApprover struct {
	name        string
	unreachable bool
	mu          sync.Mutex
	got         []Request
}

func (r *recordApprover) Name() string { return r.name }
func (r *recordApprover) Notify(_ context.Context, req Request) error {
	if r.unreachable {
		return errors.New("unreachable")
	}
	r.mu.Lock()
	r.got = append(r.got, req)
	r.mu.Unlock()
	return nil
}

func newReg(t *testing.T, now func() time.Time, ap ...Approver) (*Registry, store.Store) {
	t.Helper()
	st := store.NewMemory()
	return NewRegistry(st, now, ap...), st
}

func TestEscalateApproveRunsContinuation(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	ap := &recordApprover{name: "web"}
	reg, st := newReg(t, func() time.Time { return now }, ap)
	ctx := context.Background()

	done := make(chan bool, 1)
	appr, err := reg.Escalate(ctx, 7, "web", 15*time.Minute, "approve payment X?",
		func(_ context.Context, approved bool, _ string) { done <- approved })
	if err != nil {
		t.Fatalf("Escalate: %v", err)
	}
	if len(ap.got) != 1 || ap.got[0].ID != appr.ID {
		t.Fatalf("notify: %+v", ap.got)
	}

	won, err := reg.Resolve(ctx, appr.ID, true, "frank")
	if err != nil || !won {
		t.Fatalf("Resolve: won=%v err=%v", won, err)
	}
	select {
	case approved := <-done:
		if !approved {
			t.Fatal("continuation got deny")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("continuation never ran")
	}
	got, _ := st.GetApproval(ctx, appr.ID)
	if got.Status != store.ApprovalApproved || got.Responder != "frank" {
		t.Fatalf("stored: %+v", got)
	}
	// Second resolve loses.
	if won, _ := reg.Resolve(ctx, appr.ID, false, "attacker"); won {
		t.Fatal("second resolver won")
	}
}

func TestEscalateUnreachableFailsClosed(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	ap := &recordApprover{name: "bisonrelay", unreachable: true}
	reg, st := newReg(t, func() time.Time { return now }, ap)
	ctx := context.Background()

	ran := false
	_, err := reg.Escalate(ctx, 1, "bisonrelay", time.Minute, "x",
		func(context.Context, bool, string) { ran = true })
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected ErrUnreachable, got %v", err)
	}
	if ran {
		t.Fatal("continuation ran on unreachable escalation")
	}
	// The approval was recorded as denied, not left pending.
	pending, _ := st.ListPendingApprovals(ctx, now)
	if len(pending) != 0 {
		t.Fatalf("unreachable left %d pending", len(pending))
	}
}

func TestExpiryDeniesAndRunsContinuation(t *testing.T) {
	cur := time.Unix(1750000000, 0).UTC()
	nowFn := func() time.Time { return cur }
	reg, st := newReg(t, nowFn, &recordApprover{name: "web"})
	ctx := context.Background()

	verdict := make(chan bool, 1)
	appr, err := reg.Escalate(ctx, 3, "web", time.Minute, "x",
		func(_ context.Context, approved bool, _ string) { verdict <- approved })
	if err != nil {
		t.Fatal(err)
	}

	cur = cur.Add(2 * time.Minute) // past TTL
	reg.ExpireNow(ctx)
	select {
	case approved := <-verdict:
		if approved {
			t.Fatal("expiry approved")
		}
	default:
		t.Fatal("expiry did not run continuation")
	}
	got, _ := st.GetApproval(ctx, appr.ID)
	if got.Status != store.ApprovalExpired {
		t.Fatalf("status after expiry: %s", got.Status)
	}
	// Resolving an expired approval loses.
	if won, _ := reg.Resolve(ctx, appr.ID, true, "late"); won {
		t.Fatal("resolved an expired approval")
	}
}

func TestChannelSelection(t *testing.T) {
	now := time.Unix(1750000000, 0).UTC()
	br := &recordApprover{name: "bisonrelay"}
	web := &recordApprover{name: "web"}
	reg, _ := newReg(t, func() time.Time { return now }, br, web)
	ctx := context.Background()

	// channel "both" notifies everyone.
	if _, err := reg.Escalate(ctx, 1, "both", time.Minute, "x", func(context.Context, bool, string) {}); err != nil {
		t.Fatal(err)
	}
	if len(br.got) != 1 || len(web.got) != 1 {
		t.Fatalf("both: br=%d web=%d", len(br.got), len(web.got))
	}
	// channel "web" notifies only web.
	if _, err := reg.Escalate(ctx, 2, "web", time.Minute, "x", func(context.Context, bool, string) {}); err != nil {
		t.Fatal(err)
	}
	if len(br.got) != 1 || len(web.got) != 2 {
		t.Fatalf("web only: br=%d web=%d", len(br.got), len(web.got))
	}
}
