package bazaar

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/x402"
)

// maxBodyBytes caps a request body. A lightning PaymentPayload with an
// embedded invoice is about 1.5 KB; 1 MiB is generous headroom.
const maxBodyBytes = 1 << 20

// Service is a running dcrbazaar facilitator.
type Service struct {
	cfg      Config
	networks []dcr402.Network          // configured order (drives /supported)
	byCAIP2  map[string]dcr402.Network // network resolution
	keys     map[string]bool           // API keys (empty = open)
	store    Store
	onchain  onchainLooker    // dcrdata reader for the onchain method; nil when disabled
	nowFn    func() time.Time // clock override for tests; nil uses time.Now
}

// New builds a Service from cfg and a Store. The Store is required; use
// NewMemory or OpenSQLite.
func New(cfg Config, st Store) (*Service, error) {
	if st == nil {
		return nil, fmt.Errorf("dcrbazaar: store is required")
	}
	nets, err := cfg.resolveNetworks()
	if err != nil {
		return nil, err
	}
	byCAIP2 := make(map[string]dcr402.Network, len(nets))
	for _, n := range nets {
		byCAIP2[n.CAIP2] = n
	}
	keys := make(map[string]bool, len(cfg.APIKeys))
	for _, k := range cfg.APIKeys {
		keys[k] = true
	}
	var oc onchainLooker
	if cfg.Onchain.Enabled {
		oc = newDcrdataClient(cfg.Onchain.DcrdataURL)
	}
	return &Service{cfg: cfg, networks: nets, byCAIP2: byCAIP2, keys: keys, store: st, onchain: oc}, nil
}

func (s *Service) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

func (s *Service) networkFor(caip2 string) (dcr402.Network, bool) {
	n, ok := s.byCAIP2[caip2]
	return n, ok
}

// Handler returns the facilitator's HTTP routes: the standard x402 v2
// facilitator API (/verify, /settle, /supported).
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /verify", s.gated(s.handleVerify))
	mux.HandleFunc("POST /settle", s.gated(s.handleSettle))
	mux.HandleFunc("GET /supported", s.handleSupported)
	s.registerDiscovery(mux)
	return mux
}

// gated enforces API-key auth on /verify and /settle when keys are
// configured. With no keys set (the self-hosted default) it is a pass-through.
func (s *Service) gated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.keys) > 0 && !s.validKey(r.Header.Get("X-API-Key")) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid X-API-Key"})
			return
		}
		next(w, r)
	}
}

// validKey reports whether presented matches a configured key, in constant
// time (no early exit on a per-byte mismatch).
func (s *Service) validKey(presented string) bool {
	ok := false
	for k := range s.keys {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(k)) == 1 {
			ok = true
		}
	}
	return ok
}

func (s *Service) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.verify(r.Context(), req))
}

func (s *Service) handleSettle(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resp := s.settle(r.Context(), req)
	// On a fresh settlement, catalog the resource from any echoed bazaar
	// discovery extension and report the outcome in EXTENSION-RESPONSES.
	if resp.Success && s.cfg.Discovery.Enabled {
		if status, reason := s.harvestBazaar(r.Context(), req.PaymentPayload, req.PaymentRequirements); status != "" {
			if hdr, ok := extensionResponsesHeader(status, reason); ok {
				w.Header().Set("Extension-Responses", hdr)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Service) handleSupported(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.supported())
}

// supported advertises the configured networks under the exact scheme, one
// kind per enabled transfer method (lightning always; onchain when the dcrdata
// verify source is configured). Signers is empty: neither Decred method has an
// on-chain settlement signer - lightning settles peer-to-peer and the onchain
// method is payer-broadcast, so dcrbazaar only reads/notarizes, never signs.
func (s *Service) supported() x402.SupportedResponse {
	methods := []string{x402.MethodLightning}
	if s.cfg.Onchain.Enabled {
		methods = append(methods, x402.MethodOnchain)
	}
	kinds := make([]x402.SupportedKind, 0, len(s.networks)*len(methods))
	for _, n := range s.networks {
		for _, m := range methods {
			kinds = append(kinds, x402.SupportedKind{
				X402Version: x402.Version,
				Scheme:      x402.SchemeExact,
				Network:     n.CAIP2,
				Extra:       x402.MustRaw(map[string]string{"assetTransferMethod": m}),
			})
		}
	}
	return x402.SupportedResponse{
		Kinds:      kinds,
		Extensions: s.extensions(),
		Signers:    map[string][]string{},
	}
}

// extensions lists the protocol extensions this facilitator implements. The
// bazaar discovery index is advertised when enabled.
func (s *Service) extensions() []string {
	ext := []string{}
	if s.cfg.Discovery.Enabled {
		ext = append(ext, extensionBazaar)
	}
	return ext
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
