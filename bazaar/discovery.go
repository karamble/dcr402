package bazaar

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/karamble/dcr402/lib/x402"
)

// extensionBazaar is the x402 extension key for the discovery index; it is
// advertised in /supported when discovery is enabled.
const extensionBazaar = "bazaar"

const (
	// maxPageSize caps a discovery page so an unauthenticated read can never
	// force the whole index to be filtered and serialized at once.
	maxPageSize = 100
	// maxResources caps the index so unauthenticated submit spam cannot grow
	// it (and the per-read O(N) work) without bound.
	maxResources = 10_000
)

// clampPageSize bounds a requested page limit to [1, maxPageSize].
func clampPageSize(limit int) int {
	if limit <= 0 || limit > maxPageSize {
		return maxPageSize
	}
	return limit
}

// indexFull reports whether a new url would exceed the index cap (updating an
// existing entry is always allowed).
func (s *Service) indexFull(ctx context.Context, url string) bool {
	all, err := s.store.Resources(ctx)
	if err != nil || len(all) < maxResources {
		return false
	}
	for _, x := range all {
		if x.URL == url {
			return false
		}
	}
	return true
}

// Resource is one entry in the discovery index: what a seller submits, keyed
// by URL. It is rendered into the Bazaar DiscoveryResource envelope on read.
type Resource struct {
	URL         string                     // resource url (primary key)
	Type        string                     // "http" or "mcp"
	Accepts     []x402.PaymentRequirements // the resource's accepts[] entries
	Description string
	MimeType    string
	ServiceName string
	Tags        []string
	IconURL     string
	Networks    []string  // CAIP-2 ids across accepts, for network filtering
	LastUpdated time.Time // upsert time
}

// discoveryResource is the Bazaar wire item (x402-foundation/x402/go/
// extensions/bazaar: DiscoveryResource).
type discoveryResource struct {
	Resource    string            `json:"resource"`
	Type        string            `json:"type"`
	X402Version int               `json:"x402Version"`
	Accepts     []json.RawMessage `json:"accepts"`
	LastUpdated string            `json:"lastUpdated"`
	Description string            `json:"description,omitempty"`
	MimeType    string            `json:"mimeType,omitempty"`
	ServiceName string            `json:"serviceName,omitempty"`
	Tags        []string          `json:"tags,omitempty"`
	IconURL     string            `json:"iconUrl,omitempty"`
}

type pagination struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

// listResourcesResponse is the GET /discovery/resources body.
type listResourcesResponse struct {
	X402Version int                 `json:"x402Version"`
	Items       []discoveryResource `json:"items"`
	Pagination  pagination          `json:"pagination"`
}

// searchResourcesResponse is the GET /discovery/search body.
type searchResourcesResponse struct {
	X402Version int                 `json:"x402Version"`
	Resources   []discoveryResource `json:"resources"`
}

// resourceFilter is the intersection of the list and search query params.
type resourceFilter struct {
	Type    string
	PayTo   string
	Scheme  string
	Network string
	Query   string // search only: case-insensitive substring
}

// registerDiscovery adds the discovery routes when the index is enabled.
func (s *Service) registerDiscovery(mux *http.ServeMux) {
	if !s.cfg.Discovery.Enabled {
		return
	}
	mux.HandleFunc("GET /discovery/resources", s.handleListResources)
	mux.HandleFunc("GET /discovery/search", s.handleSearchResources)
	mux.HandleFunc("POST /discovery/submit", s.gatedSubmit(s.handleSubmit))
}

// gatedSubmit gates submissions: open when public_submit is set, otherwise it
// requires an API key. It fails closed - with no keys and public_submit off,
// submit is disabled rather than left open.
func (s *Service) gatedSubmit(next http.HandlerFunc) http.HandlerFunc {
	if s.cfg.Discovery.PublicSubmit {
		return next
	}
	if len(s.keys) == 0 {
		return func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "submit is disabled"})
		}
	}
	return s.gated(next)
}

func (s *Service) handleListResources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := resourceFilter{
		Type:    q.Get("type"),
		PayTo:   q.Get("payTo"),
		Scheme:  q.Get("scheme"),
		Network: q.Get("network"),
	}
	all, err := s.store.Resources(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	matched := filterResources(all, filter)
	total := len(matched)
	offset := atoiDefault(q.Get("offset"), 0)
	limit := clampPageSize(atoiDefault(q.Get("limit"), 0))
	page := paginate(matched, offset, limit)

	items := make([]discoveryResource, 0, len(page))
	for _, res := range page {
		items = append(items, toDiscoveryResource(res))
	}
	writeJSON(w, http.StatusOK, listResourcesResponse{
		X402Version: x402.Version,
		Items:       items,
		Pagination:  pagination{Limit: limit, Offset: offset, Total: total},
	})
}

func (s *Service) handleSearchResources(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := resourceFilter{
		Type:    q.Get("type"),
		PayTo:   q.Get("payTo"),
		Scheme:  q.Get("scheme"),
		Network: q.Get("network"),
		Query:   q.Get("query"),
	}
	all, err := s.store.Resources(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	matched := filterResources(all, filter)
	matched = paginate(matched, 0, clampPageSize(atoiDefault(q.Get("limit"), 0)))

	items := make([]discoveryResource, 0, len(matched))
	for _, res := range matched {
		items = append(items, toDiscoveryResource(res))
	}
	writeJSON(w, http.StatusOK, searchResourcesResponse{
		X402Version: x402.Version,
		Resources:   items,
	})
}

func (s *Service) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req SubmitRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	res, err := req.toResource(s.now())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if s.indexFull(r.Context(), res.URL) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "discovery index is full"})
		return
	}
	if err := s.store.PutResource(r.Context(), res); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "indexed", "resource": res.URL})
}

// filterResources returns, in stable URL order, the resources matching f.
func filterResources(all []Resource, f resourceFilter) []Resource {
	out := make([]Resource, 0, len(all))
	for _, r := range all {
		if f.Type != "" && !strings.EqualFold(r.Type, f.Type) {
			continue
		}
		if f.Network != "" && !containsFold(r.Networks, f.Network) {
			continue
		}
		if f.Scheme != "" && !acceptsHaveScheme(r, f.Scheme) {
			continue
		}
		if f.PayTo != "" && !acceptsHavePayTo(r, f.PayTo) {
			continue
		}
		if f.Query != "" && !matchesQuery(r, f.Query) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

func paginate(rs []Resource, offset, limit int) []Resource {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rs) {
		return nil
	}
	rs = rs[offset:]
	if limit > 0 && limit < len(rs) {
		rs = rs[:limit]
	}
	return rs
}

func toDiscoveryResource(r Resource) discoveryResource {
	accepts := make([]json.RawMessage, 0, len(r.Accepts))
	for _, a := range r.Accepts {
		accepts = append(accepts, x402.MustRaw(a))
	}
	return discoveryResource{
		Resource:    r.URL,
		Type:        r.Type,
		X402Version: x402.Version,
		Accepts:     accepts,
		LastUpdated: r.LastUpdated.UTC().Format(time.RFC3339),
		Description: r.Description,
		MimeType:    r.MimeType,
		ServiceName: r.ServiceName,
		Tags:        r.Tags,
		IconURL:     r.IconURL,
	}
}

func acceptsHaveScheme(r Resource, scheme string) bool {
	for _, a := range r.Accepts {
		if strings.EqualFold(a.Scheme, scheme) {
			return true
		}
	}
	return false
}

func acceptsHavePayTo(r Resource, payTo string) bool {
	for _, a := range r.Accepts {
		if strings.EqualFold(a.PayTo, payTo) {
			return true
		}
	}
	return false
}

func matchesQuery(r Resource, query string) bool {
	q := strings.ToLower(query)
	if strings.Contains(strings.ToLower(r.ServiceName), q) ||
		strings.Contains(strings.ToLower(r.Description), q) ||
		strings.Contains(strings.ToLower(r.URL), q) {
		return true
	}
	for _, tag := range r.Tags {
		if strings.Contains(strings.ToLower(tag), q) {
			return true
		}
	}
	return false
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
