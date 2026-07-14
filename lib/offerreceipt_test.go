package dcr402

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

func verifyJWS(t *testing.T, pub ed25519.PublicKey, compact string) map[string]any {
	t.Helper()
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		t.Fatalf("not a compact JWS: %q", compact)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("JWS signature does not verify")
	}
	pj, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(pj, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestOfferReceipt(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ln := &fakeLN{}
	g, err := New(Config{
		Backend: ln, Store: store.NewMemory(), Network: Mainnet, PayTo: goldenDest,
		Service: "or", TokenTTL: time.Hour, ChallengeTTL: time.Hour, OfferReceiptKey: priv,
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

	ext, ok := pr.Extensions[ExtensionOfferReceipt]
	if !ok {
		t.Fatalf("no offer-receipt extension advertised: %v", pr.Extensions)
	}
	var oinfo struct {
		Offers []struct {
			Format, Signature string
		} `json:"offers"`
	}
	if err := json.Unmarshal(ext.Info, &oinfo); err != nil {
		t.Fatal(err)
	}
	if len(oinfo.Offers) != 1 || oinfo.Offers[0].Format != "jws" {
		t.Fatalf("offers wrong: %+v", oinfo.Offers)
	}
	op := verifyJWS(t, pub, oinfo.Offers[0].Signature)
	if op["payTo"] != goldenDest || op["scheme"] != "exact" {
		t.Fatalf("offer payload wrong: %v", op)
	}

	ln.Pay()
	pp := x402.PaymentPayload{
		X402Version: 2, Accepted: pr.Accepts[0],
		Resource: &x402.ResourceInfo{URL: "https://api.example.com/x"},
		Payload:  x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
	}
	settle, _, vErr := g.Settle(context.Background(), pp, goldenAtoms)
	if vErr != nil {
		t.Fatalf("settle: %v", vErr)
	}
	rext, ok := settle.Extensions[ExtensionOfferReceipt]
	if !ok {
		t.Fatal("no receipt in settlement")
	}
	var rinfo struct {
		Receipt struct {
			Format, Signature string
		} `json:"receipt"`
	}
	if err := json.Unmarshal(rext.Info, &rinfo); err != nil {
		t.Fatal(err)
	}
	if rinfo.Receipt.Format != "jws" {
		t.Fatalf("receipt wrong: %+v", rinfo.Receipt)
	}
	rp := verifyJWS(t, pub, rinfo.Receipt.Signature)
	if rp["resourceUrl"] != "https://api.example.com/x" {
		t.Fatalf("receipt payload wrong: %v", rp)
	}
}

func TestOfferReceiptAbsentWithoutKey(t *testing.T) {
	g, err := New(Config{
		Backend: &fakeLN{}, Store: store.NewMemory(), Network: Mainnet, PayTo: goldenDest,
		Service: "nokey", TokenTTL: time.Hour, ChallengeTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(g.Require(goldenAtoms)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {})))
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	var pr x402.PaymentRequired
	_ = x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &pr)
	resp.Body.Close()
	if _, ok := pr.Extensions[ExtensionOfferReceipt]; ok {
		t.Fatal("offer-receipt advertised without a signing key")
	}
}
