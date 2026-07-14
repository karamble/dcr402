// Package store is dcr402-agent's audit ledger and state: every payment
// attempt with its full policy decision trace (recorded BEFORE any
// signing), completed payments with receipts, the L402 credential cache,
// approval lifecycle, and small daemon state. Budget usage is derived from
// the payments table with time-bounded queries — a single source of truth,
// no mutable window counters to drift.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned by lookups that miss.
var ErrNotFound = errors.New("store: not found")

// Decision is a policy outcome.
type Decision string

const (
	DecisionAllow    Decision = "allow"
	DecisionDeny     Decision = "deny"
	DecisionEscalate Decision = "escalate"
)

// Approval statuses.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"
)

// Attempt is one policy-evaluated payment attempt (immutable once written).
// JSON field names are snake_case for the web/MCP API; RuleTrace is
// json.RawMessage so it embeds as a nested array, not a base64 string.
type Attempt struct {
	ID          int64           `json:"id"`
	Tool        string          `json:"tool"`
	Rail        string          `json:"rail"`
	Destination string          `json:"destination"`
	AmountAtoms int64           `json:"amount_atoms"`
	Memo        string          `json:"memo"`
	Decision    Decision        `json:"decision"`
	RuleTrace   json.RawMessage `json:"rule_trace"` // JSON []policy.RuleResult
	CreatedAt   time.Time       `json:"created_at"`
}

// Payment is a completed (successful) payment.
type Payment struct {
	ID          int64           `json:"id"`
	AttemptID   int64           `json:"attempt_id"`
	Rail        string          `json:"rail"`
	AmountAtoms int64           `json:"amount_atoms"`
	FeeAtoms    int64           `json:"fee_atoms"`
	Invoice     string          `json:"invoice,omitempty"`
	Preimage    string          `json:"preimage,omitempty"`
	TxID        string          `json:"txid,omitempty"`
	L402Service string          `json:"l402_service,omitempty"`
	Receipt     json.RawMessage `json:"receipt"` // JSON receipt per the scheme
	CreatedAt   time.Time       `json:"created_at"`
}

// Token is a cached L402 credential for a service host.
type Token struct {
	ServiceHost   string    `json:"service_host"`
	Authorization string    `json:"authorization"` // complete Authorization header value
	ValidUntil    int64     `json:"valid_until"`   // unix; 0 = unknown
	PaidAtoms     int64     `json:"paid_atoms"`
	CreatedAt     time.Time `json:"created_at"`
	LastUsed      time.Time `json:"last_used"`
}

// Approval is one escalation awaiting (or past) the owner's verdict.
type Approval struct {
	ID         string    `json:"id"` // short hex, owner-typable
	AttemptID  int64     `json:"attempt_id"`
	Channel    string    `json:"channel"`
	Status     string    `json:"status"`
	Responder  string    `json:"responder,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	ResolvedAt time.Time `json:"resolved_at,omitempty"` // zero until resolved
}

// Usage is the current budget consumption view.
type Usage struct {
	DayAtoms  int64 // spent since UTC midnight
	WeekAtoms int64 // spent since ISO week start (Monday 00:00 UTC)
	HourCount int64 // successful payments in the rolling last hour
}

// DayStart returns the UTC calendar-day start for t.
func DayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// WeekStart returns the ISO week start (Monday 00:00 UTC) for t.
func WeekStart(t time.Time) time.Time {
	d := DayStart(t)
	wd := int(d.Weekday())
	if wd == 0 { // Sunday
		wd = 7
	}
	return d.AddDate(0, 0, -(wd - 1))
}

// Store is the persistence contract. Implementations must be safe for
// concurrent use.
type Store interface {
	// RecordAttempt persists the attempt + trace BEFORE any signing.
	RecordAttempt(ctx context.Context, a Attempt) (int64, error)
	GetAttempt(ctx context.Context, id int64) (Attempt, error)
	// ListAttempts returns newest-first, up to limit, with id < beforeID
	// (0 = from newest).
	ListAttempts(ctx context.Context, limit int, beforeID int64) ([]Attempt, error)

	RecordPayment(ctx context.Context, p Payment) (int64, error)
	GetPayment(ctx context.Context, id int64) (Payment, error)
	ListPayments(ctx context.Context, limit int, beforeID int64) ([]Payment, error)
	// Usage derives budget consumption from payments at time now.
	Usage(ctx context.Context, now time.Time) (Usage, error)

	PutToken(ctx context.Context, t Token) error
	GetToken(ctx context.Context, serviceHost string) (Token, error)
	TouchToken(ctx context.Context, serviceHost string, at time.Time) error
	DeleteToken(ctx context.Context, serviceHost string) error

	CreateApproval(ctx context.Context, a Approval) error
	GetApproval(ctx context.Context, id string) (Approval, error)
	// ResolveApproval flips a PENDING, unexpired approval to status; ok
	// reports whether this call won (exactly one resolver wins).
	ResolveApproval(ctx context.Context, id, status, responder string, at time.Time) (bool, error)
	ListPendingApprovals(ctx context.Context, now time.Time) ([]Approval, error)
	// ExpireApprovals marks pending approvals past their TTL as expired
	// and returns them (for fail-closed bookkeeping).
	ExpireApprovals(ctx context.Context, now time.Time) ([]Approval, error)

	// GetState/SetState hold small daemon state (e.g. the cumulative
	// Bison Relay PMStream ack sequence under key "br_ack").
	GetState(ctx context.Context, key string) (string, error)
	SetState(ctx context.Context, key, value string) error

	Close() error
}

// Memory is an in-memory Store for tests.
type Memory struct {
	mu        sync.Mutex
	attempts  []Attempt
	payments  []Payment
	tokens    map[string]Token
	approvals map[string]Approval
	state     map[string]string
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		tokens:    map[string]Token{},
		approvals: map[string]Approval{},
		state:     map[string]string{},
	}
}

func (m *Memory) RecordAttempt(_ context.Context, a Attempt) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a.ID = int64(len(m.attempts) + 1)
	m.attempts = append(m.attempts, a)
	return a.ID, nil
}

func (m *Memory) GetAttempt(_ context.Context, id int64) (Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || int(id) > len(m.attempts) {
		return Attempt{}, ErrNotFound
	}
	return m.attempts[id-1], nil
}

func (m *Memory) ListAttempts(_ context.Context, limit int, beforeID int64) ([]Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Attempt
	for i := len(m.attempts) - 1; i >= 0 && len(out) < limit; i-- {
		a := m.attempts[i]
		if beforeID > 0 && a.ID >= beforeID {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (m *Memory) RecordPayment(_ context.Context, p Payment) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p.ID = int64(len(m.payments) + 1)
	m.payments = append(m.payments, p)
	return p.ID, nil
}

func (m *Memory) GetPayment(_ context.Context, id int64) (Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id < 1 || int(id) > len(m.payments) {
		return Payment{}, ErrNotFound
	}
	return m.payments[id-1], nil
}

func (m *Memory) ListPayments(_ context.Context, limit int, beforeID int64) ([]Payment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Payment
	for i := len(m.payments) - 1; i >= 0 && len(out) < limit; i-- {
		p := m.payments[i]
		if beforeID > 0 && p.ID >= beforeID {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (m *Memory) Usage(_ context.Context, now time.Time) (Usage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	day, week := DayStart(now), WeekStart(now)
	hourAgo := now.Add(-time.Hour)
	var u Usage
	for _, p := range m.payments {
		if !p.CreatedAt.Before(day) {
			u.DayAtoms += p.AmountAtoms
		}
		if !p.CreatedAt.Before(week) {
			u.WeekAtoms += p.AmountAtoms
		}
		if p.CreatedAt.After(hourAgo) {
			u.HourCount++
		}
	}
	return u, nil
}

func (m *Memory) PutToken(_ context.Context, t Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[t.ServiceHost] = t
	return nil
}

func (m *Memory) GetToken(_ context.Context, host string) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[host]
	if !ok {
		return Token{}, ErrNotFound
	}
	return t, nil
}

func (m *Memory) TouchToken(_ context.Context, host string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tokens[host]; ok {
		t.LastUsed = at
		m.tokens[host] = t
	}
	return nil
}

func (m *Memory) DeleteToken(_ context.Context, host string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tokens, host)
	return nil
}

func (m *Memory) CreateApproval(_ context.Context, a Approval) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.approvals[a.ID] = a
	return nil
}

func (m *Memory) GetApproval(_ context.Context, id string) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return Approval{}, ErrNotFound
	}
	return a, nil
}

func (m *Memory) ResolveApproval(_ context.Context, id, status, responder string, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok || a.Status != ApprovalPending || at.After(a.ExpiresAt) {
		return false, nil
	}
	a.Status = status
	a.Responder = responder
	a.ResolvedAt = at
	m.approvals[id] = a
	return true, nil
}

func (m *Memory) ListPendingApprovals(_ context.Context, now time.Time) ([]Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Approval
	for _, a := range m.approvals {
		if a.Status == ApprovalPending && !now.After(a.ExpiresAt) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) ExpireApprovals(_ context.Context, now time.Time) ([]Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Approval
	for id, a := range m.approvals {
		if a.Status == ApprovalPending && now.After(a.ExpiresAt) {
			a.Status = ApprovalExpired
			a.ResolvedAt = now
			m.approvals[id] = a
			out = append(out, a)
		}
	}
	return out, nil
}

func (m *Memory) GetState(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.state[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (m *Memory) SetState(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state[key] = value
	return nil
}

func (m *Memory) Close() error { return nil }

var _ Store = (*Memory)(nil)
