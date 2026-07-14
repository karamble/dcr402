package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/karamble/dcr402/agent/approve"
	"github.com/karamble/dcr402/agent/payclient"
	"github.com/karamble/dcr402/agent/policy"
	"github.com/karamble/dcr402/agent/store"
)

// LNRail is the Lightning dependency (rails/ln.Client satisfies it).
type LNRail interface {
	DecodeInvoice(invoice string) (LNInvoice, error)
	Pay(ctx context.Context, invoice string, feeLimitAtoms int64, timeout time.Duration) (preimageHex string, feeAtoms int64, err error)
	SpendableAtoms(ctx context.Context) (int64, error)
	Info(ctx context.Context) (pubkey string, synced bool, err error)
	CreateInvoice(ctx context.Context, amountAtoms int64, memo string, expiry time.Duration) (invoice string, paymentHash string, err error)
}

// LNInvoice mirrors rails/ln.Invoice without importing it into the tool
// surface.
type LNInvoice struct {
	AmountAtoms int64
	Destination string
	PaymentHash string
	Description string
	Timestamp   time.Time
	Expiry      time.Duration
}

// OnchainRail is the on-chain dependency (rails/onchain.Client satisfies
// it). Nil disables on-chain tools.
type OnchainRail interface {
	BalanceAtoms(ctx context.Context, requiredConfs int32) (spendable, total int64, err error)
	NewAddress(ctx context.Context) (string, error)
	Send(ctx context.Context, address string, amountAtoms int64) (txid string, err error)
}

// PayClient is the fetch_paid engine (payclient.Client satisfies it).
type PayClient interface {
	FetchPaid(ctx context.Context, url string, opt payclient.FetchOptions) (*payclient.Result, error)
	Estimate(ctx context.Context, target string) (*payclient.Estimate, error)
}

// Service is the policy-gated payment surface every front-end (MCP tools,
// web API) calls. It is the single place attempts are recorded, policy is
// enforced, payments execute, and receipts are written — front-ends never
// touch a rail or the policy engine directly.
type Service struct {
	policy   *policy.Engine
	store    store.Store
	ln       LNRail
	onchain  OnchainRail
	pay      PayClient
	approval *approve.Registry
	now      func() time.Time

	// payMu serializes the policy-check-then-pay critical section across
	// every payment path (direct, fetch, and approved continuations).
	// Budget/velocity enforcement derives usage from the ledger and would
	// otherwise let two concurrent tool calls both pass a check that only
	// one should — this makes each payment observe the prior one's spend.
	payMu sync.Mutex

	feeLimitAtoms int64
	payTimeout    time.Duration
}

// ServiceDeps wires a Service.
type ServiceDeps struct {
	Policy   *policy.Engine
	Store    store.Store
	LN       LNRail
	Onchain  OnchainRail // optional
	Pay      PayClient
	Approval *approve.Registry
	Now      func() time.Time
}

// NewService builds the payment service.
func NewService(d ServiceDeps) *Service {
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		policy: d.Policy, store: d.Store, ln: d.LN, onchain: d.Onchain,
		pay: d.Pay, approval: d.Approval, now: now,
		feeLimitAtoms: 10_000, payTimeout: 60 * time.Second,
	}
}

// PolicyError carries a denial's full context for machine-actionable tool
// errors.
type PolicyError struct {
	Decision policy.Decision
}

func (e *PolicyError) Error() string {
	return "policy_denied: " + e.Decision.Reason
}

// preApprovedKey marks a context whose payment the owner already approved,
// so the policy gate treats an Escalate outcome as Allow. The value carries
// the approved amount as a ceiling: if a fetch re-quotes higher after the
// invoice expired during the approval TTL, the gate refuses rather than
// silently overpaying what the owner authorized.
type ctxKey int

const preApprovedKey ctxKey = iota

type preApproved struct{ maxAtoms int64 }

// needsApprovalError is the gate's internal signal that a fetch escalated;
// Service.FetchPaid catches it, parks the approval, and reports
// PendingError to the caller.
type needsApprovalError struct {
	attemptID int64
	decision  policy.Decision
	attempt   policy.Attempt
}

func (e *needsApprovalError) Error() string { return "needs_approval" }

// gate is the payclient's policy hook: it records the attempt with its
// trace and enforces policy BEFORE any payment. Deny → *PolicyError;
// escalate → *needsApprovalError (unless the context is pre-approved).
func (s *Service) gate(ctx context.Context, a policy.Attempt) (int64, error) {
	usage, err := s.usage(ctx)
	if err != nil {
		return 0, err
	}
	dec := s.policy.Current().Evaluate(a, usage)
	attemptID, err := s.record(ctx, a, dec)
	if err != nil {
		return 0, err
	}
	switch dec.Outcome {
	case policy.Allow:
		return attemptID, nil
	case policy.Deny:
		return 0, &PolicyError{Decision: dec}
	case policy.Escalate:
		if ap, ok := ctx.Value(preApprovedKey).(preApproved); ok {
			if a.AmountAtoms > ap.maxAtoms {
				dec.Outcome = policy.Deny
				dec.Reason = fmt.Sprintf("challenge_changed: re-quoted %d atoms exceeds the approved %d",
					a.AmountAtoms, ap.maxAtoms)
				return 0, &PolicyError{Decision: dec}
			}
			return attemptID, nil // owner already approved this amount
		}
		return 0, &needsApprovalError{attemptID: attemptID, decision: dec, attempt: a}
	}
	return 0, fmt.Errorf("agent: unknown decision %q", dec.Outcome)
}

func (s *Service) usage(ctx context.Context) (policy.Usage, error) {
	u, err := s.store.Usage(ctx, s.now())
	if err != nil {
		return policy.Usage{}, err
	}
	return policy.Usage{DayAtoms: u.DayAtoms, WeekAtoms: u.WeekAtoms, HourCount: u.HourCount}, nil
}

func (s *Service) record(ctx context.Context, a policy.Attempt, dec policy.Decision) (int64, error) {
	trace, _ := json.Marshal(dec.Trace)
	return s.store.RecordAttempt(ctx, store.Attempt{
		Tool:        a.Tool,
		Rail:        a.Rail,
		Destination: a.Dest,
		AmountAtoms: a.AmountAtoms,
		Memo:        a.Memo,
		Decision:    store.Decision(dec.Outcome),
		RuleTrace:   trace,
		CreatedAt:   s.now(),
	})
}

func summaryFor(a policy.Attempt, dec policy.Decision) string {
	dcr := float64(a.AmountAtoms) / 1e8
	// %q on Dest and Memo (both agent-controlled) neutralizes newline
	// injection, so a crafted destination cannot forge a second prompt.
	return fmt.Sprintf("dcr402-agent approval: %s wants to spend %.8g DCR to %q. memo: %q. "+
		"Reply \"yes <id>\" to approve or \"no <id>\" to deny; send \"freeze\" to halt all "+
		"spending (\"unfreeze\" resumes). Remaining today: %.8g DCR.",
		a.Tool, dcr, a.Dest, a.Memo, float64(dec.RemainingDayAtoms)/1e8)
}

// --- Direct payment tools (pay_ln_invoice, pay_dcr_address) ----------------

// PayResult is a completed direct payment.
type PayResult struct {
	AttemptID   int64  `json:"attempt_id"`
	PaymentID   int64  `json:"payment_id"`
	Rail        string `json:"rail"`
	AmountAtoms int64  `json:"amount_atoms"`
	FeeAtoms    int64  `json:"fee_atoms,omitempty"`
	Preimage    string `json:"preimage,omitempty"`
	TxID        string `json:"txid,omitempty"`
}

// evaluateAndRecord runs policy for a direct (non-fetch) payment, records
// the attempt, and returns the attempt id on allow. Deny → *PolicyError;
// escalate → the approval created (caller decides continuation).
func (s *Service) evaluateAndRecord(ctx context.Context, a policy.Attempt) (int64, policy.Decision, error) {
	usage, err := s.usage(ctx)
	if err != nil {
		return 0, policy.Decision{}, err
	}
	dec := s.policy.Current().Evaluate(a, usage)
	id, err := s.record(ctx, a, dec)
	if err != nil {
		return 0, dec, err
	}
	return id, dec, nil
}

// PayLNInvoice pays a BOLT11 invoice under policy.
func (s *Service) PayLNInvoice(ctx context.Context, invoice, memo string, maxAmountAtoms int64) (*PayResult, error) {
	dec, err := s.ln.DecodeInvoice(invoice)
	if err != nil {
		return nil, err
	}
	if dec.AmountAtoms <= 0 {
		return nil, fmt.Errorf("agent: invoice carries no amount (amountless invoices are not payable)")
	}
	if maxAmountAtoms > 0 && dec.AmountAtoms > maxAmountAtoms {
		return nil, fmt.Errorf("agent: invoice amount %d exceeds max_amount %d", dec.AmountAtoms, maxAmountAtoms)
	}
	a := policy.Attempt{
		Tool: "pay_ln_invoice", Rail: "ln", DestKind: policy.DestLNNode,
		Dest: dec.Destination, AmountAtoms: dec.AmountAtoms, Memo: memo,
	}
	return s.executeOrEscalate(ctx, a, func(ctx context.Context, attemptID int64) (*PayResult, error) {
		return s.doLNPay(ctx, attemptID, invoice, dec)
	})
}

// PayDCRAddress sends on-chain from the dedicated account under policy.
func (s *Service) PayDCRAddress(ctx context.Context, address string, amountAtoms int64, memo string) (*PayResult, error) {
	if s.onchain == nil {
		return nil, fmt.Errorf("agent: the on-chain rail is not configured")
	}
	if amountAtoms <= 0 {
		return nil, fmt.Errorf("agent: amount must be positive")
	}
	a := policy.Attempt{
		Tool: "pay_dcr_address", Rail: "onchain", DestKind: policy.DestDCRAddress,
		Dest: address, AmountAtoms: amountAtoms, Memo: memo,
	}
	return s.executeOrEscalate(ctx, a, func(ctx context.Context, attemptID int64) (*PayResult, error) {
		return s.doOnchainPay(ctx, attemptID, address, amountAtoms)
	})
}

// executeOrEscalate runs the policy pipeline and either executes exec now
// (allow), returns *PolicyError (deny), or parks an approval whose
// continuation runs exec asynchronously (escalate), returning
// *approve.PendingError.
func (s *Service) executeOrEscalate(ctx context.Context, a policy.Attempt, exec func(context.Context, int64) (*PayResult, error)) (*PayResult, error) {
	s.payMu.Lock()
	defer s.payMu.Unlock()
	if err := s.notFrozen(ctx); err != nil {
		return nil, err
	}
	attemptID, dec, err := s.evaluateAndRecord(ctx, a)
	if err != nil {
		return nil, err
	}
	switch dec.Outcome {
	case policy.Allow:
		return exec(ctx, attemptID)
	case policy.Deny:
		return nil, &PolicyError{Decision: dec}
	case policy.Escalate:
		appr, err := s.approval.Escalate(ctx, attemptID, dec.ApprovalChannel, dec.ApprovalTTL,
			summaryFor(a, dec), func(runCtx context.Context, approved bool, _ string) {
				if approved {
					// Serialize the approved execution against other
					// payments too (the parent already released payMu).
					s.payMu.Lock()
					defer s.payMu.Unlock()
					_, _ = exec(runCtx, attemptID)
				}
			})
		if err != nil {
			return nil, err
		}
		return nil, &approve.PendingError{ID: appr.ID, ExpiresAt: appr.ExpiresAt}
	}
	return nil, fmt.Errorf("agent: unknown decision %q", dec.Outcome)
}

func (s *Service) doLNPay(ctx context.Context, attemptID int64, invoice string, dec LNInvoice) (*PayResult, error) {
	preimage, fee, err := s.ln.Pay(ctx, invoice, s.feeLimitAtoms, s.payTimeout)
	if err != nil {
		return nil, err
	}
	receipt, _ := json.Marshal(map[string]string{
		"invoice": invoice, "preimage": preimage,
		"settledAt": s.now().UTC().Format(time.RFC3339),
		"amount":    strconv.FormatInt(dec.AmountAtoms, 10),
	})
	pid, err := s.store.RecordPayment(ctx, store.Payment{
		AttemptID: attemptID, Rail: "ln", AmountAtoms: dec.AmountAtoms, FeeAtoms: fee,
		Invoice: invoice, Preimage: preimage, CreatedAt: s.now(), Receipt: receipt,
	})
	if err != nil {
		return nil, err
	}
	return &PayResult{AttemptID: attemptID, PaymentID: pid, Rail: "ln",
		AmountAtoms: dec.AmountAtoms, FeeAtoms: fee, Preimage: preimage}, nil
}

func (s *Service) doOnchainPay(ctx context.Context, attemptID int64, address string, amountAtoms int64) (*PayResult, error) {
	txid, err := s.onchain.Send(ctx, address, amountAtoms)
	if err != nil {
		return nil, err
	}
	receipt, _ := json.Marshal(map[string]string{
		"txid": txid, "payTo": address,
		"settledAt": s.now().UTC().Format(time.RFC3339),
		"amount":    strconv.FormatInt(amountAtoms, 10),
	})
	pid, err := s.store.RecordPayment(ctx, store.Payment{
		AttemptID: attemptID, Rail: "onchain", AmountAtoms: amountAtoms,
		TxID: txid, CreatedAt: s.now(), Receipt: receipt,
	})
	if err != nil {
		return nil, err
	}
	return &PayResult{AttemptID: attemptID, PaymentID: pid, Rail: "onchain",
		AmountAtoms: amountAtoms, TxID: txid}, nil
}

// --- fetch_paid ------------------------------------------------------------

// FetchPaid runs the paid-HTTP flow (policy enforced inside the payclient
// via s.gate). On success it records the payment. When the charge exceeds
// the approval threshold it parks an approval and returns
// *approve.PendingError; on the owner's yes, the fetch re-runs
// asynchronously on a pre-approved context.
func (s *Service) FetchPaid(ctx context.Context, url string, opt payclient.FetchOptions) (*payclient.Result, error) {
	s.payMu.Lock()
	defer s.payMu.Unlock()
	if err := s.notFrozen(ctx); err != nil {
		return nil, err
	}
	res, err := s.pay.FetchPaid(ctx, url, opt)
	var needs *needsApprovalError
	if errors.As(err, &needs) {
		approvedAtoms := needs.attempt.AmountAtoms
		appr, escErr := s.approval.Escalate(ctx, needs.attemptID,
			needs.decision.ApprovalChannel, needs.decision.ApprovalTTL,
			summaryFor(needs.attempt, needs.decision),
			func(runCtx context.Context, approved bool, _ string) {
				if approved {
					// s.FetchPaid re-acquires payMu (the parent released on
					// return); the ceiling refuses a higher re-quote.
					_, _ = s.FetchPaid(
						context.WithValue(runCtx, preApprovedKey, preApproved{maxAtoms: approvedAtoms}),
						url, opt)
				}
			})
		if escErr != nil {
			return nil, escErr
		}
		return nil, &approve.PendingError{ID: appr.ID, ExpiresAt: appr.ExpiresAt}
	}
	// Record any settled payment BEFORE surfacing a merchant error. A merchant
	// that accepts the Lightning payment and then returns an HTTP error still
	// took the money; returning first would leave the payment off the ledger,
	// and since the day/week/velocity caps are all ledger-derived they would
	// read zero and let the same call re-pay without bound.
	if res != nil && res.PaidAtoms > 0 {
		host := hostOf(url)
		_, _ = s.store.RecordPayment(ctx, store.Payment{
			AttemptID: res.AttemptID, Rail: res.Rail, AmountAtoms: res.PaidAtoms,
			FeeAtoms: res.FeeAtoms, Invoice: res.Invoice, Preimage: res.Preimage,
			L402Service: host, Receipt: res.Receipt, CreatedAt: s.now(),
		})
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// Gatekeeper exposes the service's policy hook to the payclient.
func (s *Service) Gatekeeper() payclient.Gatekeeper { return s.gate }

// --- read tools ------------------------------------------------------------

// Estimate probes a URL/invoice without paying.
func (s *Service) Estimate(ctx context.Context, target string) (*payclient.Estimate, error) {
	return s.pay.Estimate(ctx, target)
}

// GetReceiveAddress returns an inbound funding address (on-chain rail).
func (s *Service) GetReceiveAddress(ctx context.Context) (string, error) {
	if s.onchain == nil {
		return "", fmt.Errorf("agent: the on-chain rail is not configured")
	}
	return s.onchain.NewAddress(ctx)
}

// CreateInvoice mints an inbound LN invoice.
func (s *Service) CreateInvoice(ctx context.Context, amountAtoms int64, memo string) (invoice, paymentHash string, err error) {
	if amountAtoms <= 0 {
		return "", "", fmt.Errorf("agent: amount must be positive")
	}
	return s.ln.CreateInvoice(ctx, amountAtoms, memo, time.Hour)
}

// Status is the wallet/policy snapshot for wallet_status and the web UI.
type Status struct {
	Network            string `json:"network"`
	NodePubkey         string `json:"node_pubkey"`
	Synced             bool   `json:"synced"`
	LNSpendableAtoms   int64  `json:"ln_spendable_atoms"`
	OnchainAtoms       int64  `json:"onchain_atoms"`
	OnchainEnabled     bool   `json:"onchain_enabled"`
	DayAtoms           int64  `json:"day_spent_atoms"`
	WeekAtoms          int64  `json:"week_spent_atoms"`
	HourCount          int64  `json:"hour_payment_count"`
	PolicyMode         string `json:"policy_mode"`
	DailyBudgetAtoms   int64  `json:"daily_budget_atoms"`
	WeeklyBudgetAtoms  int64  `json:"weekly_budget_atoms"`
	MaxPerPaymentAtoms int64  `json:"max_per_payment_atoms"`
	ThresholdAtoms     int64  `json:"approval_threshold_atoms"`
	PendingApprovals   int    `json:"pending_approvals"`
	Frozen             bool   `json:"frozen"`
	FrozenReason       string `json:"frozen_reason,omitempty"`
}

// Status assembles the snapshot.
func (s *Service) Status(ctx context.Context, network string) (Status, error) {
	st := Status{Network: network, OnchainEnabled: s.onchain != nil}
	if pk, synced, err := s.ln.Info(ctx); err == nil {
		st.NodePubkey, st.Synced = pk, synced
	}
	if bal, err := s.ln.SpendableAtoms(ctx); err == nil {
		st.LNSpendableAtoms = bal
	}
	if s.onchain != nil {
		if spendable, _, err := s.onchain.BalanceAtoms(ctx, 1); err == nil {
			st.OnchainAtoms = spendable
		}
	}
	if u, err := s.usage(ctx); err == nil {
		st.DayAtoms, st.WeekAtoms, st.HourCount = u.DayAtoms, u.WeekAtoms, u.HourCount
	}
	p := s.policy.Current()
	st.PolicyMode = p.Mode
	st.DailyBudgetAtoms = p.DailyBudgetAtoms
	st.WeeklyBudgetAtoms = p.WeeklyBudgetAtoms
	st.MaxPerPaymentAtoms = p.MaxPerPaymentAtoms
	st.ThresholdAtoms = p.ThresholdAtoms
	if pending, err := s.approval.Pending(ctx); err == nil {
		st.PendingApprovals = len(pending)
	}
	if fz := s.FreezeStatus(ctx); fz.Frozen {
		st.Frozen, st.FrozenReason = true, fz.Reason
	}
	return st, nil
}

// Payments lists recent payments.
func (s *Service) Payments(ctx context.Context, limit int, beforeID int64) ([]store.Payment, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.store.ListPayments(ctx, limit, beforeID)
}

// Attempts lists recent attempts (allowed and blocked) for the audit view.
func (s *Service) Attempts(ctx context.Context, limit int, beforeID int64) ([]store.Attempt, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.store.ListAttempts(ctx, limit, beforeID)
}

// Receipt returns one payment's receipt.
func (s *Service) Receipt(ctx context.Context, id int64) (store.Payment, error) {
	return s.store.GetPayment(ctx, id)
}

// Approval returns one approval's state.
func (s *Service) Approval(ctx context.Context, id string) (store.Approval, error) {
	return s.approval.Get(ctx, id)
}

// PendingApprovals lists live approvals (web dashboard).
func (s *Service) PendingApprovals(ctx context.Context) ([]store.Approval, error) {
	return s.approval.Pending(ctx)
}

// ResolveApproval delivers a web/UI verdict.
func (s *Service) ResolveApproval(ctx context.Context, id string, approved bool, responder string) (bool, error) {
	return s.approval.Resolve(ctx, id, approved, responder)
}

func hostOf(rawURL string) string {
	if i := strings.Index(rawURL, "://"); i >= 0 {
		rest := rawURL[i+3:]
		if j := strings.IndexAny(rest, "/:"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return rawURL
}
