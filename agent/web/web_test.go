package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewRequiresToken checks that the dashboard fails closed: no token, no server.
func TestNewRequiresToken(t *testing.T) {
	if _, err := New(Config{Token: ""}); err == nil {
		t.Fatal("New must reject an empty token (the API cannot be unauthenticated)")
	}
}

// TestAPIRequiresToken checks that every /api route rejects an unauthenticated
// request. The guard runs before any handler, so a nil Service is never
// reached on these paths.
func TestAPIRequiresToken(t *testing.T) {
	srv, err := New(Config{Token: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{
		"/api/status", "/api/approvals", "/api/payments",
		"/api/attempts", "/api/events",
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s without token: status %d, want 401", path, resp.StatusCode)
		}
	}

	// The resolve endpoint rejects unauthenticated POSTs.
	resp, err := http.Post(ts.URL+"/api/approvals/resolve", "application/json",
		strings.NewReader(`{"id":"abc","approved":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("resolve without token: status %d, want 401", resp.StatusCode)
	}

	// A wrong token is rejected.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/approvals", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	bad, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: status %d, want 401", bad.StatusCode)
	}
}
