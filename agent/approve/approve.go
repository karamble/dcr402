// Package approve is dcr402-agent's human-in-the-loop escalation: policy
// decisions above the approval threshold park the payment as a pending
// approval, notify the owner over pluggable channels (Bison Relay, the web
// dashboard, anything implementing Approver), and execute the payment
// asynchronously once — and only if — the owner says yes within the TTL.
// Everything fails closed: unreachable channels deny, expired TTLs deny,
// and a daemon restart drops continuations so stale approvals can never
// move money.
package approve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/karamble/dcr402/agent/store"
)

// Request is what an Approver presents to the owner.
type Request struct {
	ID        string
	Summary   string // the complete human-readable message
	ExpiresAt time.Time
}

// Approver notifies the owner about a pending request. Resolution flows
// back through Registry.Resolve (each channel implements its own listener).
type Approver interface {
	// Name identifies the channel ("bisonrelay", "web", ...).
	Name() string
	// Notify presents the request to the owner. An error marks this
	// channel unreachable for the request.
	Notify(ctx context.Context, req Request) error
}

// WebApprover is the pull-based web channel: pending approvals are listed
// from the store by the dashboard, so notification is inherently
// successful.
type WebApprover struct{}

func (WebApprover) Name() string                          { return "web" }
func (WebApprover) Notify(context.Context, Request) error { return nil }

// Continuation runs when the approval resolves. approved is false for
// deny, expiry, and freeze; responder says who/what resolved it.
type Continuation func(ctx context.Context, approved bool, responder string)

// PendingError is returned to tools when a payment escalated: not a
// failure — a state. Machine-actionable per the dcrpay spec §6.
type PendingError struct {
	ID        string
	ExpiresAt time.Time
}

func (e *PendingError) Error() string {
	return fmt.Sprintf("pending_approval: id=%s expires=%s", e.ID, e.ExpiresAt.UTC().Format(time.RFC3339))
}

// ErrUnreachable reports that no configured channel accepted the
// notification — the escalation fails closed as denied.
var ErrUnreachable = fmt.Errorf("approve: no approval channel reachable — denied (fail-closed)")

// Registry tracks pending approvals and their continuations.
type Registry struct {
	st        store.Store
	now       func() time.Time
	approvers []Approver

	mu      sync.Mutex
	waiting map[string]Continuation
}

// NewRegistry builds a registry over the audit store.
func NewRegistry(st store.Store, now func() time.Time, approvers ...Approver) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{
		st:        st,
		now:       now,
		approvers: approvers,
		waiting:   map[string]Continuation{},
	}
}

// AddApprover registers another notification channel (e.g. Bison Relay,
// which needs the registry to exist first).
func (r *Registry) AddApprover(a Approver) {
	r.mu.Lock()
	r.approvers = append(r.approvers, a)
	r.mu.Unlock()
}

// Escalate parks a payment pending the owner's verdict and notifies the
// channel(s). It returns the created approval; the caller reports
// PendingError to the agent. If no channel is reachable the approval is
// immediately denied (fail-closed) and ErrUnreachable returned.
func (r *Registry) Escalate(ctx context.Context, attemptID int64, channel string, ttl time.Duration, summary string, cont Continuation) (store.Approval, error) {
	// 8 random bytes (64-bit): collision-safe over the table's lifetime and
	// infeasible to guess, while still short enough to copy from a PM.
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return store.Approval{}, err
	}
	now := r.now()
	approval := store.Approval{
		ID:        hex.EncodeToString(idBytes),
		AttemptID: attemptID,
		Channel:   channel,
		Status:    store.ApprovalPending,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	if err := r.st.CreateApproval(ctx, approval); err != nil {
		return store.Approval{}, err
	}
	r.mu.Lock()
	r.waiting[approval.ID] = cont
	r.mu.Unlock()

	req := Request{ID: approval.ID, Summary: summary, ExpiresAt: approval.ExpiresAt}
	notified := 0
	for _, a := range r.approvers {
		if channel != "both" && a.Name() != channel {
			continue
		}
		if err := a.Notify(ctx, req); err == nil {
			notified++
		}
	}
	if notified == 0 {
		// Fail closed: nobody heard about it, so nobody can approve it.
		r.mu.Lock()
		delete(r.waiting, approval.ID)
		r.mu.Unlock()
		_, _ = r.st.ResolveApproval(ctx, approval.ID, store.ApprovalDenied, "unreachable-channel", now)
		return store.Approval{}, ErrUnreachable
	}
	return approval, nil
}

// Resolve delivers the owner's verdict. Exactly one resolver wins; the
// continuation (when this process created the approval) runs
// asynchronously. Approvals surviving a restart have no continuation and
// therefore resolve on the books but never execute — fail-closed by
// construction.
func (r *Registry) Resolve(ctx context.Context, id string, approved bool, responder string) (bool, error) {
	status := store.ApprovalDenied
	if approved {
		status = store.ApprovalApproved
	}
	won, err := r.st.ResolveApproval(ctx, id, status, responder, r.now())
	if err != nil || !won {
		return won, err
	}
	r.mu.Lock()
	cont := r.waiting[id]
	delete(r.waiting, id)
	r.mu.Unlock()
	if cont != nil {
		go cont(context.WithoutCancel(ctx), approved, responder)
	}
	return true, nil
}

// Pending lists live approvals.
func (r *Registry) Pending(ctx context.Context) ([]store.Approval, error) {
	return r.st.ListPendingApprovals(ctx, r.now())
}

// Get returns one approval.
func (r *Registry) Get(ctx context.Context, id string) (store.Approval, error) {
	return r.st.GetApproval(ctx, id)
}

// RunExpiry expires overdue approvals (fail-closed deny) every interval
// until ctx ends. Run it as a goroutine.
func (r *Registry) RunExpiry(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			expired, err := r.st.ExpireApprovals(ctx, r.now())
			if err != nil {
				continue
			}
			for _, a := range expired {
				r.mu.Lock()
				cont := r.waiting[a.ID]
				delete(r.waiting, a.ID)
				r.mu.Unlock()
				if cont != nil {
					go cont(context.WithoutCancel(ctx), false, "ttl-expired")
				}
			}
		}
	}
}

// ExpireNow runs one expiry sweep immediately (tests, shutdown).
func (r *Registry) ExpireNow(ctx context.Context) {
	expired, err := r.st.ExpireApprovals(ctx, r.now())
	if err != nil {
		return
	}
	for _, a := range expired {
		r.mu.Lock()
		cont := r.waiting[a.ID]
		delete(r.waiting, a.ID)
		r.mu.Unlock()
		if cont != nil {
			cont(ctx, false, "ttl-expired")
		}
	}
}
