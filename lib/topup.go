package dcr402

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/karamble/dcr402/lib/l402"
	"github.com/karamble/dcr402/lib/ledger"
	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

// TopupOptions configure the top-up endpoint.
type TopupOptions struct {
	// MinAtoms/MaxAtoms bound the accepted top-up size
	// (defaults 100,000 / 1,000,000,000 — 0.001 to 10 DCR).
	MinAtoms int64
	MaxAtoms int64
	// DefaultAtoms is used when the request names no amount
	// (default 1,000,000 — 0.01 DCR).
	DefaultAtoms int64
	// Confirmations is the required on-chain depth (default 2).
	Confirmations int32
	// Resource overrides the ResourceInfo (default: the request URL).
	Resource func(*http.Request) x402.ResourceInfo
}

func (o *TopupOptions) fill() {
	if o.MinAtoms <= 0 {
		o.MinAtoms = 100_000
	}
	if o.MaxAtoms <= 0 {
		o.MaxAtoms = 1_000_000_000
	}
	if o.DefaultAtoms <= 0 {
		o.DefaultAtoms = 1_000_000
	}
	if o.Confirmations <= 0 {
		o.Confirmations = 2
	}
}

// Topup returns the credit top-up endpoint: F2, the slow rail funding the
// fast one. An unpaid request (optionally `?atoms=N`) receives a 402
// challenge offering the lightning method and — when the gate has an
// onchain backend — the onchain method, both minted over ONE token_id so
// either rail funds the same account. A retry carrying PAYMENT-SIGNATURE is
// verified, settled once, granted to the ledger idempotently, and answered
// with the account credential.
func (g *Gate) Topup(opts TopupOptions) http.Handler {
	opts.fill()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.cfg.Ledger == nil {
			http.Error(w, "gate has no ledger configured", http.StatusInternalServerError)
			return
		}
		if sig := r.Header.Get(x402.HeaderPaymentSignature); sig != "" {
			g.serveTopupSettlement(w, r, sig)
			return
		}

		atoms := opts.DefaultAtoms
		if v := r.FormValue("atoms"); v != "" {
			parsed, err := strconv.ParseInt(v, 10, 64)
			if err != nil || parsed <= 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "invalid atoms parameter",
				})
				return
			}
			atoms = parsed
		}
		if atoms < opts.MinAtoms || atoms > opts.MaxAtoms {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":    "amount out of bounds",
				"minAtoms": opts.MinAtoms,
				"maxAtoms": opts.MaxAtoms,
			})
			return
		}

		resource := g.resourceFor(r)
		if opts.Resource != nil {
			resource = opts.Resource(r)
		}
		art, err := g.buildTopupChallenge(r.Context(), resource, "", atoms, opts.Confirmations)
		if err != nil {
			http.Error(w, "building top-up challenge", http.StatusInternalServerError)
			return
		}
		g.writeChallengeHeaders(w, art)
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"error":   "payment_required",
			"detail":  "pay any offered method; settlement credits your account and returns its credential",
			"atoms":   atoms,
			"methods": len(art.PaymentRequired.Accepts),
		})
	})
}

func (g *Gate) serveTopupSettlement(w http.ResponseWriter, r *http.Request, sig string) {
	var pp x402.PaymentPayload
	if err := x402.DecodeHeader(sig, &pp); err != nil {
		g.respondPaymentFailure(w, r, 0, verifyErrf(ReasonInvalidPayload, "PAYMENT-SIGNATURE: %v", err))
		return
	}
	// A top-up amount is buyer-chosen, so there is no per-resource minimum.
	out, vErr := g.settleFull(r.Context(), pp, 0)
	if vErr != nil {
		// Note: respondPaymentFailure would mint a per-call challenge for
		// non-pending failures; for top-ups the client should GET the
		// endpoint again instead, so emit the failure response only.
		failure, err := x402.EncodeHeader(out.Response)
		if err == nil {
			w.Header().Set(x402.HeaderPaymentResponse, failure)
		}
		status := http.StatusPaymentRequired
		body := map[string]any{"error": "payment_verification_failed", "reason": vErr.Reason}
		if vErr.Reason == ReasonInsufficientConfs {
			body = map[string]any{"status": "pending", "reason": vErr.Reason, "detail": vErr.Detail}
		}
		writeJSON(w, status, body)
		return
	}

	// Grant is idempotent on the settlement's external ref, so replays of
	// an already-settled proof converge instead of double-crediting.
	balance, alreadyGranted, err := g.cfg.Ledger.Grant(r.Context(),
		out.AccountID, out.Rail, out.AmountAtoms, out.ExternalRef, out.Receipt, g.now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "ledger grant failed",
		})
		return
	}

	if header, err := x402.EncodeHeader(out.Response); err == nil {
		w.Header().Set(x402.HeaderPaymentResponse, header)
	}
	status := "credited"
	if out.Already || alreadyGranted {
		status = "already_credited"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       status,
		"rail":         out.Rail,
		"grantedAtoms": out.AmountAtoms,
		"balanceAtoms": balance,
		"account":      out.AccountID,
	})
}

// TopupChallenge mints the dual-method top-up challenge — a lightning entry
// and, when the gate has an onchain backend, an onchain entry — for a
// buyer-chosen amount, without touching any ledger. Callers with their own
// account system (the 402sellerkit seam, custom services) serve the returned
// object through their own envelope and settle the paid retry with Settle;
// Topup remains the batteries-included HTTP endpoint over the gate ledger.
// confirmations <= 0 uses the TopupOptions default of 2.
func (g *Gate) TopupChallenge(ctx context.Context, resource x402.ResourceInfo, amountAtoms int64, confirmations int32) (x402.PaymentRequired, error) {
	return g.TopupChallengeBound(ctx, resource, "", amountAtoms, confirmations)
}

// TopupChallengeBound is TopupChallenge with the settlement binding key
// decoupled from the wire resource, mirroring ChallengeBound: both minted
// entries are stored under bind while the 402 carries resource untouched.
// Empty bind falls back to resource.URL.
func (g *Gate) TopupChallengeBound(ctx context.Context, resource x402.ResourceInfo, bind string, amountAtoms int64, confirmations int32) (x402.PaymentRequired, error) {
	if confirmations <= 0 {
		confirmations = 2
	}
	art, err := g.buildTopupChallenge(ctx, resource, bind, amountAtoms, confirmations)
	if err != nil {
		return x402.PaymentRequired{}, err
	}
	return art.PaymentRequired, nil
}

// buildTopupChallenge assembles the dual-method top-up challenge: a
// lightning entry and, with an onchain backend, an onchain entry — one
// shared token_id, so both credentials name the same ledger account.
// bind semantics match buildChallenge.
func (g *Gate) buildTopupChallenge(ctx context.Context, resource x402.ResourceInfo, bind string, amountAtoms int64, confirmations int32) (*challengeArtifacts, error) {
	now := g.now()
	if bind == "" {
		bind = resource.URL
	}

	var tokenID [32]byte
	if _, err := rand.Read(tokenID[:]); err != nil {
		return nil, err
	}

	// Lightning entry.
	memo := g.cfg.Service + " topup " + bind
	invoice, paymentHash, err := g.cfg.Backend.CreateInvoice(ctx, amountAtoms, memo, g.cfg.ChallengeTTL)
	if err != nil {
		return nil, fmt.Errorf("dcr402: create invoice: %w", err)
	}
	macL, macLB64, err := g.mintCredential(ctx, paymentHash, tokenID, now)
	if err != nil {
		return nil, err
	}
	entryL := x402.PaymentRequirements{
		Scheme:            x402.SchemeExact,
		Network:           g.cfg.Network.CAIP2,
		Amount:            strconv.FormatInt(amountAtoms, 10),
		Asset:             x402.AssetDCR,
		PayTo:             g.cfg.PayTo,
		MaxTimeoutSeconds: int(g.cfg.ChallengeTTL.Seconds()),
		Extra: x402.MustRaw(x402.LightningExtra{
			AssetTransferMethod: x402.MethodLightning,
			Invoice:             invoice,
			PaymentHash:         hex.EncodeToString(paymentHash[:]),
		}),
	}
	reqL, err := json.Marshal(entryL)
	if err != nil {
		return nil, err
	}
	if err := g.cfg.Store.PutChallenge(ctx, store.Challenge{
		PaymentHash:  paymentHash,
		Invoice:      invoice,
		MacaroonB64:  macLB64,
		RootKeyID:    sha256.Sum256(macL.ID),
		Requirements: reqL,
		Resource:     bind,
		AmountAtoms:  amountAtoms,
		CreatedAt:    now,
		ExpiresAt:    now.Add(g.cfg.ChallengeTTL),
	}); err != nil {
		return nil, fmt.Errorf("dcr402: store challenge: %w", err)
	}

	accepts := []x402.PaymentRequirements{entryL}

	// Onchain entry (optional).
	if g.cfg.Onchain != nil {
		address, err := g.cfg.Onchain.NewDepositAddress(ctx)
		if err != nil {
			return nil, fmt.Errorf("dcr402: new deposit address: %w", err)
		}
		var credPre [32]byte
		if _, err := rand.Read(credPre[:]); err != nil {
			return nil, err
		}
		// On-chain payments yield no Lightning preimage, so the account
		// credential is minted over a server-generated one, delivered at
		// settlement (store.OnchainChallenge.CredentialPreimage).
		credHash := sha256.Sum256(credPre[:])
		macO, macOB64, err := g.mintCredential(ctx, credHash, tokenID, now)
		if err != nil {
			return nil, err
		}
		entryO := x402.PaymentRequirements{
			Scheme:            x402.SchemeExact,
			Network:           g.cfg.Network.CAIP2,
			Amount:            strconv.FormatInt(amountAtoms, 10),
			Asset:             x402.AssetDCR,
			PayTo:             address,
			MaxTimeoutSeconds: int(g.cfg.ChallengeTTL.Seconds()),
			Extra: x402.MustRaw(x402.OnchainExtra{
				AssetTransferMethod: x402.MethodOnchain,
				Confirmations:       int(confirmations),
				// Per-challenge secret: only the 402 recipient can echo it, so
				// rule 1 (accepted == offered) binds settlement to the payer.
				Credential: macOB64,
			}),
		}
		reqO, err := json.Marshal(entryO)
		if err != nil {
			return nil, err
		}
		if err := g.cfg.Store.PutOnchainChallenge(ctx, store.OnchainChallenge{
			Address:            address,
			Requirements:       reqO,
			Resource:           bind,
			AmountAtoms:        amountAtoms,
			Confirmations:      confirmations,
			MacaroonB64:        macOB64,
			RootKeyID:          sha256.Sum256(macO.ID),
			CredentialPreimage: hex.EncodeToString(credPre[:]),
			CreatedAt:          now,
			ExpiresAt:          now.Add(g.cfg.ChallengeTTL),
		}); err != nil {
			return nil, fmt.Errorf("dcr402: store onchain challenge: %w", err)
		}
		accepts = append(accepts, entryO)
	}

	pr := x402.PaymentRequired{
		X402Version: x402.Version,
		Error:       "Payment required",
		Resource:    resource,
		Accepts:     accepts,
		Extensions: map[string]x402.Extension{
			ExtensionL402: {
				Info: x402.MustRaw(map[string]int64{
					"tokenTtlSeconds": int64(g.cfg.TokenTTL.Seconds()),
				}),
				Schema: l402AdvertSchema,
			},
		},
	}
	return &challengeArtifacts{PaymentRequired: pr, Macaroon: macL, Invoice: invoice}, nil
}

// Credential errors reported by ChargeCredential.
var (
	// ErrNoCredential: the request presented no credential at all.
	ErrNoCredential = errors.New("dcr402: no credential presented")
	// ErrInvalidCredential: a credential was presented but failed
	// authorization (wrong scheme, malformed, unknown, revoked, expired,
	// or wrong service).
	ErrInvalidCredential = errors.New("dcr402: invalid or expired credential")
)

// credentialAccount authorizes an Authorization value and returns its
// account id (hex token_id).
func (g *Gate) credentialAccount(ctx context.Context, authorization string) (string, error) {
	if strings.TrimSpace(authorization) == "" {
		return "", ErrNoCredential
	}
	macs, preimage, isL402, perr := l402.ParseAuthorization(authorization)
	if !isL402 {
		return "", fmt.Errorf("%w: not an L402 credential", ErrInvalidCredential)
	}
	if perr != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCredential, perr)
	}
	account, err := g.authorizeToken(ctx, macs, preimage)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	return account, nil
}

// ChargeCredential authorizes an L402/LSAT Authorization value and
// atomically charges its account (token_id) — the building block behind
// RequireCredits, exported for callers that need dynamic prices or
// non-HTTP error shaping (the dcr402d gateway's MCP path). It returns the
// remaining balance. Failures are ErrNoCredential, ErrInvalidCredential
// (with the cause wrapped), *ledger.InsufficientBalanceError, or an
// internal error.
// idemKey is an optional caller idempotency key (empty = charge every call):
// when a client resupplies the same key on a retry, the account is charged at
// most once. See RequireCredits (the Idempotency-Key header).
func (g *Gate) ChargeCredential(ctx context.Context, authorization, tool string, amountAtoms int64, idemKey string) (int64, error) {
	if g.cfg.Ledger == nil {
		return 0, fmt.Errorf("dcr402: gate has no ledger configured")
	}
	account, err := g.credentialAccount(ctx, authorization)
	if err != nil {
		return 0, err
	}
	return g.cfg.Ledger.Charge(ctx, account, tool, amountAtoms, tool, idemKey, g.now())
}

// CredentialBalance authorizes a credential and reports its account and
// balance without charging.
func (g *Gate) CredentialBalance(ctx context.Context, authorization string) (account string, balanceAtoms int64, err error) {
	if g.cfg.Ledger == nil {
		return "", 0, fmt.Errorf("dcr402: gate has no ledger configured")
	}
	account, err = g.credentialAccount(ctx, authorization)
	if err != nil {
		return "", 0, err
	}
	balanceAtoms, err = g.cfg.Ledger.Balance(ctx, account)
	return account, balanceAtoms, err
}

// RequireCredits gates a handler behind the credit ledger: a valid L402
// credential names the account (token_id), the charge is atomic, and the
// remaining balance rides the Dcr402-Balance header. Errors are
// machine-actionable per the F2 flow: insufficient credits report the exact
// shortfall and the top-up path.
//
// A client that sets an Idempotency-Key request header makes the charge
// idempotent: a retry carrying the same key is charged at most once. Without
// the header every call charges (the header is the opt-in).
func (g *Gate) RequireCredits(tool string, amountAtoms int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			balance, err := g.ChargeCredential(r.Context(),
				r.Header.Get("Authorization"), tool, amountAtoms, r.Header.Get("Idempotency-Key"))
			switch {
			case err == nil:
				w.Header().Set("Dcr402-Balance", strconv.FormatInt(balance, 10))
				next.ServeHTTP(w, r)
			case errors.Is(err, ErrNoCredential):
				writeJSON(w, http.StatusPaymentRequired, map[string]any{
					"error":  "payment_required",
					"detail": "credit account required — top up to obtain a credential",
					"topup":  g.cfg.TopupPath,
				})
			case errors.Is(err, ErrInvalidCredential):
				writeJSON(w, http.StatusUnauthorized, map[string]any{
					"error": "invalid or expired credential",
					"topup": g.cfg.TopupPath,
				})
			default:
				var insufficient *ledger.InsufficientBalanceError
				if errors.As(err, &insufficient) {
					w.Header().Set("Dcr402-Balance", strconv.FormatInt(insufficient.Balance, 10))
					writeJSON(w, http.StatusPaymentRequired, map[string]any{
						"error":          "insufficient_credits",
						"balanceAtoms":   insufficient.Balance,
						"requiredAtoms":  insufficient.Required,
						"shortfallAtoms": insufficient.Shortfall(),
						"topup":          g.cfg.TopupPath,
					})
					return
				}
				http.Error(w, "ledger charge failed", http.StatusInternalServerError)
			}
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
