package dcr402

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/karamble/dcr402/lib/ledger"
	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

// fakeOnchain is an OnchainBackend with scriptable deposits.
type fakeOnchain struct {
	mu       sync.Mutex
	nextAddr int
	deposits map[string]fakeDeposit // by txid
}

type fakeDeposit struct {
	address string
	confs   int32
	amount  int64
}

func newFakeOnchain() *fakeOnchain {
	return &fakeOnchain{deposits: make(map[string]fakeDeposit)}
}

func (f *fakeOnchain) NewDepositAddress(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextAddr++
	return fmt.Sprintf("SsFakeDeposit%06d", f.nextAddr), nil
}

func (f *fakeOnchain) LookupDeposit(_ context.Context, txid, address string) (DepositStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dep, ok := f.deposits[txid]
	if !ok {
		return DepositStatus{}, nil
	}
	st := DepositStatus{Found: true, Confirmations: dep.confs}
	if dep.address == address {
		st.AmountToAddressAtoms = dep.amount
	}
	return st, nil
}

func (f *fakeOnchain) put(txid string, dep fakeDeposit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deposits[txid] = dep
}

type topupRig struct {
	gate    *Gate
	ledger  *ledger.Memory
	onchain *fakeOnchain
	srv     *httptest.Server
}

func newTopupRig(t *testing.T) *topupRig {
	t.Helper()
	rig := &topupRig{ledger: ledger.NewMemory(), onchain: newFakeOnchain()}
	g, err := New(Config{
		Backend:      &fakeLN{settled: true},
		Onchain:      rig.onchain,
		Ledger:       rig.ledger,
		Store:        store.NewMemory(),
		Network:      Mainnet,
		PayTo:        goldenDest,
		Service:      "example",
		TokenTTL:     time.Hour,
		ChallengeTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	rig.gate = g

	mux := http.NewServeMux()
	mux.Handle("/topup", g.Topup(TopupOptions{Confirmations: 2}))
	mux.Handle("/credits", g.RequireCredits("demo", 200_000)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "CREDIT CONTENT")
		})))
	rig.srv = httptest.NewServer(mux)
	t.Cleanup(rig.srv.Close)
	return rig
}

func getJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("body JSON: %v", err)
	}
	return v
}

func (rig *topupRig) challenge(t *testing.T, atoms int64) x402.PaymentRequired {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/topup?atoms=%d", rig.srv.URL, atoms))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("topup challenge: status %d", resp.StatusCode)
	}
	var pr x402.PaymentRequired
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &pr); err != nil {
		t.Fatal(err)
	}
	return pr
}

func (rig *topupRig) settle(t *testing.T, pp x402.PaymentPayload) (*http.Response, map[string]any) {
	t.Helper()
	sig, err := x402.EncodeHeader(pp)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", rig.srv.URL+"/topup", nil)
	req.Header.Set(x402.HeaderPaymentSignature, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, getJSON(t, resp)
}

func onchainEntry(t *testing.T, pr x402.PaymentRequired) x402.PaymentRequirements {
	t.Helper()
	for _, e := range pr.Accepts {
		if e.TransferMethod() == x402.MethodOnchain {
			return e
		}
	}
	t.Fatal("challenge lacks an onchain entry")
	return x402.PaymentRequirements{}
}

// TestTopupOnchainFlow drives F2's on-chain half: pending depth, exact
// amount, credit grant, credential, idempotent replay.
func TestTopupOnchainFlow(t *testing.T) {
	rig := newTopupRig(t)
	const atoms = 500_000
	txid := strings.Repeat("ab", 32)

	pr := rig.challenge(t, atoms)
	if len(pr.Accepts) != 2 {
		t.Fatalf("expected 2 accepts entries, got %d", len(pr.Accepts))
	}
	entry := onchainEntry(t, pr)
	if entry.Amount != "500000" || entry.PayTo == "" {
		t.Fatalf("onchain entry wrong: %+v", entry)
	}
	var extra x402.OnchainExtra
	if err := json.Unmarshal(entry.Extra, &extra); err != nil || extra.Confirmations != 2 {
		t.Fatalf("onchain extra wrong: %+v err=%v", extra, err)
	}

	pp := x402.PaymentPayload{
		X402Version: 2,
		Accepted:    entry,
		Payload:     x402.MustRaw(x402.OnchainPayload{TxID: txid}),
	}

	// Unknown tx: retryable pending, nothing consumed.
	resp, body := rig.settle(t, pp)
	if resp.StatusCode != http.StatusPaymentRequired || body["status"] != "pending" {
		t.Fatalf("unknown tx: status %d body %v", resp.StatusCode, body)
	}

	// Mempool (0 conf): still pending.
	rig.onchain.put(txid, fakeDeposit{address: entry.PayTo, confs: 0, amount: atoms})
	resp, body = rig.settle(t, pp)
	if resp.StatusCode != http.StatusPaymentRequired || body["status"] != "pending" {
		t.Fatalf("0-conf: status %d body %v", resp.StatusCode, body)
	}

	// Deep enough: credited.
	rig.onchain.put(txid, fakeDeposit{address: entry.PayTo, confs: 2, amount: atoms})
	resp, body = rig.settle(t, pp)
	if resp.StatusCode != http.StatusOK || body["status"] != "credited" {
		t.Fatalf("settle: status %d body %v", resp.StatusCode, body)
	}
	if body["balanceAtoms"].(float64) != atoms {
		t.Fatalf("balance %v, want %d", body["balanceAtoms"], atoms)
	}
	var settleResp x402.SettlementResponse
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentResponse), &settleResp); err != nil {
		t.Fatal(err)
	}
	if !settleResp.Success || settleResp.Transaction != txid {
		t.Fatalf("settlement response wrong: %+v", settleResp)
	}
	if _, ok := settleResp.Extensions[ExtensionL402]; !ok {
		t.Fatal("settlement lacks the account credential")
	}

	// Replay: idempotent, no double credit.
	resp, body = rig.settle(t, pp)
	if resp.StatusCode != http.StatusOK || body["status"] != "already_credited" {
		t.Fatalf("replay: status %d body %v", resp.StatusCode, body)
	}
	if body["balanceAtoms"].(float64) != atoms {
		t.Fatalf("replay balance %v, want %d", body["balanceAtoms"], atoms)
	}
}

// TestTopupOnchainAmountMismatch: a confirmed deposit of the wrong amount
// is terminal, not pending.
func TestTopupOnchainAmountMismatch(t *testing.T) {
	rig := newTopupRig(t)
	pr := rig.challenge(t, 300_000)
	entry := onchainEntry(t, pr)
	txid := strings.Repeat("cd", 32)
	rig.onchain.put(txid, fakeDeposit{address: entry.PayTo, confs: 2, amount: 299_999})

	pp := x402.PaymentPayload{
		X402Version: 2,
		Accepted:    entry,
		Payload:     x402.MustRaw(x402.OnchainPayload{TxID: txid}),
	}
	resp, _ := rig.settle(t, pp)
	var sr x402.SettlementResponse
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentResponse), &sr); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPaymentRequired || sr.ErrorReason != ReasonAmountMismatch {
		t.Fatalf("status %d reason %q", resp.StatusCode, sr.ErrorReason)
	}
	if b, _ := rig.ledger.Balance(context.Background(), "any"); b != 0 {
		t.Fatal("mismatched deposit credited something")
	}
}

// TestTopupOnchainCredentialBinding: a settler who reconstructs `accepted`
// from the public transaction but lacks the challenge credential is rejected,
// while the genuine 402 recipient (echoing the full entry) settles.
func TestTopupOnchainCredentialBinding(t *testing.T) {
	rig := newTopupRig(t)
	const atoms = 400_000
	txid := strings.Repeat("ef", 32)

	pr := rig.challenge(t, atoms)
	entry := onchainEntry(t, pr)
	rig.onchain.put(txid, fakeDeposit{address: entry.PayTo, confs: 2, amount: atoms})

	var extra x402.OnchainExtra
	if err := json.Unmarshal(entry.Extra, &extra); err != nil || extra.Credential == "" {
		t.Fatalf("onchain challenge must carry a credential secret: %+v", extra)
	}

	// Observer: same public fields, but no credential (never received the 402).
	forged := entry
	forged.Extra = x402.MustRaw(x402.OnchainExtra{
		AssetTransferMethod: x402.MethodOnchain,
		Confirmations:       extra.Confirmations,
	})
	resp, _ := rig.settle(t, x402.PaymentPayload{
		X402Version: 2, Accepted: forged,
		Payload: x402.MustRaw(x402.OnchainPayload{TxID: txid}),
	})
	var sr x402.SettlementResponse
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentResponse), &sr); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPaymentRequired || sr.ErrorReason != ReasonInvalidRequirements {
		t.Fatalf("observer without the credential should be rejected: status %d reason %q",
			resp.StatusCode, sr.ErrorReason)
	}

	// Genuine recipient: echoes the full entry, settles.
	_, body := rig.settle(t, x402.PaymentPayload{
		X402Version: 2, Accepted: entry,
		Payload: x402.MustRaw(x402.OnchainPayload{TxID: txid}),
	})
	if body["status"] != "credited" {
		t.Fatalf("genuine settle should credit: %v", body)
	}
}

// TestTopupOnchainNotStrandedPastTTL: a confirmed deposit is credited even
// when its confirmation depth lands after the challenge TTL — the funds are
// committed on-chain, so the deposit is not stranded as expired.
func TestTopupOnchainNotStrandedPastTTL(t *testing.T) {
	now := time.Now()
	onchain := newFakeOnchain()
	led := ledger.NewMemory()
	g, err := New(Config{
		Backend: &fakeLN{settled: true}, Onchain: onchain, Ledger: led,
		Store: store.NewMemory(), Network: Mainnet, PayTo: goldenDest,
		Service: "example", TokenTTL: time.Hour, ChallengeTTL: time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/topup", g.Topup(TopupOptions{Confirmations: 2}))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/topup?atoms=250000")
	if err != nil {
		t.Fatal(err)
	}
	var pr x402.PaymentRequired
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &pr); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	entry := onchainEntry(t, pr)
	txid := strings.Repeat("77", 32)
	onchain.put(txid, fakeDeposit{address: entry.PayTo, confs: 2, amount: 250000})

	now = now.Add(2 * time.Minute) // past the 1-minute challenge TTL

	sig, err := x402.EncodeHeader(x402.PaymentPayload{
		X402Version: 2, Accepted: entry,
		Payload: x402.MustRaw(x402.OnchainPayload{TxID: txid}),
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/topup", nil)
	req.Header.Set(x402.HeaderPaymentSignature, sig)
	sResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := getJSON(t, sResp)
	if sResp.StatusCode != http.StatusOK || body["status"] != "credited" {
		t.Fatalf("a confirmed deposit past the TTL must credit, got status %d body %v",
			sResp.StatusCode, body)
	}
}

// TestTopupLightningRail settles a top-up over the lightning method and
// checks the grant.
func TestTopupLightningRail(t *testing.T) {
	rig := newTopupRig(t)
	pr := rig.challenge(t, goldenAtoms) // must match the golden invoice
	entry := pr.Accepts[0]
	if entry.TransferMethod() != x402.MethodLightning {
		t.Fatal("first entry is not lightning")
	}
	pp := x402.PaymentPayload{
		X402Version: 2,
		Accepted:    entry,
		Payload: x402.MustRaw(x402.LightningPayload{
			Preimage:    goldenPre,
			PaymentHash: goldenHash,
		}),
	}
	resp, body := rig.settle(t, pp)
	if resp.StatusCode != http.StatusOK || body["status"] != "credited" ||
		body["rail"] != x402.MethodLightning {
		t.Fatalf("lightning topup: status %d body %v", resp.StatusCode, body)
	}
	if body["balanceAtoms"].(float64) != goldenAtoms {
		t.Fatalf("balance %v", body["balanceAtoms"])
	}
}

// TestRequireCredits burns a topped-up balance down to the machine-readable
// insufficient_credits error.
func TestRequireCredits(t *testing.T) {
	rig := newTopupRig(t)
	const atoms = 500_000 // 2 × 200k charges + 100k remainder
	txid := strings.Repeat("ee", 32)
	pr := rig.challenge(t, atoms)
	entry := onchainEntry(t, pr)
	rig.onchain.put(txid, fakeDeposit{address: entry.PayTo, confs: 2, amount: atoms})
	resp, _ := rig.settle(t, x402.PaymentPayload{
		X402Version: 2,
		Accepted:    entry,
		Payload:     x402.MustRaw(x402.OnchainPayload{TxID: txid}),
	})
	var sr x402.SettlementResponse
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentResponse), &sr); err != nil {
		t.Fatal(err)
	}
	var cred struct {
		Authorization string `json:"authorization"`
	}
	if err := json.Unmarshal(sr.Extensions[ExtensionL402].Info, &cred); err != nil {
		t.Fatal(err)
	}

	call := func(auth string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", rig.srv.URL+"/credits", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	// Two calls burn 400k with zero payment latency.
	for i, wantBal := range []string{"300000", "100000"} {
		resp, body := call(cred.Authorization)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "CREDIT CONTENT") {
			t.Fatalf("call %d: status %d body %q", i, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Dcr402-Balance"); got != wantBal {
			t.Fatalf("call %d: balance header %q, want %s", i, got, wantBal)
		}
	}

	// Third call: 100k left < 200k price → exact shortfall + topup hint.
	resp2, body := call(cred.Authorization)
	if resp2.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("insufficient: status %d", resp2.StatusCode)
	}
	var insuff map[string]any
	if err := json.Unmarshal([]byte(body), &insuff); err != nil {
		t.Fatal(err)
	}
	if insuff["error"] != "insufficient_credits" ||
		insuff["shortfallAtoms"].(float64) != 100_000 ||
		insuff["topup"] != "/topup" {
		t.Fatalf("insufficient body: %v", insuff)
	}

	// No credential → payment_required with topup pointer; garbage → 401.
	respNo, bodyNo := call("")
	if respNo.StatusCode != http.StatusPaymentRequired || !strings.Contains(bodyNo, "topup") {
		t.Fatalf("no-auth: status %d body %q", respNo.StatusCode, bodyNo)
	}
	respBad, _ := call("Bearer nonsense")
	if respBad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad scheme: status %d", respBad.StatusCode)
	}

	// ChargeCredential directly: the typed-error contract the gateway
	// builds on.
	ctx := context.Background()
	if _, err := rig.gate.ChargeCredential(ctx, "", "t", 1, ""); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("empty auth: %v", err)
	}
	if _, err := rig.gate.ChargeCredential(ctx, "Bearer x", "t", 1, ""); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("bearer: %v", err)
	}
	balance, err := rig.gate.ChargeCredential(ctx, cred.Authorization, "t", 50_000, "")
	if err != nil || balance != 50_000 {
		t.Fatalf("valid charge: balance=%d err=%v", balance, err)
	}
	var insufficient2 *ledger.InsufficientBalanceError
	if _, err := rig.gate.ChargeCredential(ctx, cred.Authorization, "t", 200_000, ""); !errors.As(err, &insufficient2) {
		t.Fatalf("expected insufficient error, got %v", err)
	}
	if insufficient2.Shortfall() != 150_000 {
		t.Fatalf("shortfall %d", insufficient2.Shortfall())
	}
}

// TestTopupChallengeLedgerFree exercises the exported ledger-free mint: both
// methods offered, no ledger required, and the challenge is settleable.
func TestTopupChallengeLedgerFree(t *testing.T) {
	onchain := newFakeOnchain()
	g, err := New(Config{
		Backend:      &fakeLN{settled: true},
		Onchain:      onchain,
		Store:        store.NewMemory(),
		Network:      Mainnet,
		PayTo:        goldenDest,
		Service:      "example",
		TokenTTL:     time.Hour,
		ChallengeTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := x402.ResourceInfo{URL: "https://svc.example/topup?usd_micros=5000000", MimeType: "application/json"}
	pr, err := g.TopupChallenge(context.Background(), res, 500_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Accepts) != 2 {
		t.Fatalf("accepts = %d, want 2 (lightning + onchain)", len(pr.Accepts))
	}
	methods := map[string]bool{}
	for _, a := range pr.Accepts {
		methods[a.TransferMethod()] = true
	}
	if !methods[x402.MethodLightning] || !methods[x402.MethodOnchain] {
		t.Fatalf("methods = %v", methods)
	}
	// Without an onchain backend only lightning is offered.
	gLN, err := New(Config{
		Backend:      &fakeLN{settled: true},
		Store:        store.NewMemory(),
		Network:      Mainnet,
		PayTo:        goldenDest,
		Service:      "example",
		TokenTTL:     time.Hour,
		ChallengeTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	prLN, err := gLN.TopupChallenge(context.Background(), res, 500_000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(prLN.Accepts) != 1 || prLN.Accepts[0].TransferMethod() != x402.MethodLightning {
		t.Fatalf("lightning-only accepts = %+v", prLN.Accepts)
	}
}
