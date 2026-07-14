package dcr402

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

func TestPaymentIDAdvertisedAndValidated(t *testing.T) {
	ln := &fakeLN{}
	g, err := New(Config{
		Backend: ln, Store: store.NewMemory(), Network: Mainnet, PayTo: goldenDest,
		Service: "pid", TokenTTL: time.Hour, ChallengeTTL: time.Hour,
		PaymentID: PaymentIDConfig{Enabled: true, Required: true},
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if _, ok := pr.Extensions[ExtensionPaymentID]; !ok {
		t.Fatalf("payment-identifier not advertised: %v", pr.Extensions)
	}

	ln.Pay()
	base := x402.PaymentPayload{
		X402Version: 2, Accepted: pr.Accepts[0],
		Payload: x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
	}
	send := func(pp x402.PaymentPayload) int {
		sig, _ := x402.EncodeHeader(pp)
		req, _ := http.NewRequest("GET", srv.URL, nil)
		req.Header.Set(x402.HeaderPaymentSignature, sig)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	withID := func(id string) x402.PaymentPayload {
		pp := base
		pp.Extensions = map[string]x402.Extension{
			ExtensionPaymentID: {Info: x402.MustRaw(map[string]string{"id": id})},
		}
		return pp
	}

	if code := send(base); code != http.StatusBadRequest {
		t.Fatalf("missing required id: status %d, want 400", code)
	}
	if code := send(withID("short")); code != http.StatusPaymentRequired {
		t.Fatalf("malformed id: status %d, want 402", code)
	}
	if code := send(withID("abcdefghij1234567890")); code != http.StatusOK {
		t.Fatalf("valid id: status %d, want 200", code)
	}
}
