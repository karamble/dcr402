package dcr402

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/karamble/dcr402/lib/l402"
	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

// Golden vector constants (scheme/test-vectors/): the only invoice with a
// known preimage, signed by the published BOLT11 test key.
const (
	goldenInvoice = "lndcr2500u1pvjluezpp5qt2yngclhvn8ere496vk3fu78e0ujhqmh649qt7kg48tmedyhmwqdpqv33hydpsxgsxwmmvv3jkugrkv43hgmmjxqrrssdztqxz9z9ys3q59ml9270ej0t9wt62442s3tzzldns0a6j247qkkqzakysm9yz75xqze4a7r3h7tsys8tcugay7f8sru2l8a7s07srcqc5t8f7"
	goldenHash    = "02d449a31fbb267c8f352e9968a79e3e5fc95c1bbeaa502fd6454ebde5a4bedc"
	goldenPre     = "1111111111111111111111111111111111111111111111111111111111111111"
	goldenDest    = "03e7156ae33b0a208d0744199163177e909e80176e55d97a2f221ede0f934dd9ad"
	goldenAtoms   = 250000
)

// fakeLN is an InvoiceBackend that issues the golden invoice and settles it
// when Pay is called — the stub client's counterpart.
type fakeLN struct {
	mu      sync.Mutex
	settled bool
}

func (f *fakeLN) CreateInvoice(_ context.Context, _ int64, _ string, _ time.Duration) (string, [32]byte, error) {
	// The fake always issues the golden invoice regardless of amount: only
	// flows that SETTLE the lightning entry need the amounts to line up
	// (those use goldenAtoms); onchain-focused tests merely need a
	// challenge to exist.
	var hash [32]byte
	raw, _ := hex.DecodeString(goldenHash)
	copy(hash[:], raw)
	return goldenInvoice, hash, nil
}

func (f *fakeLN) LookupInvoice(_ context.Context, paymentHash [32]byte) (InvoiceStatus, error) {
	if hex.EncodeToString(paymentHash[:]) != goldenHash {
		return InvoiceStatus{}, fmt.Errorf("unknown invoice")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return InvoiceStatus{Settled: f.settled, AmtPaidMAtoms: goldenAtoms * 1000}, nil
}

// Pay simulates the buyer's node settling the invoice.
func (f *fakeLN) Pay() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settled = true
}

func newTestGate(t *testing.T, ln *fakeLN, st store.Store, now func() time.Time) *Gate {
	t.Helper()
	g, err := New(Config{
		Backend:      ln,
		Store:        st,
		Network:      Mainnet,
		PayTo:        goldenDest,
		Service:      "example",
		TokenTTL:     time.Hour,
		ChallengeTTL: time.Hour,
		Now:          now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestEndToEnd walks the full F1 flow against an httptest server: challenge,
// payment proof, credential reuse, idempotent replay, and broken/expired
// credential handling.
func TestEndToEnd(t *testing.T) {
	ln := &fakeLN{}
	st := store.NewMemory()
	g := newTestGate(t, ln, st, nil)

	var handlerRuns atomic.Int64
	secret := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerRuns.Add(1)
		fmt.Fprint(w, "SECRET")
	})
	srv := httptest.NewServer(g.Require(goldenAtoms)(secret))
	defer srv.Close()

	// 1. Bare request → 402 with the triple challenge.
	resp, err := http.Get(srv.URL + "/paid")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("bare request: status %d", resp.StatusCode)
	}
	www := resp.Header.Values("Www-Authenticate")
	if len(www) != 2 ||
		!strings.HasPrefix(www[0], l402.SchemeLSAT+" ") ||
		!strings.HasPrefix(www[1], l402.SchemeL402+" ") {
		t.Fatalf("WWW-Authenticate challenges wrong: %v", www)
	}
	var pr x402.PaymentRequired
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &pr); err != nil {
		t.Fatalf("PAYMENT-REQUIRED: %v", err)
	}
	if pr.X402Version != 2 || len(pr.Accepts) != 1 {
		t.Fatalf("challenge shape: %+v", pr)
	}
	entry := pr.Accepts[0]
	var extra x402.LightningExtra
	if err := json.Unmarshal(entry.Extra, &extra); err != nil {
		t.Fatal(err)
	}
	if entry.Amount != "250000" || extra.Invoice != goldenInvoice || extra.PaymentHash != goldenHash {
		t.Fatalf("challenge entry wrong: %+v extra=%+v", entry, extra)
	}
	if _, ok := pr.Extensions[ExtensionL402]; !ok {
		t.Fatal("challenge lacks l402 advertisement extension")
	}

	// 2. Pay (fake) and retry with the proof.
	ln.Pay()
	payload := x402.PaymentPayload{
		X402Version: 2,
		Resource:    &pr.Resource,
		Accepted:    entry,
		Payload:     x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
	}
	sig, err := x402.EncodeHeader(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/paid", nil)
	req.Header.Set(x402.HeaderPaymentSignature, sig)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "SECRET" {
		t.Fatalf("paid request: status %d body %q", resp.StatusCode, body)
	}
	var settle x402.SettlementResponse
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentResponse), &settle); err != nil {
		t.Fatalf("PAYMENT-RESPONSE: %v", err)
	}
	if !settle.Success || settle.Transaction != goldenHash || settle.Amount != "250000" {
		t.Fatalf("settlement wrong: %+v", settle)
	}
	if settle.Transaction == goldenPre {
		t.Fatal("settlement leaked the preimage")
	}

	// 3. The delivered credential grants token access.
	ext, ok := settle.Extensions[ExtensionL402]
	if !ok {
		t.Fatal("settlement lacks l402 credential extension")
	}
	var cred struct {
		Authorization string `json:"authorization"`
		ValidUntil    int64  `json:"validUntil"`
	}
	if err := json.Unmarshal(ext.Info, &cred); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cred.Authorization, "L402 ") || cred.ValidUntil == 0 {
		t.Fatalf("credential wrong: %+v", cred)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/paid", nil)
	req.Header.Set("Authorization", cred.Authorization)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "SECRET" {
		t.Fatalf("token request: status %d body %q", resp.StatusCode, body)
	}

	// 4. Re-presenting the same proof is idempotent: original settlement
	// returned, handler NOT re-executed.
	runsBefore := handlerRuns.Load()
	req, _ = http.NewRequest("GET", srv.URL+"/paid", nil)
	req.Header.Set(x402.HeaderPaymentSignature, sig)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay: status %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "already_settled") {
		t.Fatalf("replay body: %q", body)
	}
	if handlerRuns.Load() != runsBefore {
		t.Fatal("replay re-executed the paid handler")
	}
	var replay x402.SettlementResponse
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentResponse), &replay); err != nil {
		t.Fatal(err)
	}
	if !replay.Success || replay.Transaction != goldenHash {
		t.Fatalf("replay settlement wrong: %+v", replay)
	}

	// 5. A tampered credential gets 401 (never 402).
	tampered := tamperCredential(t, cred.Authorization)
	req, _ = http.NewRequest("GET", srv.URL+"/paid", nil)
	req.Header.Set("Authorization", tampered)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tampered token: status %d, want 401", resp.StatusCode)
	}

	// 6. An expired credential gets 401 from a future-clock gate sharing
	// the same store.
	future := func() time.Time { return time.Now().Add(48 * time.Hour) }
	g2 := newTestGate(t, ln, st, future)
	srv2 := httptest.NewServer(g2.Require(goldenAtoms)(secret))
	defer srv2.Close()
	req, _ = http.NewRequest("GET", srv2.URL+"/paid", nil)
	req.Header.Set("Authorization", cred.Authorization)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired token: status %d, want 401", resp.StatusCode)
	}
}

// tamperCredential flips a caveat inside an Authorization value without
// re-signing — the signature chain must catch it.
func tamperCredential(t *testing.T, authorization string) string {
	t.Helper()
	rest := strings.TrimPrefix(authorization, "L402 ")
	macB64, preimage, _ := strings.Cut(rest, ":")
	raw, err := base64.StdEncoding.DecodeString(macB64)
	if err != nil {
		t.Fatal(err)
	}
	mac, err := l402.UnmarshalBinary(raw)
	if err != nil {
		t.Fatal(err)
	}
	mac.Caveats[0] = "services=example:9"
	return "L402 " + base64.StdEncoding.EncodeToString(mac.MarshalBinary()) + ":" + preimage
}

// TestVerificationFailures drives the wire-visible failure paths.
func TestVerificationFailures(t *testing.T) {
	ln := &fakeLN{}
	st := store.NewMemory()
	g := newTestGate(t, ln, st, nil)
	srv := httptest.NewServer(g.Require(goldenAtoms)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "SECRET")
	})))
	defer srv.Close()

	// Obtain a live challenge.
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var pr x402.PaymentRequired
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &pr); err != nil {
		t.Fatal(err)
	}
	entry := pr.Accepts[0]
	ln.Pay()

	post := func(t *testing.T, payload x402.PaymentPayload) (int, x402.SettlementResponse) {
		t.Helper()
		sig, err := x402.EncodeHeader(payload)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest("GET", srv.URL, nil)
		req.Header.Set(x402.HeaderPaymentSignature, sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		var sr x402.SettlementResponse
		if h := resp.Header.Get(x402.HeaderPaymentResponse); h != "" {
			if err := x402.DecodeHeader(h, &sr); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, sr
	}

	base := func() x402.PaymentPayload {
		return x402.PaymentPayload{
			X402Version: 2,
			Accepted:    entry,
			Payload:     x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
		}
	}

	t.Run("wrong preimage", func(t *testing.T) {
		p := base()
		p.Payload = x402.MustRaw(x402.LightningPayload{
			Preimage:    strings.Repeat("22", 32),
			PaymentHash: goldenHash,
		})
		status, sr := post(t, p)
		if status != http.StatusPaymentRequired || sr.ErrorReason != ReasonPreimageMismatch {
			t.Fatalf("status=%d reason=%q", status, sr.ErrorReason)
		}
	})

	t.Run("wrong version", func(t *testing.T) {
		p := base()
		p.X402Version = 1
		status, sr := post(t, p)
		if status != http.StatusPaymentRequired || sr.ErrorReason != ReasonInvalidVersion {
			t.Fatalf("status=%d reason=%q", status, sr.ErrorReason)
		}
	})

	t.Run("tampered amount", func(t *testing.T) {
		p := base()
		p.Accepted.Amount = "1"
		status, sr := post(t, p)
		if status != http.StatusPaymentRequired || sr.ErrorReason != ReasonInvalidRequirements {
			t.Fatalf("status=%d reason=%q", status, sr.ErrorReason)
		}
	})

	t.Run("unknown hash", func(t *testing.T) {
		// A self-consistent preimage/hash pair that no challenge was ever
		// issued for: passes rule 3, fails rule 8's correlation check.
		pre := strings.Repeat("33", 32)
		preBytes, _ := hex.DecodeString(pre)
		sum := sha256.Sum256(preBytes)
		other := hex.EncodeToString(sum[:])

		p := base()
		var extra x402.LightningExtra
		if err := json.Unmarshal(p.Accepted.Extra, &extra); err != nil {
			t.Fatal(err)
		}
		extra.PaymentHash = other
		p.Accepted.Extra = x402.MustRaw(extra)
		p.Payload = x402.MustRaw(x402.LightningPayload{Preimage: pre, PaymentHash: other})
		status, sr := post(t, p)
		if status != http.StatusPaymentRequired || sr.ErrorReason != ReasonUnknownPaymentHash {
			t.Fatalf("status=%d reason=%q", status, sr.ErrorReason)
		}
	})

	t.Run("wrong network", func(t *testing.T) {
		p := base()
		p.Accepted.Network = Testnet3.CAIP2
		status, sr := post(t, p)
		if status != http.StatusPaymentRequired || sr.ErrorReason != ReasonInvalidNetwork {
			t.Fatalf("status=%d reason=%q", status, sr.ErrorReason)
		}
	})
}

// TestCrossPriceRejected: settlement binds the proof to the endpoint price —
// a proof for a cheaper challenge cannot settle a more expensive resource.
func TestCrossPriceRejected(t *testing.T) {
	ln := &fakeLN{}
	st := store.NewMemory()
	g := newTestGate(t, ln, st, nil)

	srv := httptest.NewServer(g.Require(goldenAtoms)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {})))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var pr x402.PaymentRequired
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &pr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ln.Pay()
	pp := x402.PaymentPayload{
		X402Version: 2,
		Accepted:    pr.Accepts[0],
		Payload:     x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
	}

	// Presenting the golden-priced proof at an endpoint requiring 10x is
	// rejected before anything is settled.
	if _, _, vErr := g.Settle(context.Background(), pp, goldenAtoms*10); vErr == nil ||
		vErr.Reason != ReasonInvalidRequirements {
		t.Fatalf("cross-price settle should reject, got %v", vErr)
	}
	// The same proof settles at (or below) its own price.
	if _, _, vErr := g.Settle(context.Background(), pp, goldenAtoms); vErr != nil {
		t.Fatalf("correct-price settle should pass: %v", vErr)
	}
}

// TestPaymentHashReusedPastWindow: a consumed payment hash re-presented after
// its challenge window is rejected as payment_hash_reused, while a challenge
// that expired unpaid keeps the generic invoice_expired. Access is refused
// either way; only the reason code differs.
func TestPaymentHashReusedPastWindow(t *testing.T) {
	// issue drives a 402 and returns the buyer's proof payload for it.
	issue := func(g *Gate) x402.PaymentPayload {
		t.Helper()
		srv := httptest.NewServer(g.Require(goldenAtoms)(http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {})))
		defer srv.Close()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		var pr x402.PaymentRequired
		if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &pr); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return x402.PaymentPayload{
			X402Version: 2,
			Accepted:    pr.Accepts[0],
			Payload:     x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
		}
	}

	t.Run("reused after consuming", func(t *testing.T) {
		ln := &fakeLN{}
		clk := time.Unix(1_700_000_000, 0)
		g := newTestGate(t, ln, store.NewMemory(), func() time.Time { return clk })
		pp := issue(g)
		ln.Pay()
		if _, _, vErr := g.Settle(context.Background(), pp, goldenAtoms); vErr != nil {
			t.Fatalf("first settle should pass: %v", vErr)
		}
		clk = clk.Add(2 * time.Hour) // past the 1h ChallengeTTL
		_, _, vErr := g.Settle(context.Background(), pp, goldenAtoms)
		if vErr == nil || vErr.Reason != ReasonPaymentHashReused {
			t.Fatalf("reuse past window: got %v, want %s", vErr, ReasonPaymentHashReused)
		}
	})

	t.Run("expired unpaid", func(t *testing.T) {
		ln := &fakeLN{}
		clk := time.Unix(1_700_000_000, 0)
		g := newTestGate(t, ln, store.NewMemory(), func() time.Time { return clk })
		pp := issue(g)
		ln.Pay()
		clk = clk.Add(2 * time.Hour) // expire before it is ever settled
		_, _, vErr := g.Settle(context.Background(), pp, goldenAtoms)
		if vErr == nil || vErr.Reason != ReasonInvoiceExpired {
			t.Fatalf("expired unpaid: got %v, want %s", vErr, ReasonInvoiceExpired)
		}
	})
}

// TestMCPFlow exercises the MCP helper surface: challenge tool result,
// payment via _meta, settlement response.
func TestMCPFlow(t *testing.T) {
	ln := &fakeLN{}
	st := store.NewMemory()
	g := newTestGate(t, ln, st, nil)
	ctx := context.Background()

	result, err := g.MCPChallenge(ctx, "process", "Example payable tool", goldenAtoms)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("tool result must be isError")
	}
	var pr x402.PaymentRequired
	if err := json.Unmarshal(result.StructuredContent, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Resource.URL != "mcp://tool/process" {
		t.Fatalf("resource url %q", pr.Resource.URL)
	}
	if result.Content[0].Text == "" ||
		!strings.Contains(result.Content[0].Text, `"x402Version":2`) {
		t.Fatal("content[0].text missing serialized challenge")
	}

	ln.Pay()
	pp := x402.PaymentPayload{
		X402Version: 2,
		Accepted:    pr.Accepts[0],
		Payload:     x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
	}
	meta := map[string]json.RawMessage{MetaPayment: x402.MustRaw(pp)}
	parsed, present, err := DecodeMetaPayment(meta)
	if !present || err != nil {
		t.Fatalf("DecodeMetaPayment: present=%v err=%v", present, err)
	}
	settle, already, vErr := g.MCPSettle(ctx, parsed, goldenAtoms)
	if vErr != nil || already || !settle.Success {
		t.Fatalf("MCPSettle: %+v already=%v err=%v", settle, already, vErr)
	}
	if settle.Transaction != goldenHash {
		t.Fatalf("transaction %q", settle.Transaction)
	}
}
