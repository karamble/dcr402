// Package store defines the persistence contract for dcr402 gates —
// challenge records, consume-once settlements, and L402 root keys — with a
// SQLite implementation for production and an in-memory one for tests.
//
// The store is what makes the scheme's replay rules real: settlement
// consumption is atomic (exactly one first presentation per payment hash)
// and root-key deletion is credential revocation. Records must be retained
// at least as long as any credential minted against them remains valid.
package store

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned by lookups that miss.
var ErrNotFound = errors.New("store: not found")

// Challenge is the issuer-side record of one 402 challenge.
type Challenge struct {
	// PaymentHash keys the challenge (the invoice's payment hash).
	PaymentHash [32]byte
	// Invoice is the BOLT11 string offered in the challenge.
	Invoice string
	// MacaroonB64 is the credential minted at challenge time (annex §3).
	MacaroonB64 string
	// RootKeyID is sha256(macaroon identifier) — the root-key lookup key.
	RootKeyID [32]byte
	// Requirements is the canonical JSON of the offered accepts[] entry;
	// verification rule 1 compares the client's echo against it.
	Requirements []byte
	// Resource is the resource URL the challenge was issued for.
	Resource string
	// AmountAtoms is the offered amount.
	AmountAtoms int64
	// CreatedAt/ExpiresAt bound the challenge's lifetime (rule 7).
	CreatedAt time.Time
	ExpiresAt time.Time
}

// OnchainChallenge is the issuer-side record of one onchain challenge,
// keyed by its unique per-challenge deposit address.
type OnchainChallenge struct {
	// Address is the fresh deposit address (also the entry's payTo).
	Address string
	// Requirements is the canonical JSON of the offered accepts[] entry.
	Requirements []byte
	// Resource is the resource URL the challenge was issued for.
	Resource string
	// AmountAtoms is the exact amount the transaction must pay.
	AmountAtoms int64
	// Confirmations is the required depth (verification rule 11).
	Confirmations int32
	// MacaroonB64 is the credential minted at challenge time.
	MacaroonB64 string
	// RootKeyID is sha256(macaroon identifier).
	RootKeyID [32]byte
	// CredentialPreimage is the server-generated bearer secret (hex)
	// whose SHA-256 is the credential macaroon's identifier hash —
	// on-chain payments have no Lightning preimage, so the server makes
	// one and delivers it at settlement.
	CredentialPreimage string
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// Store is the persistence interface a Gate requires. Implementations must
// be safe for concurrent use.
type Store interface {
	// PutChallenge records a freshly issued challenge.
	PutChallenge(ctx context.Context, c Challenge) error
	// GetChallenge fetches a challenge by payment hash.
	GetChallenge(ctx context.Context, paymentHash [32]byte) (Challenge, error)

	// PutOnchainChallenge records a freshly issued onchain challenge.
	PutOnchainChallenge(ctx context.Context, c OnchainChallenge) error
	// GetOnchainChallenge fetches an onchain challenge by deposit address.
	GetOnchainChallenge(ctx context.Context, address string) (OnchainChallenge, error)

	// Settle atomically records the first settlement of paymentHash and
	// returns (response, false, nil). If the hash was already consumed it
	// returns the originally stored response and already=true — the
	// idempotent re-presentation path of verification rule 8.
	Settle(ctx context.Context, paymentHash [32]byte, response []byte, at time.Time) (stored []byte, already bool, err error)

	// Settled reports whether paymentHash was already consumed by a prior
	// settlement, recording nothing. It lets verification distinguish a
	// genuinely expired challenge from a proof re-presented past its window
	// (invoice_expired vs payment_hash_reused).
	Settled(ctx context.Context, paymentHash [32]byte) (bool, error)

	// PutRootKey stores an L402 root key under sha256(identifier).
	PutRootKey(ctx context.Context, keyID [32]byte, rootKey [32]byte) error
	// GetRootKey fetches a root key; ErrNotFound means never minted or
	// revoked.
	GetRootKey(ctx context.Context, keyID [32]byte) ([32]byte, error)
	// DeleteRootKey revokes a credential (annex §7). Deleting a missing
	// key is not an error.
	DeleteRootKey(ctx context.Context, keyID [32]byte) error

	Close() error
}

// Memory is an in-memory Store for tests and ephemeral gates.
type Memory struct {
	mu          sync.Mutex
	challenges  map[[32]byte]Challenge
	onchain     map[string]OnchainChallenge
	settlements map[[32]byte][]byte
	rootKeys    map[[32]byte][32]byte
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		challenges:  make(map[[32]byte]Challenge),
		onchain:     make(map[string]OnchainChallenge),
		settlements: make(map[[32]byte][]byte),
		rootKeys:    make(map[[32]byte][32]byte),
	}
}

func (m *Memory) PutOnchainChallenge(_ context.Context, c OnchainChallenge) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onchain[c.Address] = c
	return nil
}

func (m *Memory) GetOnchainChallenge(_ context.Context, address string) (OnchainChallenge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.onchain[address]
	if !ok {
		return OnchainChallenge{}, ErrNotFound
	}
	return c, nil
}

func (m *Memory) PutChallenge(_ context.Context, c Challenge) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.challenges[c.PaymentHash] = c
	return nil
}

func (m *Memory) GetChallenge(_ context.Context, hash [32]byte) (Challenge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.challenges[hash]
	if !ok {
		return Challenge{}, ErrNotFound
	}
	return c, nil
}

func (m *Memory) Settle(_ context.Context, hash [32]byte, response []byte, _ time.Time) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.settlements[hash]; ok {
		return prev, true, nil
	}
	stored := append([]byte(nil), response...)
	m.settlements[hash] = stored
	return stored, false, nil
}

func (m *Memory) Settled(_ context.Context, hash [32]byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.settlements[hash]
	return ok, nil
}

func (m *Memory) PutRootKey(_ context.Context, keyID [32]byte, rootKey [32]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rootKeys[keyID] = rootKey
	return nil
}

func (m *Memory) GetRootKey(_ context.Context, keyID [32]byte) ([32]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.rootKeys[keyID]
	if !ok {
		return [32]byte{}, ErrNotFound
	}
	return k, nil
}

func (m *Memory) DeleteRootKey(_ context.Context, keyID [32]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rootKeys, keyID)
	return nil
}

func (m *Memory) Close() error { return nil }

var _ Store = (*Memory)(nil)
