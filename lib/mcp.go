package dcr402

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/karamble/dcr402/lib/x402"
)

// MCP transport constants (specs/transports-v2/mcp.md).
const (
	// MetaPayment is the tools/call params._meta key carrying the
	// client's PaymentPayload.
	MetaPayment = "x402/payment"
	// MetaPaymentResponse is the result._meta key carrying the server's
	// SettlementResponse.
	MetaPaymentResponse = "x402/payment-response"
)

// MCPToolURL renders the host-less fallback resource URL convention for a
// payable tool (specs/transports-v2/mcp.md). It identifies the tool on the
// wire but not the serving endpoint; sellers that want facilitators to
// catalog a connectable endpoint advertise their public MCP URL instead —
// see MCPResourceURL and Config.MCPServerURL. It remains the settlement
// binding key either way.
func MCPToolURL(tool string) string { return "mcp://tool/" + tool }

// MCPResourceURL renders the wire resource URL for a payable tool: the
// server's public streamable-HTTP endpoint when known (the bazaar catalog
// form — the tool itself rides the discovery extension's info.input
// toolName), else the host-less MCPToolURL fallback.
func MCPResourceURL(serverURL, tool string) string {
	if serverURL == "" {
		return MCPToolURL(tool)
	}
	return serverURL
}

// ToolContent is one MCP tool-result content item.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolResult is the payment-required MCP tool result: isError with the
// PaymentRequired object in BOTH structuredContent and content[0].text, as
// the transport requires. It marshals to the exact wire shape; embed it in
// whatever MCP framework the server uses.
type ToolResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Content           []ToolContent   `json:"content"`
}

// PaymentRequiredToolResult wraps a PaymentRequired as the MCP tool result.
func PaymentRequiredToolResult(pr x402.PaymentRequired) (*ToolResult, error) {
	raw, err := json.Marshal(pr)
	if err != nil {
		return nil, err
	}
	return &ToolResult{
		IsError:           true,
		StructuredContent: raw,
		Content:           []ToolContent{{Type: "text", Text: string(raw)}},
	}, nil
}

// MCPChallenge builds the payment-required tool result for a payable tool:
// a fresh challenge priced at amountAtoms. The resource URL is the gate's
// Config.MCPServerURL when set, else the "mcp://tool/<tool>" fallback;
// settlement always binds to the per-tool key.
func (g *Gate) MCPChallenge(ctx context.Context, tool, description string, amountAtoms int64) (*ToolResult, error) {
	return g.MCPChallengeAt(ctx, g.cfg.MCPServerURL, tool, description, amountAtoms)
}

// MCPChallengeAt is MCPChallenge with an explicit server endpoint URL for
// the wire resource — for callers whose public URL varies per request (a
// gateway fronting many upstreams). Empty serverURL falls back to the
// host-less mcp://tool/<tool> convention. The challenge is stored under
// the per-tool binding key MCPToolURL(tool) regardless, so a proof minted
// for one tool never settles another even when every tool of the server
// advertises the same resource URL.
func (g *Gate) MCPChallengeAt(ctx context.Context, serverURL, tool, description string, amountAtoms int64) (*ToolResult, error) {
	var extra map[string]x402.Extension
	if g.cfg.MCPDiscovery != nil {
		if ext := g.cfg.MCPDiscovery(tool); ext != nil {
			extra = map[string]x402.Extension{ExtensionBazaar: *ext}
		}
	}
	art, err := g.buildChallenge(ctx, x402.ResourceInfo{
		URL:         MCPResourceURL(serverURL, tool),
		Description: description,
		MimeType:    "application/json",
	}, MCPToolURL(tool), amountAtoms, extra)
	if err != nil {
		return nil, err
	}
	return PaymentRequiredToolResult(art.PaymentRequired)
}

// DecodeMetaPayment extracts the PaymentPayload from a tools/call
// params._meta map (the value under MetaPayment). ok is false when the key
// is absent.
func DecodeMetaPayment(meta map[string]json.RawMessage) (x402.PaymentPayload, bool, error) {
	var pp x402.PaymentPayload
	raw, present := meta[MetaPayment]
	if !present {
		return pp, false, nil
	}
	if err := json.Unmarshal(raw, &pp); err != nil {
		return pp, true, fmt.Errorf("dcr402: %s: %w", MetaPayment, err)
	}
	return pp, true, nil
}

// MCPSettle verifies and settles an MCP payment, returning the value for
// result._meta[MetaPaymentResponse]. Semantics match the HTTP path: already
// reports idempotent re-presentation (do not re-execute the tool), and a
// non-nil *VerifyError comes with a failure SettlementResponse to return.
func (g *Gate) MCPSettle(ctx context.Context, pp x402.PaymentPayload, requiredAtoms int64) (x402.SettlementResponse, bool, *VerifyError) {
	return g.Settle(ctx, pp, requiredAtoms)
}

// EncodeMetaPaymentResponse renders the result._meta map carrying sr under
// MetaPaymentResponse — the server-side inverse of DecodeMetaPaymentResponse.
// Attach the returned map to the successful tool result's _meta.
func EncodeMetaPaymentResponse(sr x402.SettlementResponse) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(sr)
	if err != nil {
		return nil, fmt.Errorf("dcr402: %s: %w", MetaPaymentResponse, err)
	}
	return map[string]json.RawMessage{MetaPaymentResponse: raw}, nil
}

// MCPPaymentOutcome is the transport result of ServeMCPPayment.
type MCPPaymentOutcome struct {
	// Challenge is non-nil when no payment was presented: return it as the
	// tool result (it is an isError payment-required result).
	Challenge *ToolResult
	// Paid reports whether a payment was presented and verified.
	Paid bool
	// Settlement is the verified settlement (valid when Paid).
	Settlement x402.SettlementResponse
	// Already is true on idempotent re-presentation of a consumed proof: the
	// caller must NOT re-run its side effect, only re-emit the result.
	Already bool
	// ReceiptMeta is the result._meta map to attach on success (carries
	// MetaPaymentResponse).
	ReceiptMeta map[string]json.RawMessage
}

// ServeMCPPayment runs the server half of the x402 MCP transport for one
// payable tool call, so a seller never hand-rolls the wire: with no payment in
// meta it returns a Challenge (bare PaymentRequired, in both structuredContent
// and content[0].text) to return as the tool result; with a payment it
// verifies and settles at requiredAtoms and returns the Settlement plus the
// ReceiptMeta to attach under result._meta[MetaPaymentResponse]. The caller
// runs its own business action (execute the tool, register, credit a top-up)
// using Settlement and skips it when Already is true. A *VerifyError is
// returned when a presented payment does not verify.
func (g *Gate) ServeMCPPayment(ctx context.Context, tool, description string, requiredAtoms int64,
	meta map[string]json.RawMessage) (*MCPPaymentOutcome, *VerifyError, error) {
	return g.ServeMCPPaymentAt(ctx, g.cfg.MCPServerURL, tool, description, requiredAtoms, meta)
}

// ServeMCPPaymentAt is ServeMCPPayment with an explicit server endpoint URL
// for challenge resources — the MCPChallengeAt counterpart. Empty serverURL
// falls back to the mcp://tool/<tool> convention.
func (g *Gate) ServeMCPPaymentAt(ctx context.Context, serverURL, tool, description string, requiredAtoms int64,
	meta map[string]json.RawMessage) (*MCPPaymentOutcome, *VerifyError, error) {
	pp, ok, err := DecodeMetaPayment(meta)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		ch, err := g.MCPChallengeAt(ctx, serverURL, tool, description, requiredAtoms)
		if err != nil {
			return nil, nil, err
		}
		return &MCPPaymentOutcome{Challenge: ch}, nil, nil
	}
	settlement, already, vErr := g.MCPSettle(ctx, pp, requiredAtoms)
	if vErr != nil {
		return nil, vErr, nil
	}
	receipt, err := EncodeMetaPaymentResponse(settlement)
	if err != nil {
		return nil, nil, err
	}
	return &MCPPaymentOutcome{Paid: true, Settlement: settlement, Already: already, ReceiptMeta: receipt}, nil, nil
}

// --- Client (buyer) side -------------------------------------------------
//
// The helpers above emit and settle payments as a payable MCP server; the
// ones below are their inverse — what a paying MCP client uses. The payment
// travels only in params._meta[MetaPayment] and the receipt in
// result._meta[MetaPaymentResponse], never in tool arguments, so a buyer
// built on these stays the spec-compliant x402 MCP client half.

// EncodeMetaPayment renders the tools/call params._meta map carrying pp under
// MetaPayment — the inverse of DecodeMetaPayment.
func EncodeMetaPayment(pp x402.PaymentPayload) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(pp)
	if err != nil {
		return nil, fmt.Errorf("dcr402: %s: %w", MetaPayment, err)
	}
	return map[string]json.RawMessage{MetaPayment: raw}, nil
}

// ParsePaymentRequiredResult extracts the x402 challenge from a payment-
// required tool result's structuredContent — the inverse of
// PaymentRequiredToolResult. It errors when structured is not a v2
// PaymentRequired with at least one accepts entry, i.e. a genuine tool error
// rather than a payment challenge.
func ParsePaymentRequiredResult(structured json.RawMessage) (x402.PaymentRequired, error) {
	var pr x402.PaymentRequired
	if len(structured) == 0 {
		return pr, fmt.Errorf("dcr402: empty tool result structuredContent")
	}
	if err := json.Unmarshal(structured, &pr); err != nil {
		return pr, fmt.Errorf("dcr402: tool result structuredContent: %w", err)
	}
	if pr.X402Version == 0 || len(pr.Accepts) == 0 {
		return pr, fmt.Errorf("dcr402: tool result is not a payment-required challenge")
	}
	return pr, nil
}

// DecodeMetaPaymentResponse reads the settlement receipt from a tool result's
// _meta map (the value under MetaPaymentResponse) — the inverse of what
// MCPSettle produces. ok is false when the key is absent.
func DecodeMetaPaymentResponse(meta map[string]json.RawMessage) (x402.SettlementResponse, bool, error) {
	var sr x402.SettlementResponse
	raw, present := meta[MetaPaymentResponse]
	if !present {
		return sr, false, nil
	}
	if err := json.Unmarshal(raw, &sr); err != nil {
		return sr, true, fmt.Errorf("dcr402: %s: %w", MetaPaymentResponse, err)
	}
	return sr, true, nil
}

// MCPToolResult is the transport-neutral view of a tools/call result the
// paid-fetch driver needs: the structured content, the result-level _meta
// map, and whether the call is an error result. Adapt your MCP SDK's result
// type into this (marshal StructuredContent and each _meta value to JSON).
type MCPToolResult struct {
	StructuredContent json.RawMessage
	Meta              map[string]json.RawMessage
	IsError           bool
}

// MCPCaller performs a single MCP tools/call. args is the tool's arguments;
// meta is the params._meta map — nil on the unpaid probe, carrying
// MetaPayment on the paid retry. Adapt your MCP SDK's client to this one
// method; the driver keeps this package free of any concrete SDK dependency.
type MCPCaller interface {
	CallTool(ctx context.Context, tool string, args any, meta map[string]json.RawMessage) (MCPToolResult, error)
}

// MCPPayer settles a payment-required challenge and returns the PaymentPayload
// to retry with. Wire your wallet / x402 pay path in here.
type MCPPayer func(ctx context.Context, pr x402.PaymentRequired) (x402.PaymentPayload, error)

// MCPPaidResult is the outcome of FetchPaidMCP.
type MCPPaidResult struct {
	// StructuredContent is the tool's successful structured result.
	StructuredContent json.RawMessage
	// Receipt is result._meta[MetaPaymentResponse], set when a payment was
	// made and the server returned a receipt.
	Receipt *x402.SettlementResponse
	// Paid reports whether a payment was required and made. False when the
	// first call already succeeded (credit mode, a free tool, or a cached
	// credential).
	Paid bool
}

// FetchPaidMCP calls a payable MCP tool and completes the x402 pay-and-retry
// over the MCP transport: it probes once, and if the result is a payment-
// required challenge it invokes pay and retries the same call once with the
// PaymentPayload in params._meta[MetaPayment], then returns the tool result
// with the receipt from result._meta[MetaPaymentResponse]. The payment
// travels only in transport _meta, never in tool arguments.
func FetchPaidMCP(ctx context.Context, caller MCPCaller, tool string, args any, pay MCPPayer) (*MCPPaidResult, error) {
	first, err := caller.CallTool(ctx, tool, args, nil)
	if err != nil {
		return nil, err
	}
	if !first.IsError {
		// Already served: credit mode, a free tool, or a cached credential.
		return &MCPPaidResult{StructuredContent: first.StructuredContent}, nil
	}
	pr, err := ParsePaymentRequiredResult(first.StructuredContent)
	if err != nil {
		// An error result that is not a payment challenge is a real tool
		// failure — surface it rather than trying to pay.
		return nil, fmt.Errorf("dcr402: tool %q errored: %s", tool, toolErrText(first))
	}
	pp, err := pay(ctx, pr)
	if err != nil {
		return nil, fmt.Errorf("dcr402: paying %q challenge: %w", tool, err)
	}
	meta, err := EncodeMetaPayment(pp)
	if err != nil {
		return nil, err
	}
	second, err := caller.CallTool(ctx, tool, args, meta)
	if err != nil {
		return nil, err
	}
	if second.IsError {
		return nil, fmt.Errorf("dcr402: tool %q rejected the payment: %s", tool, toolErrText(second))
	}
	receipt, ok, err := DecodeMetaPaymentResponse(second.Meta)
	if err != nil {
		return nil, err
	}
	res := &MCPPaidResult{StructuredContent: second.StructuredContent, Paid: true}
	if ok {
		res.Receipt = &receipt
	}
	return res, nil
}

// toolErrText renders a concise message from an error tool result's
// structured content ({error, message}), falling back to the raw JSON.
func toolErrText(r MCPToolResult) string {
	if len(r.StructuredContent) == 0 {
		return "unknown error"
	}
	var probe struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(r.StructuredContent, &probe)
	switch {
	case probe.Error != "" && probe.Message != "":
		return probe.Error + ": " + probe.Message
	case probe.Message != "":
		return probe.Message
	case probe.Error != "":
		return probe.Error
	default:
		return string(r.StructuredContent)
	}
}
