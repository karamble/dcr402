package dcr402

import (
	"context"
	"testing"
	"time"

	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

// TestChallengeExport mints a challenge for an arbitrary resource through
// the exported builder and settles its paid proof.
func TestChallengeExport(t *testing.T) {
	ln := &fakeLN{}
	g, err := New(Config{
		Backend: ln, Store: store.NewMemory(), Network: Mainnet, PayTo: goldenDest,
		Service: "export", TokenTTL: time.Hour, ChallengeTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := x402.ResourceInfo{URL: "https://api.example.com/topup?usd_micros=3000", Description: "credit topup"}
	pr, err := g.Challenge(context.Background(), res, goldenAtoms, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pr.X402Version != x402.Version || len(pr.Accepts) != 1 {
		t.Fatalf("challenge shape wrong: %+v", pr)
	}
	entry := pr.Accepts[0]
	if entry.TransferMethod() != x402.MethodLightning || entry.Network != Mainnet.CAIP2 ||
		entry.PayTo != goldenDest || entry.Amount != "250000" {
		t.Fatalf("lightning entry wrong: %+v", entry)
	}
	if pr.Resource.URL != res.URL {
		t.Fatalf("resource not carried: %+v", pr.Resource)
	}
	if _, ok := pr.Extensions[ExtensionL402]; !ok {
		t.Fatal("l402 extension missing")
	}

	// The minted challenge is of record: its paid proof settles.
	ln.Pay()
	pp := x402.PaymentPayload{
		X402Version: 2,
		Accepted:    entry,
		Resource:    &pr.Resource,
		Payload:     x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
	}
	settle, already, vErr := g.Settle(context.Background(), pp, goldenAtoms)
	if vErr != nil || already || !settle.Success {
		t.Fatalf("settle: %v already=%v %+v", vErr, already, settle)
	}
}

// TestChallengeExportRejectsNonPositive covers the amount guard.
func TestChallengeExportRejectsNonPositive(t *testing.T) {
	g, err := New(Config{
		Backend: &fakeLN{}, Store: store.NewMemory(), Network: Mainnet, PayTo: goldenDest,
		Service: "export", TokenTTL: time.Hour, ChallengeTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Challenge(context.Background(), x402.ResourceInfo{URL: "x"}, 0, nil); err == nil {
		t.Fatal("zero amount accepted")
	}
}
