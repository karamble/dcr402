package dcr402

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

// callerFunc adapts a plain function to the MCPCaller interface.
type callerFunc func(ctx context.Context, tool string, args any, meta map[string]json.RawMessage) (MCPToolResult, error)

func (f callerFunc) CallTool(ctx context.Context, tool string, args any, meta map[string]json.RawMessage) (MCPToolResult, error) {
	return f(ctx, tool, args, meta)
}

// TestMCPMetaRoundTrip checks the buyer encode/decode helpers are the exact
// inverse of the server ones: a payment survives EncodeMetaPayment ->
// DecodeMetaPayment, and a receipt survives the _meta response channel.
func TestMCPMetaRoundTrip(t *testing.T) {
	pp := x402.PaymentPayload{
		X402Version: 2,
		Accepted: x402.PaymentRequirements{
			Scheme: "exact", Network: Mainnet.CAIP2, Amount: "250000", Asset: "DCR", PayTo: goldenDest,
		},
		Payload: x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
	}
	meta, err := EncodeMetaPayment(pp)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := meta[MetaPayment]; !ok {
		t.Fatalf("encoded meta lacks %q: %v", MetaPayment, meta)
	}
	got, ok, err := DecodeMetaPayment(meta)
	if err != nil || !ok {
		t.Fatalf("DecodeMetaPayment: ok=%v err=%v", ok, err)
	}
	if got.X402Version != 2 || got.Accepted.Amount != "250000" || got.Accepted.PayTo != goldenDest {
		t.Fatalf("payload round-trip: %+v", got)
	}

	sr := x402.SettlementResponse{Success: true, Transaction: goldenHash, Network: Mainnet.CAIP2, Amount: "250000"}
	rmeta := map[string]json.RawMessage{MetaPaymentResponse: x402.MustRaw(sr)}
	back, ok, err := DecodeMetaPaymentResponse(rmeta)
	if err != nil || !ok {
		t.Fatalf("DecodeMetaPaymentResponse: ok=%v err=%v", ok, err)
	}
	if !back.Success || back.Transaction != goldenHash || back.Amount != "250000" {
		t.Fatalf("receipt round-trip: %+v", back)
	}
	if _, ok, _ := DecodeMetaPaymentResponse(nil); ok {
		t.Fatal("absent receipt must report ok=false")
	}
}

// TestParsePaymentRequiredResult round-trips a real Gate challenge through the
// buyer parser and rejects a non-challenge error result.
func TestParsePaymentRequiredResult(t *testing.T) {
	g := newTestGate(t, &fakeLN{}, store.NewMemory(), nil)
	res, err := g.MCPChallenge(context.Background(), "get_preview", "preview", goldenAtoms)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a payment-required tool result must be an error result")
	}
	pr, err := ParsePaymentRequiredResult(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if pr.X402Version != 2 || len(pr.Accepts) == 0 {
		t.Fatalf("parsed challenge: %+v", pr)
	}
	if pr.Resource.URL != MCPToolURL("get_preview") {
		t.Fatalf("resource url = %q", pr.Resource.URL)
	}
	// A genuine tool error (no accepts) must not parse as a challenge.
	bad := x402.MustRaw(map[string]any{"error": "bad_request", "message": "lat required"})
	if _, err := ParsePaymentRequiredResult(bad); err == nil {
		t.Fatal("non-challenge error result must be rejected")
	}
}

// TestEncodeDecodeReceiptRoundTrip pairs the server receipt encoder with the
// buyer decoder over the same _meta key.
func TestEncodeDecodeReceiptRoundTrip(t *testing.T) {
	sr := x402.SettlementResponse{Success: true, Transaction: goldenHash, Network: Mainnet.CAIP2, Amount: "250000"}
	meta, err := EncodeMetaPaymentResponse(sr)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := meta[MetaPaymentResponse]; !ok {
		t.Fatalf("encoded meta lacks %q: %v", MetaPaymentResponse, meta)
	}
	back, ok, err := DecodeMetaPaymentResponse(meta)
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}
	if !back.Success || back.Transaction != goldenHash || back.Amount != "250000" {
		t.Fatalf("receipt round-trip: %+v", back)
	}
}

// TestServeMCPPayment exercises the reusable server bracket: an unpaid call
// yields a spec-shaped challenge; a paid call verifies+settles and hands back
// the settlement plus the _meta receipt for the caller to attach.
func TestServeMCPPayment(t *testing.T) {
	ln := &fakeLN{}
	g := newTestGate(t, ln, store.NewMemory(), nil)
	ctx := context.Background()

	// Unpaid: no _meta payment → challenge.
	out, vErr, err := g.ServeMCPPayment(ctx, "get_preview", "preview", goldenAtoms, nil)
	if err != nil || vErr != nil {
		t.Fatalf("unpaid: vErr=%v err=%v", vErr, err)
	}
	if out.Paid || out.Challenge == nil || !out.Challenge.IsError {
		t.Fatalf("unpaid outcome: %+v", out)
	}
	pr, err := ParsePaymentRequiredResult(out.Challenge.StructuredContent)
	if err != nil {
		t.Fatalf("challenge not spec-shaped: %v", err)
	}
	// Spec: PaymentRequired must ride BOTH structuredContent and content[0].text.
	if len(out.Challenge.Content) == 0 || out.Challenge.Content[0].Text == "" {
		t.Fatal("challenge missing content[0].text mirror")
	}
	if pr.Resource.URL != MCPToolURL("get_preview") {
		t.Fatalf("resource url = %q", pr.Resource.URL)
	}

	// Paid: build the proof from the challenge and settle through the bracket.
	ln.Pay()
	pp := x402.PaymentPayload{
		X402Version: 2,
		Resource:    &pr.Resource,
		Accepted:    pr.Accepts[0],
		Payload:     x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
	}
	meta, err := EncodeMetaPayment(pp)
	if err != nil {
		t.Fatal(err)
	}
	out, vErr, err = g.ServeMCPPayment(ctx, "get_preview", "preview", goldenAtoms, meta)
	if err != nil || vErr != nil {
		t.Fatalf("paid: vErr=%v err=%v", vErr, err)
	}
	if !out.Paid || out.Challenge != nil {
		t.Fatalf("paid outcome: %+v", out)
	}
	if !out.Settlement.Success || out.Settlement.Transaction != goldenHash {
		t.Fatalf("settlement: %+v", out.Settlement)
	}
	back, ok, err := DecodeMetaPaymentResponse(out.ReceiptMeta)
	if err != nil || !ok || !back.Success {
		t.Fatalf("receipt meta: %+v ok=%v err=%v", back, ok, err)
	}
}

// gateCaller adapts a real Gate to MCPCaller: the unpaid probe returns the
// tool's challenge; the paid retry verifies+settles the _meta payment and
// returns the tool output plus the receipt in result._meta.
type gateCaller struct {
	g      *Gate
	atoms  int64
	output json.RawMessage
	calls  int
}

func (c *gateCaller) CallTool(ctx context.Context, tool string, _ any, meta map[string]json.RawMessage) (MCPToolResult, error) {
	c.calls++
	pp, ok, err := DecodeMetaPayment(meta)
	if err != nil {
		return MCPToolResult{}, err
	}
	if !ok {
		res, err := c.g.MCPChallenge(ctx, tool, "test tool", c.atoms)
		if err != nil {
			return MCPToolResult{}, err
		}
		return MCPToolResult{StructuredContent: res.StructuredContent, IsError: true}, nil
	}
	sr, _, verr := c.g.MCPSettle(ctx, pp, c.atoms)
	if verr != nil {
		body := x402.MustRaw(map[string]any{"error": "payment_invalid", "message": verr.Error()})
		return MCPToolResult{StructuredContent: body, IsError: true}, nil
	}
	return MCPToolResult{
		StructuredContent: c.output,
		Meta:              map[string]json.RawMessage{MetaPaymentResponse: x402.MustRaw(sr)},
	}, nil
}

// TestFetchPaidMCP drives the full compliant pay-and-retry loop against the
// real Gate settle path and golden vectors: probe -> challenge -> pay ->
// retry with _meta -> tool output + receipt.
func TestFetchPaidMCP(t *testing.T) {
	ln := &fakeLN{}
	g := newTestGate(t, ln, store.NewMemory(), nil)
	caller := &gateCaller{g: g, atoms: goldenAtoms, output: x402.MustRaw(map[string]any{"result_url": "https://x/y.jpg"})}

	paid := 0
	pay := func(_ context.Context, pr x402.PaymentRequired) (x402.PaymentPayload, error) {
		paid++
		ln.Pay() // the buyer's node settles the invoice
		return x402.PaymentPayload{
			X402Version: 2,
			Resource:    &pr.Resource,
			Accepted:    pr.Accepts[0],
			Payload:     x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
		}, nil
	}

	out, err := FetchPaidMCP(context.Background(), caller, "get_preview", map[string]any{"lat": 1.0}, pay)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Paid {
		t.Fatal("expected Paid=true")
	}
	if paid != 1 {
		t.Fatalf("pay called %d times, want 1", paid)
	}
	if caller.calls != 2 {
		t.Fatalf("caller invoked %d times, want 2 (probe + paid retry)", caller.calls)
	}
	if out.Receipt == nil || !out.Receipt.Success || out.Receipt.Transaction != goldenHash {
		t.Fatalf("receipt: %+v", out.Receipt)
	}
	if out.Receipt.Transaction == goldenPre {
		t.Fatal("receipt leaked the preimage")
	}
	var body map[string]any
	if err := json.Unmarshal(out.StructuredContent, &body); err != nil {
		t.Fatal(err)
	}
	if body["result_url"] != "https://x/y.jpg" {
		t.Fatalf("tool output: %v", body)
	}
}

// TestFetchPaidMCPNoPaymentWhenSucceeds confirms a first-call success (credit
// mode / free tool) returns without invoking the payer.
func TestFetchPaidMCPNoPaymentWhenSucceeds(t *testing.T) {
	caller := callerFunc(func(context.Context, string, any, map[string]json.RawMessage) (MCPToolResult, error) {
		return MCPToolResult{StructuredContent: x402.MustRaw(map[string]any{"ok": true})}, nil
	})
	called := false
	pay := func(context.Context, x402.PaymentRequired) (x402.PaymentPayload, error) {
		called = true
		return x402.PaymentPayload{}, nil
	}
	out, err := FetchPaidMCP(context.Background(), caller, "about", nil, pay)
	if err != nil {
		t.Fatal(err)
	}
	if out.Paid || called {
		t.Fatalf("no payment expected: Paid=%v payerCalled=%v", out.Paid, called)
	}
}

// TestFetchPaidMCPRealErrorSurfaces confirms a genuine tool error (not a
// payment challenge) is surfaced without attempting payment.
func TestFetchPaidMCPRealErrorSurfaces(t *testing.T) {
	caller := callerFunc(func(context.Context, string, any, map[string]json.RawMessage) (MCPToolResult, error) {
		body := x402.MustRaw(map[string]any{"error": "bad_request", "message": "lat required"})
		return MCPToolResult{StructuredContent: body, IsError: true}, nil
	})
	called := false
	pay := func(context.Context, x402.PaymentRequired) (x402.PaymentPayload, error) {
		called = true
		return x402.PaymentPayload{}, nil
	}
	if _, err := FetchPaidMCP(context.Background(), caller, "get_preview", nil, pay); err == nil {
		t.Fatal("expected the tool error to surface")
	}
	if called {
		t.Fatal("payer must not run for a non-payment tool error")
	}
}
