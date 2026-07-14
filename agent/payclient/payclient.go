// Package payclient is dcr402-agent's paid-HTTP engine: the client side of
// the dcr402 scheme. It probes a resource, understands BOTH challenge
// envelopes — x402 v2 `Payment-Required` (preferred) and classic
// `WWW-Authenticate: L402`/`LSAT` — decodes the invoice locally so policy
// can rule BEFORE any payment, pays over the Lightning rail, retries with
// the matching proof, and caches the returned credential per service host
// so repeat access is free.
package payclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/l402"
	"github.com/karamble/dcr402/lib/x402"

	"github.com/karamble/dcr402/agent/policy"
	"github.com/karamble/dcr402/agent/rails/ln"
	"github.com/karamble/dcr402/agent/store"
)

// LNRail is the payment dependency (rails/ln.Client satisfies it).
type LNRail interface {
	Pay(ctx context.Context, invoice string, feeLimitAtoms int64, timeout time.Duration) (preimageHex string, feeAtoms int64, err error)
}

// TokenStore is the credential-cache dependency (agent/store satisfies it).
type TokenStore interface {
	GetToken(ctx context.Context, serviceHost string) (store.Token, error)
	PutToken(ctx context.Context, t store.Token) error
	TouchToken(ctx context.Context, serviceHost string, at time.Time) error
	DeleteToken(ctx context.Context, serviceHost string) error
}

// Gatekeeper is the policy hook, called with the fully decoded attempt
// BEFORE any payment. It returns the recorded attempt id to proceed, or an
// error (deny / pending-approval) that aborts the fetch.
type Gatekeeper func(ctx context.Context, a policy.Attempt) (attemptID int64, err error)

// Config wires a Client.
type Config struct {
	LN      LNRail
	Network dcr402.Network
	Tokens  TokenStore
	Gate    Gatekeeper
	// HTTP is the transport (default: http.DefaultClient).
	HTTP *http.Client
	// FeeLimitAtoms bounds Lightning routing fees (default 10,000).
	FeeLimitAtoms int64
	// PayTimeout bounds one payment (default 60s).
	PayTimeout time.Duration
	Now        func() time.Time
}

// Client executes paid fetches.
type Client struct {
	cfg Config
}

// New validates and returns a Client. Gate may be nil at construction and
// set later with SetGate (the daemon wires it after the Service exists).
func New(cfg Config) (*Client, error) {
	if cfg.LN == nil || cfg.Tokens == nil {
		return nil, fmt.Errorf("payclient: LN and Tokens are required")
	}
	if cfg.Network.Params == nil {
		return nil, fmt.Errorf("payclient: Network is required")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = http.DefaultClient
	}
	if cfg.FeeLimitAtoms <= 0 {
		cfg.FeeLimitAtoms = 10_000
	}
	if cfg.PayTimeout <= 0 {
		cfg.PayTimeout = 60 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Client{cfg: cfg}, nil
}

// SetGate installs the policy hook after construction.
func (c *Client) SetGate(g Gatekeeper) { c.cfg.Gate = g }

// Rails reported in Result.
const (
	RailFree   = "free"
	RailCached = "cached"
	RailX402   = "x402"
	RailL402   = "l402"
)

// FetchOptions tune one FetchPaid call.
type FetchOptions struct {
	Method      string // default GET
	Body        []byte
	ContentType string
	// Memo is the agent-supplied reason (policy may require it).
	Memo string
	// MaxAmountAtoms is the agent-side guard: a challenge above it aborts
	// before policy. 0 = no guard.
	MaxAmountAtoms int64
	// ForceLegacy skips the x402 envelope and uses WWW-Authenticate L402
	// (testing / legacy-only services).
	ForceLegacy bool
}

// Result is the outcome of a FetchPaid. When a payment occurred, Result is
// returned even alongside an error (the receipt matters regardless).
type Result struct {
	Status int
	Header http.Header
	Body   []byte

	Rail        string
	PaidAtoms   int64
	FeeAtoms    int64
	Invoice     string
	Preimage    string
	Receipt     json.RawMessage
	AttemptID   int64
	TokenCached bool
}

// challenge is the envelope-independent view of a 402.
type challenge struct {
	rail        string // RailX402 | RailL402
	invoice     string
	amountAtoms int64
	destination string
	paymentHash string
	description string
	// x402 specifics for the retry payload.
	entry      x402.PaymentRequirements
	resource   *x402.ResourceInfo
	extensions map[string]x402.Extension
	// legacy specifics.
	macaroonB64 string
}

// FetchPaid performs the paid request flow against url.
func (c *Client) FetchPaid(ctx context.Context, rawURL string, opt FetchOptions) (*Result, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("payclient: invalid url %q", rawURL)
	}
	host := u.Hostname()
	if opt.Method == "" {
		opt.Method = http.MethodGet
	}

	// 1. Try with the cached credential (or bare).
	auth := ""
	now := c.cfg.Now()
	if tok, err := c.cfg.Tokens.GetToken(ctx, host); err == nil {
		if tok.ValidUntil == 0 || now.Unix() < tok.ValidUntil {
			auth = tok.Authorization
		}
	}
	resp, body, err := c.do(ctx, opt.Method, rawURL, opt, auth)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 300 {
		rail := RailFree
		if auth != "" {
			rail = RailCached
			_ = c.cfg.Tokens.TouchToken(ctx, host, now)
		}
		return &Result{Status: resp.StatusCode, Header: resp.Header, Body: body, Rail: rail}, nil
	}
	if auth != "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusPaymentRequired) {
		// Credential no longer honored: drop it and re-request bare so we
		// hold a fresh challenge.
		_ = c.cfg.Tokens.DeleteToken(ctx, host)
		resp, body, err = c.do(ctx, opt.Method, rawURL, opt, "")
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 300 {
			return &Result{Status: resp.StatusCode, Header: resp.Header, Body: body, Rail: RailFree}, nil
		}
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		return &Result{Status: resp.StatusCode, Header: resp.Header, Body: body},
			fmt.Errorf("payclient: unexpected status %d", resp.StatusCode)
	}

	// 2. Parse the challenge.
	ch, err := c.parseChallenge(resp, opt.ForceLegacy)
	if err != nil {
		return nil, err
	}
	if opt.MaxAmountAtoms > 0 && ch.amountAtoms > opt.MaxAmountAtoms {
		return nil, fmt.Errorf("payclient: challenge asks %d atoms, above the agent guard %d",
			ch.amountAtoms, opt.MaxAmountAtoms)
	}

	// 3. Policy gate — BEFORE any payment.
	if c.cfg.Gate == nil {
		return nil, fmt.Errorf("payclient: no policy gate configured")
	}
	attemptID, err := c.cfg.Gate(ctx, policy.Attempt{
		Tool:        "fetch_paid",
		Rail:        "ln",
		DestKind:    policy.DestDomain,
		Dest:        host,
		AmountAtoms: ch.amountAtoms,
		Memo:        opt.Memo,
	})
	if err != nil {
		return nil, err
	}

	// 4. Pay.
	preimage, fee, err := c.cfg.LN.Pay(ctx, ch.invoice, c.cfg.FeeLimitAtoms, c.cfg.PayTimeout)
	if err != nil {
		return nil, fmt.Errorf("payclient: payment: %w", err)
	}
	receipt := x402.MustRaw(map[string]string{
		"invoice":   ch.invoice,
		"preimage":  preimage,
		"settledAt": c.cfg.Now().UTC().Format(time.RFC3339),
		"amount":    strconv.FormatInt(ch.amountAtoms, 10),
	})
	result := &Result{
		Rail:      ch.rail,
		PaidAtoms: ch.amountAtoms,
		FeeAtoms:  fee,
		Invoice:   ch.invoice,
		Preimage:  preimage,
		Receipt:   receipt,
		AttemptID: attemptID,
	}

	// 5. Retry with the proof.
	retryAuth, retrySig := "", ""
	if ch.rail == RailX402 {
		payload := x402.PaymentPayload{
			X402Version: x402.Version,
			Resource:    ch.resource,
			Accepted:    ch.entry,
			Payload: x402.MustRaw(x402.LightningPayload{
				Preimage:    preimage,
				PaymentHash: ch.paymentHash,
			}),
			Extensions: ch.extensions,
		}
		if retrySig, err = x402.EncodeHeader(payload); err != nil {
			return result, fmt.Errorf("payclient: encode payload: %w", err)
		}
	} else {
		retryAuth = l402.SchemeL402 + " " + ch.macaroonB64 + ":" + preimage
	}
	resp, body, err = c.doRetry(ctx, opt, rawURL, retryAuth, retrySig)
	if err != nil {
		return result, fmt.Errorf("payclient: paid retry: %w", err)
	}
	result.Status = resp.StatusCode
	result.Header = resp.Header
	result.Body = body
	if resp.StatusCode >= 300 {
		return result, fmt.Errorf("payclient: paid but service answered %d — keep the receipt", resp.StatusCode)
	}

	// 6. Cache the credential for free re-access.
	credential, validUntil := "", int64(0)
	if ch.rail == RailX402 {
		var settle x402.SettlementResponse
		if h := resp.Header.Get(x402.HeaderPaymentResponse); h != "" {
			if err := x402.DecodeHeader(h, &settle); err == nil {
				if ext, ok := settle.Extensions[dcr402.ExtensionL402]; ok {
					var info struct {
						Authorization string `json:"authorization"`
						ValidUntil    int64  `json:"validUntil"`
					}
					if json.Unmarshal(ext.Info, &info) == nil && info.Authorization != "" {
						credential, validUntil = info.Authorization, info.ValidUntil
					}
				}
			}
		}
	} else {
		credential = retryAuth
	}
	if credential != "" {
		if err := c.cfg.Tokens.PutToken(ctx, store.Token{
			ServiceHost:   host,
			Authorization: credential,
			ValidUntil:    validUntil,
			PaidAtoms:     ch.amountAtoms,
			CreatedAt:     now,
			LastUsed:      now,
		}); err == nil {
			result.TokenCached = true
		}
	}
	return result, nil
}

// parseChallenge extracts the envelope-independent challenge, preferring
// x402 v2 and cross-checking every claim against the locally decoded
// invoice — nothing the server says is trusted unverified.
func (c *Client) parseChallenge(resp *http.Response, forceLegacy bool) (*challenge, error) {
	if !forceLegacy {
		if h := resp.Header.Get(x402.HeaderPaymentRequired); h != "" {
			return c.parseX402(h)
		}
	}
	l4, err := l402.FindChallenge(resp.Header.Values("WWW-Authenticate"))
	if err != nil {
		return nil, fmt.Errorf("payclient: L402 challenge: %w", err)
	}
	if l4 == nil {
		return nil, fmt.Errorf("payclient: 402 without a recognizable challenge")
	}
	inv, err := ln.DecodeInvoice(l4.Invoice, c.cfg.Network.Params)
	if err != nil {
		return nil, err
	}
	if inv.AmountAtoms <= 0 {
		return nil, fmt.Errorf("payclient: challenge invoice carries no amount")
	}
	return &challenge{
		rail:        RailL402,
		invoice:     l4.Invoice,
		amountAtoms: inv.AmountAtoms,
		destination: inv.Destination,
		paymentHash: inv.PaymentHash,
		description: inv.Description,
		macaroonB64: l4.MacaroonB64,
	}, nil
}

func (c *Client) parseX402(header string) (*challenge, error) {
	var pr x402.PaymentRequired
	if err := x402.DecodeHeader(header, &pr); err != nil {
		return nil, fmt.Errorf("payclient: PAYMENT-REQUIRED: %w", err)
	}
	if pr.X402Version != x402.Version {
		return nil, fmt.Errorf("payclient: unsupported x402 version %d", pr.X402Version)
	}
	for _, entry := range pr.Accepts {
		if entry.TransferMethod() != x402.MethodLightning {
			continue
		}
		if entry.Network != c.cfg.Network.CAIP2 {
			return nil, fmt.Errorf("payclient: challenge network %q, agent is on %q",
				entry.Network, c.cfg.Network.CAIP2)
		}
		var extra x402.LightningExtra
		if err := json.Unmarshal(entry.Extra, &extra); err != nil {
			return nil, fmt.Errorf("payclient: lightning extra: %w", err)
		}
		inv, err := ln.DecodeInvoice(extra.Invoice, c.cfg.Network.Params)
		if err != nil {
			return nil, err
		}
		amount, err := strconv.ParseInt(entry.Amount, 10, 64)
		if err != nil || amount <= 0 {
			return nil, fmt.Errorf("payclient: challenge amount %q", entry.Amount)
		}
		// Cross-checks: the envelope must agree with the invoice.
		if !strings.EqualFold(inv.PaymentHash, extra.PaymentHash) {
			return nil, fmt.Errorf("payclient: challenge paymentHash differs from invoice")
		}
		if !strings.EqualFold(inv.Destination, entry.PayTo) {
			return nil, fmt.Errorf("payclient: challenge payTo differs from invoice destination")
		}
		if inv.AmountMAtoms != amount*1000 {
			return nil, fmt.Errorf("payclient: challenge amount %d differs from invoice (%d m-atoms)",
				amount, inv.AmountMAtoms)
		}
		res := pr.Resource
		return &challenge{
			rail:        RailX402,
			invoice:     extra.Invoice,
			amountAtoms: amount,
			destination: inv.Destination,
			paymentHash: extra.PaymentHash,
			description: inv.Description,
			entry:       entry,
			resource:    &res,
			extensions:  pr.Extensions,
		}, nil
	}
	return nil, fmt.Errorf("payclient: no lightning entry in the challenge")
}

func (c *Client) do(ctx context.Context, method, rawURL string, opt FetchOptions, authorization string) (*http.Response, []byte, error) {
	var bodyReader io.Reader
	if len(opt.Body) > 0 {
		bodyReader = bytes.NewReader(opt.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	if opt.ContentType != "" {
		req.Header.Set("Content-Type", opt.ContentType)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

func (c *Client) doRetry(ctx context.Context, opt FetchOptions, rawURL, authorization, paymentSignature string) (*http.Response, []byte, error) {
	var bodyReader io.Reader
	if len(opt.Body) > 0 {
		bodyReader = bytes.NewReader(opt.Body)
	}
	req, err := http.NewRequestWithContext(ctx, opt.Method, rawURL, bodyReader)
	if err != nil {
		return nil, nil, err
	}
	if opt.ContentType != "" {
		req.Header.Set("Content-Type", opt.ContentType)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if paymentSignature != "" {
		req.Header.Set(x402.HeaderPaymentSignature, paymentSignature)
	}
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, nil, err
	}
	return resp, body, nil
}

// Estimate describes what a resource (or bare invoice) would cost — it
// never pays.
type Estimate struct {
	Kind        string // "invoice" | "url"
	Rail        string // x402 | l402 | free | cached
	AmountAtoms int64
	Destination string
	PaymentHash string
	Description string
	Host        string
}

// Estimate probes a URL's 402 challenge, or decodes a bare BOLT11 invoice.
func (c *Client) Estimate(ctx context.Context, target string) (*Estimate, error) {
	if strings.HasPrefix(strings.ToLower(target), "ln") && !strings.Contains(target, "://") {
		inv, err := ln.DecodeInvoice(target, c.cfg.Network.Params)
		if err != nil {
			return nil, err
		}
		return &Estimate{
			Kind:        "invoice",
			Rail:        "ln",
			AmountAtoms: inv.AmountAtoms,
			Destination: inv.Destination,
			PaymentHash: inv.PaymentHash,
			Description: inv.Description,
		}, nil
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("payclient: estimate target is neither an invoice nor a url")
	}
	resp, _, err := c.do(ctx, http.MethodGet, target, FetchOptions{}, "")
	if err != nil {
		return nil, err
	}
	est := &Estimate{Kind: "url", Host: u.Hostname()}
	if resp.StatusCode < 300 {
		est.Rail = RailFree
		return est, nil
	}
	if resp.StatusCode != http.StatusPaymentRequired {
		return nil, fmt.Errorf("payclient: estimate got status %d", resp.StatusCode)
	}
	ch, err := c.parseChallenge(resp, false)
	if err != nil {
		return nil, err
	}
	est.Rail = ch.rail
	est.AmountAtoms = ch.amountAtoms
	est.Destination = ch.destination
	est.PaymentHash = ch.paymentHash
	est.Description = ch.description
	return est, nil
}
