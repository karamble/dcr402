package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// stores under test: constructor per implementation.
func implementations(t *testing.T) map[string]Store {
	t.Helper()
	sq, err := OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { sq.Close() })
	return map[string]Store{
		"memory": NewMemory(),
		"sqlite": sq,
	}
}

func TestChallengeRoundTrip(t *testing.T) {
	for name, s := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var hash, keyID [32]byte
			hash[0], keyID[0] = 1, 2
			c := Challenge{
				PaymentHash:  hash,
				Invoice:      "lndcr1...",
				MacaroonB64:  "AgEC",
				RootKeyID:    keyID,
				Requirements: []byte(`{"scheme":"exact"}`),
				Resource:     "https://api.example.com/x",
				AmountAtoms:  250000,
				CreatedAt:    time.Unix(1750000000, 0),
				ExpiresAt:    time.Unix(1750003600, 0),
			}
			if err := s.PutChallenge(ctx, c); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetChallenge(ctx, hash)
			if err != nil {
				t.Fatal(err)
			}
			if got.Invoice != c.Invoice || got.AmountAtoms != c.AmountAtoms ||
				got.RootKeyID != c.RootKeyID || string(got.Requirements) != string(c.Requirements) ||
				!got.ExpiresAt.Equal(c.ExpiresAt) {
				t.Fatalf("challenge round-trip mismatch: %+v", got)
			}
			if _, err := s.GetChallenge(ctx, [32]byte{9}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing challenge: err=%v", err)
			}
		})
	}
}

func TestSettleConsumeOnce(t *testing.T) {
	for name, s := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var hash [32]byte
			hash[0] = 3
			first, already, err := s.Settle(ctx, hash, []byte("resp-1"), time.Now())
			if err != nil || already || string(first) != "resp-1" {
				t.Fatalf("first settle: %q already=%v err=%v", first, already, err)
			}
			second, already, err := s.Settle(ctx, hash, []byte("resp-2"), time.Now())
			if err != nil || !already {
				t.Fatalf("second settle: already=%v err=%v", already, err)
			}
			if string(second) != "resp-1" {
				t.Fatalf("second settle returned %q, want original", second)
			}
		})
	}
}

// TestSettleRace: many concurrent presentations of one hash — exactly one
// wins, everyone converges on the winner's response.
func TestSettleRace(t *testing.T) {
	for name, s := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var hash [32]byte
			hash[0] = 4
			const n = 16
			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				wins int
				got  = map[string]int{}
			)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					resp := []byte{byte('a' + i)}
					stored, already, err := s.Settle(ctx, hash, resp, time.Now())
					if err != nil {
						t.Errorf("settle: %v", err)
						return
					}
					mu.Lock()
					defer mu.Unlock()
					if !already {
						wins++
					}
					got[string(stored)]++
				}(i)
			}
			wg.Wait()
			if wins != 1 {
				t.Fatalf("%d winners, want exactly 1", wins)
			}
			if len(got) != 1 {
				t.Fatalf("divergent stored responses: %v", got)
			}
		})
	}
}

func TestRootKeyLifecycle(t *testing.T) {
	for name, s := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			var keyID, rootKey [32]byte
			keyID[0], rootKey[0] = 5, 6
			if err := s.PutRootKey(ctx, keyID, rootKey); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetRootKey(ctx, keyID)
			if err != nil || got != rootKey {
				t.Fatalf("root key round-trip: %v %v", got, err)
			}
			// Revocation.
			if err := s.DeleteRootKey(ctx, keyID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetRootKey(ctx, keyID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("revoked key still present: err=%v", err)
			}
			// Deleting again is not an error.
			if err := s.DeleteRootKey(ctx, keyID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSQLitePersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "persist.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	var hash [32]byte
	hash[0] = 7
	if _, _, err := s.Settle(ctx, hash, []byte("durable"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	stored, already, err := s2.Settle(ctx, hash, []byte("other"), time.Now())
	if err != nil || !already || string(stored) != "durable" {
		t.Fatalf("settlement not durable across reopen: %q already=%v err=%v",
			stored, already, err)
	}
}
