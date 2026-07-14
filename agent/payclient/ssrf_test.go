package payclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGuardedClientBlocksInternal checks that fetch_paid's transport refuses
// internal targets while still reaching allow-listed ones.
func TestGuardedClientBlocksInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://") // loopback host:port

	t.Run("loopback refused by default", func(t *testing.T) {
		c := GuardedClient("", nil, 5*time.Second)
		if resp, err := c.Get(srv.URL); err == nil {
			resp.Body.Close()
			t.Fatal("expected a loopback fetch to be refused")
		}
	})

	t.Run("allow-listed loopback succeeds", func(t *testing.T) {
		c := GuardedClient("", []string{addr}, 5*time.Second)
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatalf("allow-listed loopback should succeed: %v", err)
		}
		resp.Body.Close()
	})

	t.Run("own address refused even when allow-listed", func(t *testing.T) {
		c := GuardedClient(addr, []string{addr}, 5*time.Second)
		if resp, err := c.Get(srv.URL); err == nil {
			resp.Body.Close()
			t.Fatal("expected the agent's own address to be refused")
		}
	})
}
