package bazaar

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/x402"
)

// Golden vector (scheme/test-vectors/): the only invoice with a known
// preimage, signed by the published BOLT11 test key, created 2017-06-01.
const (
	goldenInvoice = "lndcr2500u1pvjluezpp5qt2yngclhvn8ere496vk3fu78e0ujhqmh649qt7kg48tmedyhmwqdpqv33hydpsxgsxwmmvv3jkugrkv43hgmmjxqrrssdztqxz9z9ys3q59ml9270ej0t9wt62442s3tzzldns0a6j247qkkqzakysm9yz75xqze4a7r3h7tsys8tcugay7f8sru2l8a7s07srcqc5t8f7"
	goldenHash    = "02d449a31fbb267c8f352e9968a79e3e5fc95c1bbeaa502fd6454ebde5a4bedc"
	goldenPre     = "1111111111111111111111111111111111111111111111111111111111111111"
	goldenDest    = "03e7156ae33b0a208d0744199163177e909e80176e55d97a2f221ede0f934dd9ad"
)

// goldenClock returns a time inside the golden invoice's validity window
// (created 1496314658, 3600 s expiry) so the facilitator's rule 7 passes.
func goldenClock() time.Time { return time.Unix(1496314658+60, 0) }

func goldenRequirements() x402.PaymentRequirements {
	return x402.PaymentRequirements{
		Scheme:            x402.SchemeExact,
		Network:           dcr402.Mainnet.CAIP2,
		Amount:            "250000",
		Asset:             x402.AssetDCR,
		PayTo:             goldenDest,
		MaxTimeoutSeconds: 3600,
		Extra: x402.MustRaw(x402.LightningExtra{
			AssetTransferMethod: x402.MethodLightning,
			Invoice:             goldenInvoice,
			PaymentHash:         goldenHash,
		}),
	}
}

func goldenRequest() verifyRequest {
	req := goldenRequirements()
	return verifyRequest{
		X402Version: 2,
		PaymentPayload: x402.PaymentPayload{
			X402Version: 2,
			Accepted:    req,
			Payload:     x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
		},
		PaymentRequirements: req,
	}
}

func newTestService(t *testing.T, cfg Config) *Service {
	t.Helper()
	svc, err := New(cfg, NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	svc.nowFn = goldenClock
	return svc
}

func TestVerify(t *testing.T) {
	svc := newTestService(t, Config{Networks: []string{"mainnet"}})
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	var ok x402.VerifyResponse
	postJSON(t, srv.URL+"/verify", goldenRequest(), &ok)
	if !ok.IsValid {
		t.Fatalf("valid proof rejected: %+v", ok)
	}

	bad := goldenRequest()
	bad.PaymentPayload.Accepted.Amount = "1" // preimage still valid; equality fails
	var badVR x402.VerifyResponse
	postJSON(t, srv.URL+"/verify", bad, &badVR)
	if badVR.IsValid || badVR.InvalidReason != dcr402.ReasonInvalidRequirements {
		t.Fatalf("tampered amount: %+v", badVR)
	}

	wrong := goldenRequest()
	wrong.PaymentPayload.Accepted.Network = dcr402.Testnet3.CAIP2
	var netVR x402.VerifyResponse
	postJSON(t, srv.URL+"/verify", wrong, &netVR)
	if netVR.IsValid || netVR.InvalidReason != dcr402.ReasonInvalidNetwork {
		t.Fatalf("unsupported network: %+v", netVR)
	}

	badPre := goldenRequest()
	badPre.PaymentPayload.Payload = x402.MustRaw(x402.LightningPayload{
		Preimage:    "2222222222222222222222222222222222222222222222222222222222222222",
		PaymentHash: goldenHash,
	})
	var preVR x402.VerifyResponse
	postJSON(t, srv.URL+"/verify", badPre, &preVR)
	if preVR.IsValid || preVR.InvalidReason != dcr402.ReasonPreimageMismatch {
		t.Fatalf("wrong preimage: %+v", preVR)
	}
}

func TestSettle(t *testing.T) {
	svc := newTestService(t, Config{Networks: []string{"mainnet"}})
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	var sr x402.SettlementResponse
	postJSON(t, srv.URL+"/settle", goldenRequest(), &sr)
	if !sr.Success || sr.Transaction != goldenHash ||
		sr.Network != dcr402.Mainnet.CAIP2 || sr.Amount != "250000" {
		t.Fatalf("settle: %+v", sr)
	}

	// Idempotent notarization: a second settle returns the same response.
	var replay x402.SettlementResponse
	postJSON(t, srv.URL+"/settle", goldenRequest(), &replay)
	if !replay.Success || replay.Transaction != goldenHash {
		t.Fatalf("settle replay: %+v", replay)
	}

	// A rejected payment settles to success:false with the reason, HTTP 200.
	bad := goldenRequest()
	bad.PaymentPayload.Accepted.Network = dcr402.Testnet3.CAIP2
	var badSR x402.SettlementResponse
	postJSON(t, srv.URL+"/settle", bad, &badSR)
	if badSR.Success || badSR.ErrorReason != dcr402.ReasonInvalidNetwork {
		t.Fatalf("rejected settle: %+v", badSR)
	}
}

func TestSupported(t *testing.T) {
	svc := newTestService(t, Config{Networks: []string{"mainnet", "simnet"}})
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/supported")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sup x402.SupportedResponse
	if err := json.NewDecoder(resp.Body).Decode(&sup); err != nil {
		t.Fatal(err)
	}
	if len(sup.Kinds) != 2 {
		t.Fatalf("kinds: %+v", sup.Kinds)
	}
	k := sup.Kinds[0]
	if k.X402Version != 2 || k.Scheme != x402.SchemeExact || k.Network != dcr402.Mainnet.CAIP2 {
		t.Fatalf("kind 0: %+v", k)
	}
	if sup.Kinds[1].Network != dcr402.Simnet.CAIP2 {
		t.Fatalf("kind 1: %+v", sup.Kinds[1])
	}
}

func TestAPIKeyGate(t *testing.T) {
	svc := newTestService(t, Config{Networks: []string{"mainnet"}, APIKeys: []string{"secret"}})
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	body, _ := json.Marshal(goldenRequest())

	resp, err := http.Post(srv.URL+"/verify", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing key: status %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest("POST", srv.URL+"/verify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "secret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("valid key: status %d, want 200", resp2.StatusCode)
	}

	// /supported stays public even with keys configured.
	sup, err := http.Get(srv.URL + "/supported")
	if err != nil {
		t.Fatal(err)
	}
	sup.Body.Close()
	if sup.StatusCode != http.StatusOK {
		t.Fatalf("supported with keys: status %d", sup.StatusCode)
	}
}

func postJSON(t *testing.T, url string, reqBody, out any) {
	t.Helper()
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}
