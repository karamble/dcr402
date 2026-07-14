// Command e2e is the dcr402 acceptance run: against the live simnet
// nodes stood up by harness.sh, it gates a real HTTP endpoint on the
// seller's dcrlnd, pays the challenge invoice from the buyer's dcrlnd over
// a real channel, redeems the preimage proof, and exercises credential
// reuse and replay idempotency. Exit 0 = PASS.
package main

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/decred/dcrlnd/lnrpc"
	"github.com/decred/dcrlnd/lnrpc/routerrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	dcr402 "github.com/karamble/dcr402/lib"
	dcr402lnd "github.com/karamble/dcr402/lib/dcrlnd"
	"github.com/karamble/dcr402/lib/ledger"
	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

const (
	priceAtoms  = 250000    // 0.0025 DCR per call (F1)
	topupAtoms  = 1_000_000 // 0.01 DCR top-up (F2)
	creditPrice = 100_000   // per credit-gated call (F2)
)

// mine asks the harness to mine n blocks (DCR402_E2E_MINE).
func mine(n int) {
	cmd := os.Getenv("DCR402_E2E_MINE")
	if cmd == "" {
		fail("missing DCR402_E2E_MINE (restart the harness: it writes env.sh)")
	}
	if out, err := exec.Command("sh", "-c", fmt.Sprintf("%s %d", cmd, n)).CombinedOutput(); err != nil {
		fail("mining: %v: %s", err, out)
	}
}

func env(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fail("missing environment variable %s (run via harness.sh e2e)", key)
	}
	return v
}

func fail(format string, args ...any) {
	fmt.Printf("\x1b[1;31mFAIL\x1b[0m "+format+"\n", args...)
	os.Exit(1)
}

func step(format string, args ...any) {
	fmt.Printf("\x1b[1;34m==>\x1b[0m "+format+"\n", args...)
}

type buyerCred struct{ hexMac string }

func (c buyerCred) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"macaroon": c.hexMac}, nil
}
func (c buyerCred) RequireTransportSecurity() bool { return true }

func dialBuyer() *grpc.ClientConn {
	certPEM, err := os.ReadFile(env("DCR402_E2E_BUYER_TLS"))
	if err != nil {
		fail("buyer tls cert: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		fail("buyer tls cert did not parse")
	}
	mac, err := os.ReadFile(env("DCR402_E2E_BUYER_ADMIN_MAC"))
	if err != nil {
		fail("buyer macaroon: %v", err)
	}
	conn, err := grpc.NewClient(env("DCR402_E2E_BUYER_RPC"),
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(pool, "")),
		grpc.WithPerRPCCredentials(buyerCred{hexMac: hex.EncodeToString(mac)}),
	)
	if err != nil {
		fail("buyer dial: %v", err)
	}
	return conn
}

// payInvoice settles the invoice through the buyer's node and returns the
// preimage hex — the client half of flow F1 step 3/4.
func payInvoice(ctx context.Context, router routerrpc.RouterClient, invoice string) string {
	stream, err := router.SendPaymentV2(ctx, &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice,
		TimeoutSeconds: 60,
		FeeLimitAtoms:  10000,
	})
	if err != nil {
		fail("SendPaymentV2: %v", err)
	}
	for {
		update, err := stream.Recv()
		if err != nil {
			fail("payment stream: %v", err)
		}
		switch update.Status {
		case lnrpc.Payment_SUCCEEDED:
			return update.PaymentPreimage
		case lnrpc.Payment_FAILED:
			fail("payment failed: %v", update.FailureReason)
		}
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// --- Seller side: a dcr402-gated HTTP service on the seller node. ---
	step("connecting seller gate to dcrlnd (invoice.macaroon only)")
	backend, err := dcr402lnd.New(dcr402lnd.Config{
		Host:         env("DCR402_E2E_SELLER_RPC"),
		TLSCertPath:  env("DCR402_E2E_SELLER_TLS"),
		MacaroonPath: env("DCR402_E2E_SELLER_INVOICE_MAC"),
		ChainParams:  dcr402.Simnet.Params,
	})
	if err != nil {
		fail("seller backend: %v", err)
	}
	defer backend.Close()

	dbDir, err := os.MkdirTemp("", "dcr402-e2e-*")
	if err != nil {
		fail("tempdir: %v", err)
	}
	defer os.RemoveAll(dbDir)
	st, err := store.OpenSQLite(filepath.Join(dbDir, "gate.db"))
	if err != nil {
		fail("store: %v", err)
	}
	defer st.Close()
	led, err := ledger.OpenSQLite(filepath.Join(dbDir, "ledger.db"))
	if err != nil {
		fail("ledger: %v", err)
	}
	defer led.Close()

	gate, err := dcr402.New(dcr402.Config{
		Backend:      backend,
		Onchain:      backend,
		Ledger:       led,
		Store:        st,
		Network:      dcr402.Simnet,
		PayTo:        env("DCR402_E2E_SELLER_PUBKEY"),
		Service:      "dcr402-simnet",
		TokenTTL:     time.Hour,
		ChallengeTTL: 10 * time.Minute,
		TopupPath:    "/topup",
	})
	if err != nil {
		fail("gate: %v", err)
	}

	var handlerRuns atomic.Int64
	mux := http.NewServeMux()
	mux.Handle("/paid", gate.Require(priceAtoms)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			handlerRuns.Add(1)
			fmt.Fprint(w, "PAID CONTENT")
		})))
	mux.Handle("/topup", gate.Topup(dcr402.TopupOptions{Confirmations: 1}))
	mux.Handle("/credits", gate.RequireCredits("simnet-credits", creditPrice)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "CREDIT CONTENT")
		})))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	step("gated endpoints at %s (/paid %d, /credits %d atoms, /topup)", srv.URL, priceAtoms, creditPrice)

	// --- Buyer side. ---
	buyerConn := dialBuyer()
	defer buyerConn.Close()
	router := routerrpc.NewRouterClient(buyerConn)

	// 1. Bare request → 402 with the triple challenge.
	step("requesting without payment")
	resp, err := http.Get(srv.URL + "/paid")
	if err != nil {
		fail("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		fail("expected 402, got %d", resp.StatusCode)
	}
	www := resp.Header.Values("Www-Authenticate")
	if len(www) != 2 || !strings.HasPrefix(www[0], "LSAT ") || !strings.HasPrefix(www[1], "L402 ") {
		fail("WWW-Authenticate challenges wrong: %v", www)
	}
	var pr x402.PaymentRequired
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &pr); err != nil {
		fail("PAYMENT-REQUIRED: %v", err)
	}
	entry := pr.Accepts[0]
	var extra x402.LightningExtra
	if err := json.Unmarshal(entry.Extra, &extra); err != nil {
		fail("challenge extra: %v", err)
	}
	if entry.Network != dcr402.Simnet.CAIP2 || entry.Asset != "DCR" ||
		entry.Amount != fmt.Sprint(priceAtoms) ||
		!strings.HasPrefix(extra.Invoice, "lnsdcr") {
		fail("challenge entry wrong: %+v", entry)
	}
	step("got challenge: %s… hash %s…", extra.Invoice[:24], extra.PaymentHash[:16])

	// 2. Pay the invoice over the real channel.
	step("paying invoice via buyer dcrlnd")
	preimage := payInvoice(ctx, router, extra.Invoice)
	step("payment SUCCEEDED, preimage %s…", preimage[:16])

	// 3. Redeem the proof.
	payload := x402.PaymentPayload{
		X402Version: x402.Version,
		Resource:    &pr.Resource,
		Accepted:    entry,
		Payload: x402.MustRaw(x402.LightningPayload{
			Preimage:    preimage,
			PaymentHash: extra.PaymentHash,
		}),
	}
	sig, err := x402.EncodeHeader(payload)
	if err != nil {
		fail("encode payload: %v", err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/paid", nil)
	req.Header.Set(x402.HeaderPaymentSignature, sig)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		fail("retry: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "PAID CONTENT" {
		fail("paid request: status %d body %q", resp.StatusCode, body)
	}
	var settle x402.SettlementResponse
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentResponse), &settle); err != nil {
		fail("PAYMENT-RESPONSE: %v", err)
	}
	if !settle.Success || settle.Transaction != extra.PaymentHash {
		fail("settlement wrong: %+v", settle)
	}
	step("proof accepted; settlement transaction = payment hash")

	// 4. Reuse the delivered credential (pay once, access N times).
	ext, ok := settle.Extensions[dcr402.ExtensionL402]
	if !ok {
		fail("settlement lacks l402 credential")
	}
	var cred struct {
		Authorization string `json:"authorization"`
	}
	if err := json.Unmarshal(ext.Info, &cred); err != nil {
		fail("credential: %v", err)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/paid", nil)
	req.Header.Set("Authorization", cred.Authorization)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		fail("token request: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "PAID CONTENT" {
		fail("token request: status %d body %q", resp.StatusCode, body)
	}
	step("credential grants access without payment")

	// 5. Replay of the consumed proof is idempotent, handler not re-run.
	runs := handlerRuns.Load()
	req, _ = http.NewRequest("GET", srv.URL+"/paid", nil)
	req.Header.Set(x402.HeaderPaymentSignature, sig)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		fail("replay: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "already_settled") {
		fail("replay: status %d body %q", resp.StatusCode, body)
	}
	if handlerRuns.Load() != runs {
		fail("replay re-executed the paid handler")
	}
	step("replay answered idempotently without re-executing the handler")

	// ------------------------------------------------------------------
	// F2 — the slow rail funds the fast rail: on-chain top-up, credits.
	// ------------------------------------------------------------------
	lnClient := lnrpc.NewLightningClient(buyerConn)

	step("F2: requesting top-up challenge (%d atoms)", topupAtoms)
	resp, err = http.Get(fmt.Sprintf("%s/topup?atoms=%d", srv.URL, topupAtoms))
	if err != nil {
		fail("topup GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		fail("topup challenge: status %d", resp.StatusCode)
	}
	var topupPR x402.PaymentRequired
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &topupPR); err != nil {
		fail("topup PAYMENT-REQUIRED: %v", err)
	}
	if len(topupPR.Accepts) != 2 {
		fail("expected lightning + onchain entries, got %d", len(topupPR.Accepts))
	}
	var onchainEntry x402.PaymentRequirements
	found := false
	for _, e := range topupPR.Accepts {
		if e.TransferMethod() == x402.MethodOnchain {
			onchainEntry, found = e, true
		}
	}
	if !found {
		fail("challenge lacks the onchain entry")
	}
	depositAddr := onchainEntry.PayTo
	step("depositing %d atoms on-chain to %s", topupAtoms, depositAddr)
	sc, err := lnClient.SendCoins(ctx, &lnrpc.SendCoinsRequest{
		Addr:   depositAddr,
		Amount: topupAtoms,
	})
	if err != nil {
		fail("SendCoins: %v", err)
	}
	step("broadcast %s", sc.Txid)

	topupPayload := x402.PaymentPayload{
		X402Version: x402.Version,
		Resource:    &topupPR.Resource,
		Accepted:    onchainEntry,
		Payload:     x402.MustRaw(x402.OnchainPayload{TxID: sc.Txid}),
	}
	topupSig, err := x402.EncodeHeader(topupPayload)
	if err != nil {
		fail("encode topup payload: %v", err)
	}

	settleTopup := func() (int, map[string]any, http.Header) {
		req, _ := http.NewRequest("GET", srv.URL+"/topup", nil)
		req.Header.Set(x402.HeaderPaymentSignature, topupSig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fail("topup settle: %v", err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			fail("topup settle body: %v", err)
		}
		return resp.StatusCode, body, resp.Header
	}

	sawPending := false
	mined := false
	var topupHeader http.Header
	var topupBody map[string]any
	deadline := time.Now().Add(90 * time.Second)
	for {
		if time.Now().After(deadline) {
			fail("top-up did not settle in time (last: %v)", topupBody)
		}
		var status int
		status, topupBody, topupHeader = settleTopup()
		if status == http.StatusOK {
			break
		}
		if topupBody["status"] == "pending" {
			sawPending = true
			if !mined {
				step("deposit pending (%v) — mining a block", topupBody["reason"])
				mine(1)
				mined = true
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		fail("unexpected top-up response %d: %v", status, topupBody)
	}
	if !sawPending {
		fail("never observed the retryable insufficient_confirmations path")
	}
	if topupBody["status"] != "credited" ||
		topupBody["balanceAtoms"].(float64) != topupAtoms ||
		topupBody["rail"] != x402.MethodOnchain {
		fail("top-up outcome wrong: %v", topupBody)
	}
	step("credited %d atoms on-chain (account %v)", topupAtoms, topupBody["account"])

	var topupSettle x402.SettlementResponse
	if err := x402.DecodeHeader(topupHeader.Get(x402.HeaderPaymentResponse), &topupSettle); err != nil {
		fail("topup PAYMENT-RESPONSE: %v", err)
	}
	if topupSettle.Transaction != sc.Txid {
		fail("settlement transaction %q != txid", topupSettle.Transaction)
	}
	var acctCred struct {
		Authorization string `json:"authorization"`
	}
	if err := json.Unmarshal(topupSettle.Extensions[dcr402.ExtensionL402].Info, &acctCred); err != nil {
		fail("account credential: %v", err)
	}

	step("burning credits (%d atoms per call, no payment latency)", creditPrice)
	wantBalances := []int64{topupAtoms - creditPrice, topupAtoms - 2*creditPrice}
	for i, want := range wantBalances {
		start := time.Now()
		req, _ := http.NewRequest("GET", srv.URL+"/credits", nil)
		req.Header.Set("Authorization", acctCred.Authorization)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fail("credit call: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "CREDIT CONTENT" {
			fail("credit call %d: status %d body %q", i, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Dcr402-Balance"); got != fmt.Sprint(want) {
			fail("credit call %d: balance %s, want %d", i, got, want)
		}
		step("  call %d served in %s, balance %d atoms", i+1, time.Since(start).Round(time.Millisecond), want)
	}

	// Replaying the consumed deposit is idempotent — and lands on the same
	// account, whose balance now reflects the burns.
	status, replayBody, _ := settleTopup()
	if status != http.StatusOK || replayBody["status"] != "already_credited" ||
		replayBody["balanceAtoms"].(float64) != float64(topupAtoms-2*creditPrice) {
		fail("deposit replay: status %d body %v", status, replayBody)
	}
	step("deposit replay answered idempotently, no double credit")

	fmt.Printf("\x1b[1;32mPASS\x1b[0m dcr402 acceptance on simnet: F1 per-call + "+
		"F2 on-chain top-up and credit burn (%d paid handler executions)\n", handlerRuns.Load())
}
