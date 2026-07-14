// Package mcpserver exposes the agent's payment surface as MCP tools over
// the official modelcontextprotocol/go-sdk, on stdio and/or streamable
// HTTP. It is a thin adapter: every tool calls the agent Service, which is
// where policy, payment, and the audit trail live. There is deliberately
// no key, macaroon, policy-mutation, or channel-management tool — those are
// not present, not merely restricted.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/karamble/dcr402/agent"
	"github.com/karamble/dcr402/agent/approve"
	"github.com/karamble/dcr402/agent/payclient"
)

// Server wraps an mcp.Server bound to the agent Service.
type Server struct {
	svc     *agent.Service
	network string
	mcp     *mcp.Server
}

// New builds the MCP server and registers the tool surface.
func New(svc *agent.Service, network string) *Server {
	s := &Server{
		svc:     svc,
		network: network,
		mcp: mcp.NewServer(&mcp.Implementation{
			Name:    "dcr402-agent",
			Version: "0.1.0",
		}, nil),
	}
	s.register()
	return s
}

// MCP returns the underlying server (for transport wiring).
func (s *Server) MCP() *mcp.Server { return s.mcp }

// readHint / writeHint annotate tools so clients can distinguish inspection
// from fund movement.
var (
	readHint  = &mcp.ToolAnnotations{ReadOnlyHint: true}
	writeHint = &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(true), OpenWorldHint: ptr(true)}
)

func ptr[T any](v T) *T { return &v }

// result wraps a value as a tool result. Payment/policy conditions are
// surfaced as machine-actionable tool errors (isError) carrying structured
// content the agent can act on — never a dead end.
func result(v any, err error) (*mcp.CallToolResult, any, error) {
	if err == nil {
		return nil, v, nil
	}
	structured := errorPayload(err)
	raw, _ := json.Marshal(structured)
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}},
	}, structured, nil
}

// errorPayload renders an error as the structured, machine-actionable body
// the agent should read.
func errorPayload(err error) map[string]any {
	var pending *approve.PendingError
	if errors.As(err, &pending) {
		return map[string]any{
			"status":     "pending_approval",
			"id":         pending.ID,
			"expires_at": pending.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			"detail":     "an owner must approve this payment; poll approval_status with the id",
		}
	}
	var pol *agent.PolicyError
	if errors.As(err, &pol) {
		return map[string]any{
			"error":                "policy_denied",
			"reason":               pol.Decision.Reason,
			"rule_trace":           pol.Decision.Trace,
			"remaining_day_atoms":  pol.Decision.RemainingDayAtoms,
			"remaining_week_atoms": pol.Decision.RemainingWeekAtoms,
			"detail":               "blocked by policy before any signing; adjust the request or ask the owner",
		}
	}
	var frozen *agent.FrozenError
	if errors.As(err, &frozen) {
		return map[string]any{
			"error":  "frozen",
			"reason": frozen.State.Reason,
			"detail": "all spending is frozen by the owner; it resumes only when the owner unfreezes (the agent cannot)",
		}
	}
	return map[string]any{"error": "failed", "detail": err.Error()}
}

// addTool is the read/write registration idiom (mirrors dcrpulse@mcp).
func addTool[In any](s *Server, name, description string, readOnly bool, fn func(context.Context, In) (any, error)) {
	ann := writeHint
	if readOnly {
		ann = readHint
	}
	mcp.AddTool(s.mcp, &mcp.Tool{Name: name, Description: description, Annotations: ann},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			return result(fn(ctx, in))
		})
}

// --- tool input types ------------------------------------------------------

type emptyInput struct{}

type estimateInput struct {
	Invoice string `json:"invoice,omitempty" jsonschema:"a BOLT11 invoice to decode"`
	URL     string `json:"url,omitempty" jsonschema:"a URL to probe for its 402 challenge (never pays)"`
}

type payLNInput struct {
	Invoice        string `json:"invoice" jsonschema:"the BOLT11 invoice to pay"`
	Memo           string `json:"memo" jsonschema:"reason for the payment (may be required by policy)"`
	MaxAmountAtoms int64  `json:"max_amount_atoms,omitempty" jsonschema:"agent-side guard: refuse if the invoice exceeds this"`
}

type payDCRInput struct {
	Address     string `json:"address" jsonschema:"the destination DCR address"`
	AmountAtoms int64  `json:"amount_atoms" jsonschema:"amount in atoms (1 DCR = 1e8 atoms)"`
	Memo        string `json:"memo" jsonschema:"reason for the payment"`
}

type fetchPaidInput struct {
	URL    string `json:"url" jsonschema:"the resource URL"`
	Method string `json:"method,omitempty" jsonschema:"HTTP method (default GET)"`
	Body   string `json:"body,omitempty" jsonschema:"request body"`
	Memo   string `json:"memo" jsonschema:"reason for any payment this requires"`
}

type createInvoiceInput struct {
	AmountAtoms int64  `json:"amount_atoms" jsonschema:"amount to request, in atoms"`
	Memo        string `json:"memo,omitempty" jsonschema:"invoice memo"`
}

type listInput struct {
	Limit    int   `json:"limit,omitempty" jsonschema:"max rows (default 50)"`
	BeforeID int64 `json:"before_id,omitempty" jsonschema:"paginate: return rows with id below this"`
}

type receiptInput struct {
	ID int64 `json:"id" jsonschema:"the payment id"`
}

type approvalInput struct {
	ID string `json:"id" jsonschema:"the approval id"`
}

// register wires the whole surface (dcrpay spec §5).
func (s *Server) register() {
	addTool(s, "wallet_status",
		"Balances (Lightning + on-chain), budget remaining, policy summary, and pending approvals.",
		true, func(ctx context.Context, _ emptyInput) (any, error) {
			return s.svc.Status(ctx, s.network)
		})

	addTool(s, "estimate",
		"Decode a BOLT11 invoice or probe a URL's 402 challenge WITHOUT paying — budget before committing.",
		true, func(ctx context.Context, in estimateInput) (any, error) {
			target := in.Invoice
			if target == "" {
				target = in.URL
			}
			if target == "" {
				return nil, fmt.Errorf("estimate: provide invoice or url")
			}
			return s.svc.Estimate(ctx, target)
		})

	addTool(s, "pay_ln_invoice",
		"Pay a BOLT11 Lightning invoice, subject to policy. Returns a receipt or a machine-actionable denial/approval status.",
		false, func(ctx context.Context, in payLNInput) (any, error) {
			return s.svc.PayLNInvoice(ctx, in.Invoice, in.Memo, in.MaxAmountAtoms)
		})

	addTool(s, "pay_dcr_address",
		"Send DCR on-chain from the agent account to an address, subject to policy.",
		false, func(ctx context.Context, in payDCRInput) (any, error) {
			return s.svc.PayDCRAddress(ctx, in.Address, in.AmountAtoms, in.Memo)
		})

	addTool(s, "fetch_paid",
		"HTTP request to a URL; on a 402 payment challenge (x402 or L402), pay under policy and return the response plus a receipt. Cached credentials make repeat access free.",
		false, func(ctx context.Context, in fetchPaidInput) (any, error) {
			opt := payclient.FetchOptions{Method: in.Method, Memo: in.Memo}
			if in.Body != "" {
				opt.Body = []byte(in.Body)
				opt.ContentType = "application/json"
			}
			res, err := s.svc.FetchPaid(ctx, in.URL, opt)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"status":       res.Status,
				"rail":         res.Rail,
				"paid_atoms":   res.PaidAtoms,
				"preimage":     res.Preimage,
				"body":         string(res.Body),
				"token_cached": res.TokenCached,
			}, nil
		})

	addTool(s, "get_receive_address",
		"A fresh DCR address for funding the agent's on-chain account.",
		true, func(ctx context.Context, _ emptyInput) (any, error) {
			addr, err := s.svc.GetReceiveAddress(ctx)
			return map[string]string{"address": addr}, err
		})

	addTool(s, "create_invoice",
		"Create a Lightning invoice to receive DCR.",
		true, func(ctx context.Context, in createInvoiceInput) (any, error) {
			invoice, hash, err := s.svc.CreateInvoice(ctx, in.AmountAtoms, in.Memo)
			return map[string]string{"invoice": invoice, "payment_hash": hash}, err
		})

	addTool(s, "payments_list",
		"Recent payments with receipts (invoice/preimage or txid) and policy traces.",
		true, func(ctx context.Context, in listInput) (any, error) {
			return s.svc.Payments(ctx, in.Limit, in.BeforeID)
		})

	addTool(s, "receipt_get",
		"One payment's full receipt by id.",
		true, func(ctx context.Context, in receiptInput) (any, error) {
			return s.svc.Receipt(ctx, in.ID)
		})

	addTool(s, "approval_status",
		"Poll a pending approval by id: pending, approved, denied, or expired.",
		true, func(ctx context.Context, in approvalInput) (any, error) {
			return s.svc.Approval(ctx, in.ID)
		})
}
