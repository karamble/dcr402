// Package ledger is the dcr402 credit ledger: prepaid, atoms-denominated
// balances that let the slow rail fund the fast one — on-chain (or
// Lightning) top-ups grant credits, and gated calls burn them with zero
// payment latency.
//
// Accounts are identified by opaque string ids; by dcr402 convention the id
// is the hex L402 token_id, which survives credential rotation
// (scheme/l402-dual-envelope.md §7). The ledger is deliberately minimal:
// idempotent grants, atomic charges that never go negative, balances.
// Anything richer (fiat denominations, trials, holds for async jobs) is a
// consumer concern layered on top.
package ledger

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// InsufficientBalanceError reports a rejected charge with the exact
// shortfall — gates surface it machine-actionably.
type InsufficientBalanceError struct {
	Balance  int64
	Required int64
}

func (e *InsufficientBalanceError) Error() string {
	return fmt.Sprintf("ledger: insufficient balance: have %d atoms, need %d",
		e.Balance, e.Required)
}

// Shortfall is the missing amount in atoms.
func (e *InsufficientBalanceError) Shortfall() int64 { return e.Required - e.Balance }

// Ledger is the persistence contract. Implementations must be safe for
// concurrent use.
type Ledger interface {
	// Grant credits amountAtoms to the account, creating it if needed.
	// externalRef is the settlement's unique reference (payment hash,
	// txid) — a repeated ref is answered idempotently (already=true, no
	// double credit). receipt is the stored settlement receipt JSON.
	Grant(ctx context.Context, accountID, rail string, amountAtoms int64, externalRef string, receipt []byte, at time.Time) (balance int64, already bool, err error)

	// Charge atomically debits amountAtoms: balance check, decrement, and
	// usage event in one transaction; balances never go negative. On
	// rejection the error is *InsufficientBalanceError. idemKey is an optional
	// caller idempotency key: when non-empty, a repeat charge with the same
	// (account, idemKey) returns the current balance without debiting again, so
	// a retry is charged at most once. An empty idemKey always charges (no
	// dedup). requestRef is a free-form audit reference.
	Charge(ctx context.Context, accountID, tool string, amountAtoms int64, requestRef, idemKey string, at time.Time) (balance int64, err error)

	// Balance returns the current balance; unknown accounts are 0.
	Balance(ctx context.Context, accountID string) (int64, error)

	Close() error
}

// Memory is an in-memory Ledger for tests and ephemeral gates.
type Memory struct {
	mu       sync.Mutex
	balances map[string]int64
	grants   map[string]grantRecord
	// charged records (accountID\x00idemKey) already debited, for the caller
	// idempotency key (empty keys are never recorded).
	charged map[string]bool
}

type grantRecord struct {
	accountID string
	amount    int64
}

// NewMemory returns an empty in-memory ledger.
func NewMemory() *Memory {
	return &Memory{
		balances: make(map[string]int64),
		grants:   make(map[string]grantRecord),
		charged:  make(map[string]bool),
	}
}

func (m *Memory) Grant(_ context.Context, accountID, _ string, amountAtoms int64, externalRef string, _ []byte, _ time.Time) (int64, bool, error) {
	if amountAtoms <= 0 {
		return 0, false, fmt.Errorf("ledger: grant amount must be positive")
	}
	if externalRef == "" {
		return 0, false, fmt.Errorf("ledger: externalRef is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.grants[externalRef]; ok {
		return m.balances[prev.accountID], true, nil
	}
	m.grants[externalRef] = grantRecord{accountID: accountID, amount: amountAtoms}
	m.balances[accountID] += amountAtoms
	return m.balances[accountID], false, nil
}

func (m *Memory) Charge(_ context.Context, accountID, _ string, amountAtoms int64, _, idemKey string, _ time.Time) (int64, error) {
	if amountAtoms <= 0 {
		return 0, fmt.Errorf("ledger: charge amount must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if idemKey != "" && m.charged[accountID+"\x00"+idemKey] {
		// Already charged under this key: idempotent replay, no second debit.
		return m.balances[accountID], nil
	}
	balance := m.balances[accountID]
	if balance < amountAtoms {
		return balance, &InsufficientBalanceError{Balance: balance, Required: amountAtoms}
	}
	m.balances[accountID] = balance - amountAtoms
	if idemKey != "" {
		m.charged[accountID+"\x00"+idemKey] = true
	}
	return m.balances[accountID], nil
}

func (m *Memory) Balance(_ context.Context, accountID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.balances[accountID], nil
}

func (m *Memory) Close() error { return nil }

var _ Ledger = (*Memory)(nil)
