package bisonrelay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/karamble/dcr402/agent/approve"
	"github.com/karamble/dcr402/agent/store"
)

type fakeFreezer struct{ froze, unfroze bool }

func (f *fakeFreezer) Freeze(context.Context, string, string) error { f.froze = true; return nil }
func (f *fakeFreezer) Unfreeze(context.Context, string) error       { f.unfroze = true; return nil }

func newTestApprover() (*Approver, *fakeFreezer, *approve.Registry) {
	st := store.NewMemory()
	reg := approve.NewRegistry(st, time.Now, approve.WebApprover{})
	fz := &fakeFreezer{}
	a := &Approver{cfg: Config{Registry: reg, Store: st, Freezer: fz, Owner: "owner"}}
	return a, fz, reg
}

func escalate(t *testing.T, reg *approve.Registry) store.Approval {
	t.Helper()
	appr, err := reg.Escalate(context.Background(), 1, "both", time.Minute, "s",
		func(context.Context, bool, string) {})
	if err != nil {
		t.Fatal(err)
	}
	return appr
}

// TestHandleReplyGrammar covers the reply grammar, including the leading
// freeze/block forms and the kill-switch hook.
func TestHandleReplyGrammar(t *testing.T) {
	ctx := context.Background()

	t.Run("leading freeze trips the kill-switch", func(t *testing.T) {
		a, fz, _ := newTestApprover()
		a.handleReply(ctx, "freeze")
		if !fz.froze {
			t.Fatal(`"freeze" must trip the kill-switch`)
		}
	})

	t.Run("freeze with an id also denies it", func(t *testing.T) {
		a, fz, reg := newTestApprover()
		appr := escalate(t, reg)
		a.handleReply(ctx, "freeze "+appr.ID)
		if !fz.froze {
			t.Fatal("froze")
		}
		got, _ := reg.Get(ctx, appr.ID)
		if got.Status != store.ApprovalDenied {
			t.Fatalf("named approval should be denied, got %q", got.Status)
		}
	})

	t.Run("unfreeze resumes", func(t *testing.T) {
		a, fz, _ := newTestApprover()
		a.handleReply(ctx, "unfreeze")
		if !fz.unfroze {
			t.Fatal(`"unfreeze" must resume spending`)
		}
	})

	t.Run("trailing freeze still trips it and denies", func(t *testing.T) {
		a, fz, reg := newTestApprover()
		appr := escalate(t, reg)
		a.handleReply(ctx, "no "+appr.ID+" freeze")
		if !fz.froze {
			t.Fatal("a trailing freeze must trip the kill-switch")
		}
		got, _ := reg.Get(ctx, appr.ID)
		if got.Status != store.ApprovalDenied {
			t.Fatalf("approval should be denied, got %q", got.Status)
		}
	})

	t.Run("plain yes approves without freezing", func(t *testing.T) {
		a, fz, reg := newTestApprover()
		appr := escalate(t, reg)
		a.handleReply(ctx, "yes "+appr.ID)
		if fz.froze {
			t.Fatal(`"yes" must not freeze`)
		}
		got, _ := reg.Get(ctx, appr.ID)
		if got.Status != store.ApprovalApproved {
			t.Fatalf("approval should be approved, got %q", got.Status)
		}
	})

	t.Run("bare verdict with no id is ignored", func(t *testing.T) {
		a, _, reg := newTestApprover()
		appr := escalate(t, reg)
		a.handleReply(ctx, "yes")
		got, _ := reg.Get(ctx, appr.ID)
		if got.Status != store.ApprovalPending {
			t.Fatalf("a bare verdict must not resolve anything, got %q", got.Status)
		}
	})
}

// failResolveStore embeds a working store but fails ResolveApproval, so a
// verdict for a pending approval hits a store error.
type failResolveStore struct {
	store.Store
}

func (failResolveStore) ResolveApproval(context.Context, string, string, string, time.Time) (bool, error) {
	return false, errors.New("store unavailable")
}

// TestHandleReplyHoldsAckOnResolveError: a store failure while resolving a
// verdict must propagate (so consume holds the ack and replays the PM), while a
// reply that names no live approval - or is not a verdict - returns nil (the
// ack advances; there is nothing to retry).
func TestHandleReplyHoldsAckOnResolveError(t *testing.T) {
	ctx := context.Background()

	// Pending approval whose resolve hits a failing store -> error propagates.
	fs := failResolveStore{Store: store.NewMemory()}
	freg := approve.NewRegistry(fs, time.Now, approve.WebApprover{})
	fa := &Approver{cfg: Config{Registry: freg, Store: fs, Freezer: &fakeFreezer{}, Owner: "owner"}}
	appr := escalate(t, freg)
	if err := fa.handleReply(ctx, "yes "+appr.ID); err == nil {
		t.Fatal("a store error during resolve must propagate so the ack is held")
	}

	// A verdict naming no live approval, and plain chatter, must not hold it.
	a, _, _ := newTestApprover()
	if err := a.handleReply(ctx, "yes deadbeefcafe"); err != nil {
		t.Fatalf("naming no live approval must not hold the ack: %v", err)
	}
	if err := a.handleReply(ctx, "hello there"); err != nil {
		t.Fatalf("chatter must not hold the ack: %v", err)
	}
}
