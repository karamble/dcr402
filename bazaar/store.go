package bazaar

import (
	"context"
	"sync"
	"time"
)

// Store persists notarized settlements. It is extended by discovery.go with
// the resource index when discovery is enabled. All methods are safe for
// concurrent use.
type Store interface {
	// Settle records a settlement notarization idempotently under key (the
	// lightning payment hash, hex). The first call stores response and
	// receipt and returns (response, false, nil); later calls return the
	// stored response and true without overwriting (the facilitator moves no
	// funds, so this is pure bookkeeping).
	Settle(ctx context.Context, key string, response, receipt []byte, at time.Time) (stored []byte, already bool, err error)
	// GetSettlement returns a previously notarized response by key, so a
	// re-presented settle is idempotent even after the invoice expires.
	GetSettlement(ctx context.Context, key string) (response []byte, ok bool, err error)
	// PutResource upserts a discovery-index entry, keyed by resource URL.
	PutResource(ctx context.Context, r Resource) error
	// Resources returns every indexed resource; the caller filters and
	// paginates (the index is per-instance and modest).
	Resources(ctx context.Context) ([]Resource, error)
	// Close releases resources.
	Close() error
}

// Memory is an in-memory Store for tests and ephemeral instances (state is
// lost on restart).
type Memory struct {
	mu          sync.Mutex
	settlements map[string][]byte
	resources   map[string]Resource
}

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory {
	return &Memory{
		settlements: make(map[string][]byte),
		resources:   make(map[string]Resource),
	}
}

// Settle implements Store.
func (m *Memory) Settle(_ context.Context, key string, response, _ []byte, _ time.Time) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if stored, ok := m.settlements[key]; ok {
		return stored, true, nil
	}
	m.settlements[key] = response
	return response, false, nil
}

// GetSettlement implements Store.
func (m *Memory) GetSettlement(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.settlements[key]
	return stored, ok, nil
}

// PutResource implements Store.
func (m *Memory) PutResource(_ context.Context, r Resource) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resources[r.URL] = r
	return nil
}

// Resources implements Store.
func (m *Memory) Resources(_ context.Context) ([]Resource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Resource, 0, len(m.resources))
	for _, r := range m.resources {
		out = append(out, r)
	}
	return out, nil
}

// Close implements Store.
func (m *Memory) Close() error { return nil }
