// Package dcr402 is the embeddable payment gate: it prices HTTP handlers
// and MCP tools in DCR over the Lightning Network, emitting the dual
// x402 v2 + L402/LSAT envelope and verifying preimage proofs against the
// seller's own dcrlnd — no third party in the default deployment.
//
// Wire behavior is specified in scheme/scheme_exact_dcr.md and
// scheme/l402-dual-envelope.md in this repository; the test suite pins this
// implementation to the repository's test vectors.
package dcr402

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/karamble/dcr402/lib/l402"
	"github.com/karamble/dcr402/lib/ledger"
	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

// Config configures a Gate.
type Config struct {
	// Backend creates and inspects invoices on the seller's node
	// (required). The dcrlnd subpackage implements it over gRPC with the
	// stock invoice.macaroon.
	Backend InvoiceBackend
	// Onchain enables the onchain transfer method (deposit addresses and
	// transaction lookups); optional — without it, Topup offers the
	// lightning method only and onchain payloads are rejected. The dcrlnd
	// subpackage implements it with the same invoice.macaroon.
	Onchain OnchainBackend
	// Ledger enables credit accounts (Topup grants, RequireCredits
	// burns); optional for plain per-call gating.
	Ledger ledger.Ledger
	// TopupPath is advertised in machine-actionable credit errors
	// (default "/topup").
	TopupPath string
	// Store persists challenges, settlements, and root keys (required).
	Store store.Store
	// Network is the Decred network being charged on (required).
	Network Network
	// PayTo is the seller's dcrlnd identity public key: 33-byte
	// compressed secp256k1, hex (required). Challenge invoices must be
	// issued by this node — verification binds invoice destinations to it.
	PayTo string
	// Service names this service in L402 caveats (required).
	Service string
	// Tier is the service tier for the services caveat.
	Tier int
	// Capabilities, when set, adds a capabilities caveat to minted tokens.
	Capabilities []string
	// TokenTTL is the minted credential lifetime (default 30 days).
	TokenTTL time.Duration
	// ChallengeTTL bounds challenge/invoice validity and becomes
	// maxTimeoutSeconds (default 1 hour).
	ChallengeTTL time.Duration
	// Resource derives the ResourceInfo for a request; nil uses the
	// request URL.
	Resource func(*http.Request) x402.ResourceInfo
	// DiscoveryExtension optionally returns the bazaar discovery extension
	// for a request+method, attached to the 402 so a facilitator can catalog
	// the resource. nil advertises no discovery.
	DiscoveryExtension func(r *http.Request, method string) *x402.Extension
	// MCPDiscovery optionally returns the bazaar discovery extension for an
	// MCP tool, attached to the tool's 402. nil advertises no discovery.
	MCPDiscovery func(tool string) *x402.Extension
	// MCPServerURL is the server's public streamable-HTTP MCP endpoint
	// (e.g. "https://api.example.com/mcp"). When set, MCP challenges carry
	// it as resource.url with the tool identified by the bazaar discovery
	// extension, so facilitators catalog a connectable endpoint; empty
	// falls back to the host-less mcp://tool/<name> convention. Optional;
	// absolute http(s) when set. A trailing slash is trimmed.
	MCPServerURL string
	// PaymentID enables the payment-identifier idempotency extension.
	PaymentID PaymentIDConfig
	// OfferReceiptKey, when set, signs offers in the 402 and a receipt in the
	// settlement response (offer-receipt extension, Ed25519 JWS). nil disables
	// the extension.
	OfferReceiptKey ed25519.PrivateKey
	// Now is a clock override for tests; nil uses time.Now.
	Now func() time.Time
}

// Gate gates handlers and tools behind DCR payments.
type Gate struct {
	cfg Config
}

// New validates cfg and returns a Gate.
func New(cfg Config) (*Gate, error) {
	if cfg.Backend == nil {
		return nil, fmt.Errorf("dcr402: Config.Backend is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("dcr402: Config.Store is required")
	}
	if cfg.Network.Params == nil || cfg.Network.CAIP2 == "" {
		return nil, fmt.Errorf("dcr402: Config.Network is required (use Mainnet, Testnet3, or Simnet)")
	}
	if raw, err := hex.DecodeString(cfg.PayTo); err != nil || len(raw) != 33 {
		return nil, fmt.Errorf("dcr402: Config.PayTo must be a 33-byte compressed pubkey in hex")
	}
	if cfg.Service == "" {
		return nil, fmt.Errorf("dcr402: Config.Service is required")
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 30 * 24 * time.Hour
	}
	if cfg.ChallengeTTL <= 0 {
		cfg.ChallengeTTL = time.Hour
	}
	if cfg.TopupPath == "" {
		cfg.TopupPath = "/topup"
	}
	if cfg.MCPServerURL != "" {
		cfg.MCPServerURL = strings.TrimRight(cfg.MCPServerURL, "/")
		u, err := url.Parse(cfg.MCPServerURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("dcr402: Config.MCPServerURL must be an absolute http(s) URL")
		}
	}
	return &Gate{cfg: cfg}, nil
}

func (g *Gate) now() time.Time {
	if g.cfg.Now != nil {
		return g.cfg.Now()
	}
	return time.Now()
}

func (g *Gate) resourceFor(r *http.Request) x402.ResourceInfo {
	if g.cfg.Resource != nil {
		return g.cfg.Resource(r)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return x402.ResourceInfo{URL: scheme + "://" + r.Host + r.URL.Path}
}

// discoveryExtensions returns the bazaar extension for a request when the
// DiscoveryExtension hook is set, keyed for merging into the 402.
func (g *Gate) discoveryExtensions(r *http.Request) map[string]x402.Extension {
	if g.cfg.DiscoveryExtension == nil {
		return nil
	}
	if ext := g.cfg.DiscoveryExtension(r, r.Method); ext != nil {
		return map[string]x402.Extension{ExtensionBazaar: *ext}
	}
	return nil
}

// Require gates an HTTP handler behind a payment of amountAtoms. The
// middleware implements the annex §5 precedence table: a valid L402/LSAT
// credential is served without consuming payments; a PAYMENT-SIGNATURE
// proof is verified, settled once, and answered with the reusable
// credential; bare requests receive the dual challenge; broken credentials
// receive 401, never 402.
func (g *Gate) Require(amountAtoms int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.serve(w, r, next, amountAtoms)
		})
	}
}

func (g *Gate) serve(w http.ResponseWriter, r *http.Request, next http.Handler, amountAtoms int64) {
	ctx := r.Context()
	sig := r.Header.Get(x402.HeaderPaymentSignature)

	if authz := r.Header.Get("Authorization"); authz != "" {
		macs, preimage, isL402, perr := l402.ParseAuthorization(authz)
		if isL402 {
			var authErr error
			if perr == nil {
				_, authErr = g.authorizeToken(ctx, macs, preimage)
			} else {
				authErr = perr
			}
			if authErr == nil {
				// Token wins; any accompanying payment proof MUST NOT
				// be consumed (annex §5).
				next.ServeHTTP(w, r)
				return
			}
			// Invalid or expired credential: x402 path if a proof is
			// present, else 401 (never 402 for a broken token).
			if sig == "" {
				g.respondChallenge(w, r, amountAtoms, http.StatusUnauthorized,
					"invalid or expired L402 credential")
				return
			}
		}
	}

	if sig != "" {
		g.servePayment(ctx, w, r, next, amountAtoms, sig)
		return
	}
	g.respondChallenge(w, r, amountAtoms, http.StatusPaymentRequired, "payment required")
}

func (g *Gate) servePayment(ctx context.Context, w http.ResponseWriter, r *http.Request, next http.Handler, amountAtoms int64, sig string) {
	var pp x402.PaymentPayload
	if err := x402.DecodeHeader(sig, &pp); err != nil {
		g.respondPaymentFailure(w, r, amountAtoms,
			verifyErrf(ReasonInvalidPayload, "PAYMENT-SIGNATURE: %v", err))
		return
	}
	if g.cfg.PaymentID.Enabled {
		if id, present := extractPaymentID(pp); present {
			if !validPaymentID(id) {
				g.respondPaymentFailure(w, r, amountAtoms,
					verifyErrf(ReasonInvalidPayload, "payment-identifier id is malformed"))
				return
			}
		} else if g.cfg.PaymentID.Required {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":  "payment_identifier_required",
				"detail": "this resource requires the payment-identifier extension id",
			})
			return
		}
	}
	resp, already, vErr := g.Settle(ctx, pp, amountAtoms)
	if vErr != nil {
		g.respondPaymentFailure(w, r, amountAtoms, vErr)
		return
	}
	header, err := x402.EncodeHeader(resp)
	if err != nil {
		http.Error(w, "encoding settlement response", http.StatusInternalServerError)
		return
	}
	w.Header().Set(x402.HeaderPaymentResponse, header)

	if already {
		// Idempotent re-presentation (rule 8): the paid operation is NOT
		// re-executed; the client holds the credential for access.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "already_settled",
			"hint":   "present the L402 credential from extensions.l402 for access",
		})
		return
	}
	next.ServeHTTP(w, r)
}

// respondChallenge emits the dual challenge (annex §2) with the given
// status: WWW-Authenticate LSAT, WWW-Authenticate L402, PAYMENT-REQUIRED.
func (g *Gate) respondChallenge(w http.ResponseWriter, r *http.Request, amountAtoms int64, status int, note string) {
	art, err := g.buildChallenge(r.Context(), g.resourceFor(r), "", amountAtoms, g.discoveryExtensions(r))
	if err != nil {
		http.Error(w, "building payment challenge", http.StatusInternalServerError)
		return
	}
	g.writeChallengeHeaders(w, art)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":  "payment_required",
		"detail": note,
		"amount": art.PaymentRequired.Accepts[0].Amount,
		"asset":  x402.AssetDCR,
	})
}

// respondPaymentFailure reports a failed proof: 402 with a failure
// PAYMENT-RESPONSE plus a fresh challenge for retry. The retryable
// insufficient_confirmations condition gets no fresh challenge — the client
// re-presents the SAME proof once the deposit is deep enough.
func (g *Gate) respondPaymentFailure(w http.ResponseWriter, r *http.Request, amountAtoms int64, vErr *VerifyError) {
	failure, err := x402.EncodeHeader(g.failureResponse(vErr))
	if err == nil {
		w.Header().Set(x402.HeaderPaymentResponse, failure)
	}
	w.Header().Set("Content-Type", "application/json")
	if vErr.Reason == ReasonInsufficientConfs {
		w.WriteHeader(http.StatusPaymentRequired)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "pending",
			"reason": vErr.Reason,
			"detail": vErr.Detail,
		})
		return
	}
	if art, err := g.buildChallenge(r.Context(), g.resourceFor(r), "", amountAtoms, g.discoveryExtensions(r)); err == nil {
		g.writeChallengeHeaders(w, art)
	}
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":  "payment_verification_failed",
		"reason": vErr.Reason,
	})
}

func (g *Gate) writeChallengeHeaders(w http.ResponseWriter, art *challengeArtifacts) {
	l402.WriteChallenge(w.Header(), art.Macaroon, art.Invoice)
	if header, err := x402.EncodeHeader(art.PaymentRequired); err == nil {
		w.Header().Set(x402.HeaderPaymentRequired, header)
	}
}

// authorizeToken verifies an L402 credential stack against the root-key
// store and this gate's service (annex §4), returning the credential's
// account id — the hex token_id (annex §7).
func (g *Gate) authorizeToken(ctx context.Context, macs []*l402.Macaroon, preimageHex string) (string, error) {
	if len(macs) == 0 {
		return "", fmt.Errorf("dcr402: no macaroon")
	}
	mac := macs[0]
	id, err := mac.Identifier()
	if err != nil {
		return "", err
	}
	rootKey, err := g.cfg.Store.GetRootKey(ctx, id.StorageKey())
	if err != nil {
		return "", fmt.Errorf("dcr402: unknown or revoked credential")
	}
	if err := l402.VerifyToken(rootKey, mac, preimageHex, l402.VerifyOptions{
		Service: g.cfg.Service,
		Now:     g.now(),
	}); err != nil {
		return "", err
	}
	return hex.EncodeToString(id.TokenID[:]), nil
}
