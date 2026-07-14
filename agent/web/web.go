// Package web is dcr402-agent's OPTIONAL dashboard: a JSON API plus a
// self-contained, no-build-step single-page UI (dark, JetBrains Mono) for
// status, the live attempt/payment feed, budget gauges, and approve/deny of
// pending approvals. It depends only on the agent Service — the core has no
// dependency on it — so an integrator mounts it, replaces it with their own
// UI over the same JSON API, or omits it entirely by not calling Handler.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/karamble/dcr402/agent"
)

//go:embed static
var staticFS embed.FS

// maxSSE bounds concurrent dashboard event streams.
const maxSSE = 64

var sseCount atomic.Int32

// Server serves the dashboard for a Service.
type Server struct {
	svc     *agent.Service
	network string
	// token authenticates every /api route (required).
	token string
	mux   *http.ServeMux
}

// Config wires the web server.
type Config struct {
	Service *agent.Service
	Network string
	// Token authenticates the ENTIRE JSON API (reads, the SSE stream, and
	// approve/deny). It is required: an unauthenticated API would let any
	// local process read balances/history and resolve the agent's own
	// escalations. The binary generates one if the operator does not set it.
	Token string
}

// New builds the web server. It fails closed: a Config without a Token is
// rejected, so the API is never served open.
func New(cfg Config) (*Server, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("web: Token is required (the dashboard API must be authenticated)")
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	s := &Server{svc: cfg.Service, network: cfg.Network, token: cfg.Token, mux: http.NewServeMux()}
	// The static SPA is public (it is just HTML/JS and carries no token); the
	// JSON API under /api is authenticated on every route.
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
	s.mux.HandleFunc("/api/status", s.guard(s.handleStatus))
	s.mux.HandleFunc("/api/attempts", s.guard(s.handleAttempts))
	s.mux.HandleFunc("/api/payments", s.guard(s.handlePayments))
	s.mux.HandleFunc("/api/approvals", s.guard(s.handleApprovals))
	s.mux.HandleFunc("/api/approvals/resolve", s.guard(s.handleResolve))
	s.mux.HandleFunc("/api/freeze", s.guard(s.handleFreeze))
	s.mux.HandleFunc("/api/events", s.guard(s.handleEvents))
	return s, nil
}

// guard rejects any /api request without the bearer token.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// Handler returns the dashboard handler. Mount it wherever you like (the
// binary mounts it at "/"); do not mount it and the UI simply does not
// exist.
func (s *Server) Handler() http.Handler { return securityHeaders(s.mux) }

// securityHeaders adds a strict Content-Security-Policy (and companions) to
// every response. `script-src 'self'` blocks inline event-handler injection,
// so even if an agent-controlled field reached the DOM unescaped it could not
// run script or exfiltrate over the network.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self'; base-uri 'none'; "+
				"frame-ancestors 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.Status(r.Context(), s.network)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func limitParam(r *http.Request) (int, int64) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, _ := strconv.ParseInt(r.URL.Query().Get("before_id"), 10, 64)
	return limit, before
}

func (s *Server) handleAttempts(w http.ResponseWriter, r *http.Request) {
	limit, before := limitParam(r)
	list, err := s.svc.Attempts(r.Context(), limit, before)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handlePayments(w http.ResponseWriter, r *http.Request) {
	limit, before := limitParam(r)
	list, err := s.svc.Payments(r.Context(), limit, before)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.PendingApprovals(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleResolve approves or denies a pending approval (authenticated by the
// guard wrapper).
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID       string `json:"id"`
		Approved bool   `json:"approved"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id and approved are required"})
		return
	}
	won, err := s.svc.ResolveApproval(r.Context(), body.ID, body.Approved, "web")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !won {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "already resolved or expired"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
}

// handleFreeze trips or clears the kill-switch (authenticated by the guard).
// This is the owner-only unfreeze path: it is not exposed to MCP, so the
// driving agent cannot resume its own spending.
func (s *Server) handleFreeze(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Frozen bool   `json:"frozen"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "frozen is required"})
		return
	}
	var err error
	if body.Frozen {
		err = s.svc.Freeze(r.Context(), "dashboard", body.Reason)
	} else {
		err = s.svc.Unfreeze(r.Context(), "dashboard")
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"frozen": body.Frozen})
}

// authorized checks the bearer token, taken from the Authorization header
// (fetch) or a `token` query parameter (EventSource cannot set headers). The
// comparison is constant-time. An empty configured token never authorizes.
func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	presented := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		presented = strings.TrimPrefix(h, "Bearer ")
	} else {
		presented = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1
}

// handleEvents streams status + pending approvals as Server-Sent Events so
// the dashboard updates live without polling churn.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if n := sseCount.Add(1); n > maxSSE {
		sseCount.Add(-1)
		http.Error(w, "too many event streams", http.StatusServiceUnavailable)
		return
	}
	defer sseCount.Add(-1)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	send := func() bool {
		payload := s.snapshot(ctx)
		raw, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

func (s *Server) snapshot(ctx context.Context) map[string]any {
	out := map[string]any{}
	if st, err := s.svc.Status(ctx, s.network); err == nil {
		out["status"] = st
	}
	if ap, err := s.svc.PendingApprovals(ctx); err == nil {
		out["approvals"] = ap
	}
	if att, err := s.svc.Attempts(ctx, 12, 0); err == nil {
		out["attempts"] = att
	}
	return out
}
