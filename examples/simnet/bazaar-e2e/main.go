// Command bazaar-e2e drives a live dcrbazaar against the simnet harness: it
// creates a real invoice on the seller node, pays it from the buyer node over
// the channel, and presents the resulting proof to the facilitator's /verify
// and /settle. It then checks /supported and the discovery index. The request
// and response shapes are the x402 v2 facilitator API.
//
// It expects DCR402_FAC_URL plus the seller/buyer endpoints the harness writes
// to env.sh. Run it via examples/simnet/harness.sh bazaar-e2e.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/karamble/dcr402/bazaar"
	"github.com/karamble/dcr402/examples/simnet/internal/buyerclient"
	dcr402 "github.com/karamble/dcr402/lib"
	dcr402lnd "github.com/karamble/dcr402/lib/dcrlnd"
	"github.com/karamble/dcr402/lib/x402"
)

const amountAtoms = 250000

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	facURL := env("DCR402_FAC_URL")

	// Seller side: create an invoice on the seller's own dcrlnd.
	step("seller: create an invoice on the seller dcrlnd")
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
	invoice, paymentHash, err := backend.CreateInvoice(ctx, amountAtoms, "bazaar-e2e", time.Hour)
	if err != nil {
		fail("create invoice: %v", err)
	}
	hashHex := hex.EncodeToString(paymentHash[:])

	// Buyer side: pay the invoice over the channel; the preimage is the proof.
	step("buyer: pay the invoice and obtain the preimage")
	conn, err := buyerclient.Dial(env("DCR402_E2E_BUYER_RPC"), env("DCR402_E2E_BUYER_TLS"), env("DCR402_E2E_BUYER_ADMIN_MAC"))
	if err != nil {
		fail("buyer dial: %v", err)
	}
	defer conn.Close()
	preimage, err := buyerclient.PayInvoice(ctx, conn, invoice)
	if err != nil {
		fail("pay invoice: %v", err)
	}

	// The payload a resource server would forward to a bazaar.
	reqs := x402.PaymentRequirements{
		Scheme:            x402.SchemeExact,
		Network:           dcr402.Simnet.CAIP2,
		Amount:            strconv.Itoa(amountAtoms),
		Asset:             x402.AssetDCR,
		PayTo:             env("DCR402_E2E_SELLER_PUBKEY"),
		MaxTimeoutSeconds: 3600,
		Extra: x402.MustRaw(x402.LightningExtra{
			AssetTransferMethod: x402.MethodLightning,
			Invoice:             invoice,
			PaymentHash:         hashHex,
		}),
	}
	pp := x402.PaymentPayload{
		X402Version: 2,
		Accepted:    reqs,
		Payload:     x402.MustRaw(x402.LightningPayload{Preimage: preimage, PaymentHash: hashHex}),
	}

	// 1. /verify accepts the real proof.
	step("POST /verify -> isValid")
	var vr x402.VerifyResponse
	postFac(ctx, facURL+"/verify", verifyBody(pp, reqs), &vr)
	if !vr.IsValid {
		fail("verify rejected a real payment: %+v", vr)
	}

	// 2. /settle notarizes with no funds movement; replay is idempotent.
	step("POST /settle -> success, then idempotent replay")
	var sr x402.SettlementResponse
	postFac(ctx, facURL+"/settle", verifyBody(pp, reqs), &sr)
	if !sr.Success || sr.Transaction != hashHex || sr.Network != dcr402.Simnet.CAIP2 {
		fail("settle: %+v", sr)
	}
	var replay x402.SettlementResponse
	postFac(ctx, facURL+"/settle", verifyBody(pp, reqs), &replay)
	if !replay.Success || replay.Transaction != hashHex {
		fail("settle replay: %+v", replay)
	}

	// 3. /supported advertises the exact scheme on simnet.
	step("GET /supported -> exact on simnet")
	var sup x402.SupportedResponse
	getFac(ctx, facURL+"/supported", &sup)
	if !hasKind(sup, dcr402.Simnet.CAIP2) {
		fail("supported lacks the simnet exact kind: %+v", sup.Kinds)
	}

	// 4. Discovery: a seller submits itself, then the index lists and finds it.
	step("submit a resource -> discovery lists and searches it")
	if err := bazaar.Submit(ctx, facURL, "", bazaar.SubmitRequest{
		Resource: "https://weather.example/simnet",
		Accepts:  []x402.PaymentRequirements{reqs},
		Metadata: x402.ResourceInfo{ServiceName: "Simnet Weather", Tags: []string{"weather", "simnet"}},
	}); err != nil {
		fail("submit: %v", err)
	}
	if n := listCount(ctx, facURL+"/discovery/resources", "items"); n < 1 {
		fail("discovery listed %d resources", n)
	}
	if n := listCount(ctx, facURL+"/discovery/search?query=weather", "resources"); n < 1 {
		fail("discovery search found %d resources", n)
	}

	// 5. A payment for a network the facilitator does not serve is rejected.
	step("POST /verify (wrong network) -> isValid:false invalid_network")
	badReqs := reqs
	badReqs.Network = dcr402.Testnet3.CAIP2
	badPP := pp
	badPP.Accepted = badReqs
	var badVR x402.VerifyResponse
	postFac(ctx, facURL+"/verify", verifyBody(badPP, badReqs), &badVR)
	if badVR.IsValid || badVR.InvalidReason != dcr402.ReasonInvalidNetwork {
		fail("wrong-network verify: %+v", badVR)
	}

	fmt.Println("PASS: dcrbazaar verified a real simnet payment, notarized it idempotently, advertised /supported, and indexed a resource for discovery")
}

// verifyBody assembles the {x402Version, paymentPayload, paymentRequirements}
// request the facilitator /verify and /settle endpoints expect.
func verifyBody(pp x402.PaymentPayload, reqs x402.PaymentRequirements) any {
	return map[string]any{
		"x402Version":         x402.Version,
		"paymentPayload":      pp,
		"paymentRequirements": reqs,
	}
}

func postFac(ctx context.Context, url string, body, out any) {
	b, err := json.Marshal(body)
	if err != nil {
		fail("marshal request: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		fail("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	do(url, req, out)
}

func getFac(ctx context.Context, url string, out any) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fail("build request: %v", err)
	}
	do(url, req, out)
}

func do(url string, req *http.Request, out any) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("%s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		fail("%s: status %d: %s", url, resp.StatusCode, msg)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		fail("decode %s: %v", url, err)
	}
}

func listCount(ctx context.Context, url, field string) int {
	var raw map[string]json.RawMessage
	getFac(ctx, url, &raw)
	var items []json.RawMessage
	_ = json.Unmarshal(raw[field], &items)
	return len(items)
}

func hasKind(sup x402.SupportedResponse, caip2 string) bool {
	for _, k := range sup.Kinds {
		if k.Network == caip2 && k.Scheme == x402.SchemeExact {
			return true
		}
	}
	return false
}

func env(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fail("missing env %s", key)
	}
	return v
}

func step(format string, a ...any) { fmt.Printf("... "+format+"\n", a...) }

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", a...)
	os.Exit(1)
}
