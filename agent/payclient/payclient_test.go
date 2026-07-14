package payclient

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dcr402 "github.com/karamble/dcr402/lib"
	libstore "github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"

	"github.com/karamble/dcr402/agent/policy"
	astore "github.com/karamble/dcr402/agent/store"
)

// Golden vector constants (scheme/test-vectors/).
const (
	goldenInvoice = "lndcr2500u1pvjluezpp5qt2yngclhvn8ere496vk3fu78e0ujhqmh649qt7kg48tmedyhmwqdpqv33hydpsxgsxwmmvv3jkugrkv43hgmmjxqrrssdztqxz9z9ys3q59ml9270ej0t9wt62442s3tzzldns0a6j247qkkqzakysm9yz75xqze4a7r3h7tsys8tcugay7f8sru2l8a7s07srcqc5t8f7"
	goldenHash    = "02d449a31fbb267c8f352e9968a79e3e5fc95c1bbeaa502fd6454ebde5a4bedc"
	goldenPre     = "1111111111111111111111111111111111111111111111111111111111111111"
	goldenDest    = "03e7156ae33b0a208d0744199163177e909e80176e55d97a2f221ede0f934dd9ad"
	goldenAtoms   = 250000
)

type fakeGateLN struct{}

func (fakeGateLN) CreateInvoice(context.Context, int64, string, time.Duration) (string, [32]byte, error) {
	var hash [32]byte
	raw, _ := hex.DecodeString(goldenHash)
	copy(hash[:], raw)
	return goldenInvoice, hash, nil
}

func (fakeGateLN) LookupInvoice(context.Context, [32]byte) (dcr402.InvoiceStatus, error) {
	return dcr402.InvoiceStatus{Settled: true, AmtPaidMAtoms: goldenAtoms * 1000}, nil
}

type fakePayer struct{ calls atomic.Int64 }

func (f *fakePayer) Pay(context.Context, string, int64, time.Duration) (string, int64, error) {
	f.calls.Add(1)
	return goldenPre, 2, nil
}

type rig struct {
	client   *Client
	srv      *httptest.Server
	payer    *fakePayer
	handled  atomic.Int64
	gateErr  error
	attempts atomic.Int64
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{payer: &fakePayer{}}

	gate, err := dcr402.New(dcr402.Config{
		Backend:      fakeGateLN{},
		Store:        libstore.NewMemory(),
		Network:      dcr402.Mainnet,
		PayTo:        goldenDest,
		Service:      "payclient-test",
		TokenTTL:     time.Hour,
		ChallengeTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/paid", gate.Require(goldenAtoms)(http.HandlerFunc(
		func(w http.ResponseWriter, req *http.Request) {
			r.handled.Add(1)
			fmt.Fprint(w, "CONTENT")
		})))
	mux.HandleFunc("/free", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, "FREE")
	})
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)

	client, err := New(Config{
		LN:      r.payer,
		Network: dcr402.Mainnet,
		Tokens:  astore.NewMemory(),
		Gate: func(ctx context.Context, a policy.Attempt) (int64, error) {
			if r.gateErr != nil {
				return 0, r.gateErr
			}
			return r.attempts.Add(1), nil
		},
		HTTP: r.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.client = client
	return r
}

func TestX402FlowAndCache(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	res, err := r.client.FetchPaid(ctx, r.srv.URL+"/paid", FetchOptions{Memo: "test"})
	if err != nil {
		t.Fatalf("FetchPaid: %v", err)
	}
	if res.Rail != RailX402 || res.Status != 200 || string(res.Body) != "CONTENT" ||
		res.PaidAtoms != goldenAtoms || res.Preimage != goldenPre || !res.TokenCached {
		t.Fatalf("result: %+v", res)
	}
	if res.AttemptID == 0 || len(res.Receipt) == 0 {
		t.Fatalf("missing attempt/receipt: %+v", res)
	}
	if r.payer.calls.Load() != 1 {
		t.Fatalf("payer calls: %d", r.payer.calls.Load())
	}

	// Second call rides the cached credential — no payment.
	res, err = r.client.FetchPaid(ctx, r.srv.URL+"/paid", FetchOptions{Memo: "again"})
	if err != nil {
		t.Fatalf("cached FetchPaid: %v", err)
	}
	if res.Rail != RailCached || res.PaidAtoms != 0 || string(res.Body) != "CONTENT" {
		t.Fatalf("cached result: %+v", res)
	}
	if r.payer.calls.Load() != 1 {
		t.Fatalf("cache did not prevent payment: %d calls", r.payer.calls.Load())
	}
	if r.handled.Load() != 2 {
		t.Fatalf("handler runs: %d", r.handled.Load())
	}
}

// TestExtensionEcho: the client copies the server's 402 extensions into its
// PaymentPayload on retry, so a facilitator can catalog the resource.
func TestExtensionEcho(t *testing.T) {
	var sent map[string]x402.Extension
	gate, err := dcr402.New(dcr402.Config{
		Backend: fakeGateLN{}, Store: libstore.NewMemory(),
		Network: dcr402.Mainnet, PayTo: goldenDest, Service: "echo-test",
		TokenTTL: time.Hour, ChallengeTTL: time.Hour,
		DiscoveryExtension: func(_ *http.Request, method string) *x402.Extension {
			ext := dcr402.BuildHTTPDiscovery(method, nil, nil)
			return &ext
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	capture := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if sig := req.Header.Get(x402.HeaderPaymentSignature); sig != "" {
				var pp x402.PaymentPayload
				if x402.DecodeHeader(sig, &pp) == nil {
					sent = pp.Extensions
				}
			}
			next.ServeHTTP(w, req)
		})
	}
	mux := http.NewServeMux()
	mux.Handle("/paid", capture(gate.Require(goldenAtoms)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "OK") }))))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, err := New(Config{
		LN: &fakePayer{}, Network: dcr402.Mainnet, Tokens: astore.NewMemory(),
		Gate: func(context.Context, policy.Attempt) (int64, error) { return 1, nil },
		HTTP: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.FetchPaid(context.Background(), srv.URL+"/paid", FetchOptions{Memo: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := sent[dcr402.ExtensionBazaar]; !ok {
		t.Fatalf("client did not echo the bazaar extension; got %v", sent)
	}
}

func TestLegacyFlow(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	res, err := r.client.FetchPaid(ctx, r.srv.URL+"/paid",
		FetchOptions{Memo: "legacy", ForceLegacy: true})
	if err != nil {
		t.Fatalf("FetchPaid legacy: %v", err)
	}
	if res.Rail != RailL402 || res.Status != 200 || string(res.Body) != "CONTENT" || !res.TokenCached {
		t.Fatalf("legacy result: %+v", res)
	}
	// The cached credential is the macaroon:preimage pair itself.
	tok, err := astoreToken(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "L402 ") || !strings.HasSuffix(tok, ":"+goldenPre) {
		t.Fatalf("legacy credential shape: %q", tok)
	}

	res, err = r.client.FetchPaid(ctx, r.srv.URL+"/paid", FetchOptions{Memo: "again"})
	if err != nil || res.Rail != RailCached {
		t.Fatalf("legacy cached: %+v err=%v", res, err)
	}
	if r.payer.calls.Load() != 1 {
		t.Fatalf("paid twice: %d", r.payer.calls.Load())
	}
}

// astoreToken digs the cached credential back out through a fresh fetch of
// the token store the rig used.
func astoreToken(r *rig) (string, error) {
	// The rig's token store is inside the client config; re-fetch via a
	// tiny probe: hit the paid endpoint again with a broken payer — if the
	// token weren't cached this would try to pay and fail.
	tok := r.client.cfg.Tokens
	got, err := tok.GetToken(context.Background(), "127.0.0.1")
	return got.Authorization, err
}

func TestFreePath(t *testing.T) {
	r := newRig(t)
	res, err := r.client.FetchPaid(context.Background(), r.srv.URL+"/free", FetchOptions{})
	if err != nil || res.Rail != RailFree || string(res.Body) != "FREE" || res.PaidAtoms != 0 {
		t.Fatalf("free: %+v err=%v", res, err)
	}
	if r.payer.calls.Load() != 0 {
		t.Fatal("free path paid")
	}
}

func TestPolicyDenyBlocksPayment(t *testing.T) {
	r := newRig(t)
	r.gateErr = fmt.Errorf("policy: max_per_payment exceeded")
	_, err := r.client.FetchPaid(context.Background(), r.srv.URL+"/paid", FetchOptions{Memo: "x"})
	if err == nil || !strings.Contains(err.Error(), "max_per_payment") {
		t.Fatalf("expected policy error, got %v", err)
	}
	if r.payer.calls.Load() != 0 {
		t.Fatal("payment happened despite policy deny")
	}
}

func TestAgentGuard(t *testing.T) {
	r := newRig(t)
	_, err := r.client.FetchPaid(context.Background(), r.srv.URL+"/paid",
		FetchOptions{Memo: "x", MaxAmountAtoms: 1000})
	if err == nil || !strings.Contains(err.Error(), "guard") {
		t.Fatalf("expected guard error, got %v", err)
	}
	if r.payer.calls.Load() != 0 {
		t.Fatal("guard did not prevent payment")
	}
}

func TestEstimate(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	est, err := r.client.Estimate(ctx, r.srv.URL+"/paid")
	if err != nil {
		t.Fatalf("estimate url: %v", err)
	}
	if est.Kind != "url" || est.Rail != RailX402 || est.AmountAtoms != goldenAtoms ||
		!strings.EqualFold(est.Destination, goldenDest) {
		t.Fatalf("estimate: %+v", est)
	}
	if r.payer.calls.Load() != 0 {
		t.Fatal("estimate paid")
	}

	est, err = r.client.Estimate(ctx, goldenInvoice)
	if err != nil || est.Kind != "invoice" || est.AmountAtoms != goldenAtoms ||
		est.PaymentHash != goldenHash {
		t.Fatalf("estimate invoice: %+v err=%v", est, err)
	}

	if _, err := r.client.Estimate(ctx, r.srv.URL+"/free"); err != nil {
		t.Fatalf("estimate free: %v", err)
	}
}
