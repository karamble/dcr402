package dcr402

import (
	"context"
	"encoding/hex"
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

// TestChallengeBoundStoresBind pins the exported bind/wire split for both
// builders: the 402 carries the wire resource, the store carries the bind.
func TestChallengeBoundStoresBind(t *testing.T) {
	st := store.NewMemory()
	g, err := New(Config{
		Backend: &fakeLN{}, Store: st, Network: Mainnet, PayTo: goldenDest,
		Service: "export", TokenTTL: time.Hour, ChallengeTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	var hash [32]byte
	raw, _ := hex.DecodeString(goldenHash)
	copy(hash[:], raw)

	wire := x402.ResourceInfo{URL: "https://api.example.com/mcp"}
	pr, err := g.ChallengeBound(context.Background(), wire, "mcp://tool/register?usd_micros=1000", goldenAtoms, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Resource.URL != wire.URL {
		t.Fatalf("wire url = %q", pr.Resource.URL)
	}
	ch, err := st.GetChallenge(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Resource != "mcp://tool/register?usd_micros=1000" {
		t.Fatalf("stored bind = %q", ch.Resource)
	}

	// Topup counterpart (lightning-only backend).
	st2 := store.NewMemory()
	g2, err := New(Config{
		Backend: &fakeLN{}, Store: st2, Network: Mainnet, PayTo: goldenDest,
		Service: "export", TokenTTL: time.Hour, ChallengeTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	pr2, err := g2.TopupChallengeBound(context.Background(), wire, "mcp://tool/topup?usd_micros=5000000", goldenAtoms, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pr2.Resource.URL != wire.URL {
		t.Fatalf("topup wire url = %q", pr2.Resource.URL)
	}
	ch2, err := st2.GetChallenge(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	if ch2.Resource != "mcp://tool/topup?usd_micros=5000000" {
		t.Fatalf("topup stored bind = %q", ch2.Resource)
	}
}
