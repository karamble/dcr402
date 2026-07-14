package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"path"
	"strings"
)

// upstreamHandler builds the route handler: a reverse proxy wrapped by the
// price resolution appropriate to the upstream kind and mode. Gating
// middlewares are prebuilt per configured price entry — prices are static,
// so no per-request construction happens.
func (gw *Gateway) upstreamHandler(u *Upstream) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u.Target)
			pr.SetXForwarded()
		},
	}
	if u.MCP {
		return gw.mcpHandler(u, proxy)
	}

	entries := make(map[string]http.Handler, len(u.Prices))
	for key, atoms := range u.Prices {
		entries[key] = gw.priced(u.Mode, key, atoms, proxy)
	}
	var fallback http.Handler = proxy
	if u.PriceDefault > 0 {
		fallback = gw.priced(u.Mode, "default "+u.Route, u.PriceDefault, proxy)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Canonicalize the path for the price lookup so trailing-slash,
		// double-slash, and dot-segment variants cannot slip past a priced
		// route into the free fallback (the upstream would normalize them and
		// serve the paid resource).
		key := strings.ToUpper(r.Method) + " " + cleanPath(r.URL.Path)
		if h, ok := entries[key]; ok {
			h.ServeHTTP(w, r)
			return
		}
		fallback.ServeHTTP(w, r)
	})
}

// cleanPath returns the lexically-canonical request path used for price
// matching.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	return path.Clean(p)
}

// priced wraps next behind the configured payment mode.
func (gw *Gateway) priced(mode, tool string, atoms int64, next http.Handler) http.Handler {
	if mode == ModeCredits {
		return gw.gate.RequireCredits(tool, atoms)(next)
	}
	return gw.gate.Require(atoms)(next)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
