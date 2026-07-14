// Package policy is dcr402-agent's deterministic spending policy engine:
// the LLM explains, never decides. Policy is a YAML file the owner edits;
// evaluation is a fixed short-circuit rule pipeline producing a full trace
// for the audit ledger; no policy-mutation surface exists anywhere in the
// agent's tool set.
package policy

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// Outcomes.
type Outcome string

const (
	Allow    Outcome = "allow"
	Deny     Outcome = "deny"
	Escalate Outcome = "escalate"
)

// Destination kinds.
type DestKind string

const (
	DestDomain     DestKind = "domain"
	DestLNNode     DestKind = "ln_node"
	DestDCRAddress DestKind = "dcr_address"
)

// Modes.
const (
	ModeDefaultDeny  = "default-deny"
	ModeDefaultAllow = "default-allow"
)

// Approval channels.
const (
	ChannelBisonRelay = "bisonrelay"
	ChannelWeb        = "web"
	ChannelBoth       = "both"
)

// Attempt is one payment intent under evaluation. Dest carries the
// bare destination for its kind: a hostname (no port), an LN node pubkey
// (hex), or a DCR address.
type Attempt struct {
	Tool        string
	Rail        string
	DestKind    DestKind
	Dest        string
	AmountAtoms int64
	Memo        string
}

// Usage is the current budget consumption (derived by the caller from the
// audit store).
type Usage struct {
	DayAtoms  int64
	WeekAtoms int64
	HourCount int64
}

// RuleResult is one evaluated rule in the trace.
type RuleResult struct {
	Rule   string `json:"rule"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Decision is the deterministic outcome plus its full trace.
type Decision struct {
	Outcome Outcome
	Trace   []RuleResult
	// Reason is the failing rule's detail on deny (machine-actionable).
	Reason string
	// Remaining budget after this attempt would settle (informational).
	RemainingDayAtoms  int64
	RemainingWeekAtoms int64
	// Escalation parameters (set when Outcome == Escalate).
	ApprovalTTL     time.Duration
	ApprovalChannel string
}

// file is the YAML shape (dcrpay spec §4).
type file struct {
	Mode   string `yaml:"mode"`
	Limits struct {
		MaxPerPayment      string `yaml:"max_per_payment"`
		DailyBudget        string `yaml:"daily_budget"`
		WeeklyBudget       string `yaml:"weekly_budget"`
		MaxPaymentsPerHour int64  `yaml:"max_payments_per_hour"`
	} `yaml:"limits"`
	Approval struct {
		Threshold string `yaml:"threshold"`
		TTL       string `yaml:"ttl"`
		Channel   string `yaml:"channel"`
	} `yaml:"approval"`
	Allow struct {
		Domains      []string `yaml:"domains"`
		LNNodes      []string `yaml:"ln_nodes"`
		DCRAddresses []string `yaml:"dcr_addresses"`
	} `yaml:"allow"`
	Deny struct {
		Domains []string `yaml:"domains"`
	} `yaml:"deny"`
	MemoRequired bool `yaml:"memo_required"`
}

// Policy is a compiled, immutable policy.
type Policy struct {
	Mode               string
	MaxPerPaymentAtoms int64 // 0 = unlimited
	DailyBudgetAtoms   int64
	WeeklyBudgetAtoms  int64
	MaxPaymentsPerHour int64
	ThresholdAtoms     int64 // 0 = never escalate
	ApprovalTTL        time.Duration
	ApprovalChannel    string
	AllowDomains       []string
	DenyDomains        []string
	AllowLNNodes       map[string]bool
	AllowDCRAddresses  map[string]bool
	MemoRequired       bool
}

// ParseAmount converts a policy amount ("0.5 DCR", "0.5", "2") to atoms
// exactly — no floats, max 8 decimals. Empty means 0 (unlimited).
func ParseAmount(s string) (int64, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "DCR"))
	if s == "" {
		return 0, nil
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 {
		return 0, fmt.Errorf("policy: invalid DCR amount %q", s)
	}
	var f int64
	if hasFrac {
		if frac == "" || len(frac) > 8 {
			return 0, fmt.Errorf("policy: invalid DCR amount %q (max 8 decimals)", s)
		}
		f, err = strconv.ParseInt(frac+strings.Repeat("0", 8-len(frac)), 10, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("policy: invalid DCR amount %q", s)
		}
	}
	const atomsPerDCR = 100_000_000
	if w > (1<<63-1)/atomsPerDCR-1 {
		return 0, fmt.Errorf("policy: DCR amount %q out of range", s)
	}
	return w*atomsPerDCR + f, nil
}

// Parse compiles YAML policy bytes.
func Parse(raw []byte) (*Policy, error) {
	var f file
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}

	p := &Policy{
		Mode:               f.Mode,
		MaxPaymentsPerHour: f.Limits.MaxPaymentsPerHour,
		ApprovalChannel:    f.Approval.Channel,
		MemoRequired:       f.MemoRequired,
		AllowLNNodes:       map[string]bool{},
		AllowDCRAddresses:  map[string]bool{},
	}
	if p.Mode == "" {
		p.Mode = ModeDefaultDeny
	}
	if p.Mode != ModeDefaultDeny && p.Mode != ModeDefaultAllow {
		return nil, fmt.Errorf("policy: unknown mode %q", p.Mode)
	}
	var err error
	if p.MaxPerPaymentAtoms, err = ParseAmount(f.Limits.MaxPerPayment); err != nil {
		return nil, err
	}
	if p.DailyBudgetAtoms, err = ParseAmount(f.Limits.DailyBudget); err != nil {
		return nil, err
	}
	if p.WeeklyBudgetAtoms, err = ParseAmount(f.Limits.WeeklyBudget); err != nil {
		return nil, err
	}
	if p.ThresholdAtoms, err = ParseAmount(f.Approval.Threshold); err != nil {
		return nil, err
	}
	if f.Approval.TTL != "" {
		if p.ApprovalTTL, err = time.ParseDuration(f.Approval.TTL); err != nil {
			return nil, fmt.Errorf("policy: approval.ttl: %w", err)
		}
	}
	if p.ApprovalTTL <= 0 {
		p.ApprovalTTL = 15 * time.Minute
	}
	switch p.ApprovalChannel {
	case "":
		p.ApprovalChannel = ChannelWeb
	case ChannelBisonRelay, ChannelWeb, ChannelBoth:
	default:
		return nil, fmt.Errorf("policy: unknown approval channel %q", p.ApprovalChannel)
	}
	for _, d := range f.Allow.Domains {
		p.AllowDomains = append(p.AllowDomains, strings.ToLower(strings.TrimSpace(d)))
	}
	for _, d := range f.Deny.Domains {
		p.DenyDomains = append(p.DenyDomains, strings.ToLower(strings.TrimSpace(d)))
	}
	for _, n := range f.Allow.LNNodes {
		p.AllowLNNodes[strings.ToLower(strings.TrimSpace(n))] = true
	}
	for _, a := range f.Allow.DCRAddresses {
		p.AllowDCRAddresses[strings.TrimSpace(a)] = true
	}
	return p, nil
}

// matchDomain reports whether host matches pattern (exact, or leading
// wildcard "*.example.com" matching any subdomain).
func matchDomain(pattern, host string) bool {
	host = strings.ToLower(host)
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+rest) || host == rest
	}
	return host == pattern
}

func matchAny(patterns []string, host string) (string, bool) {
	for _, p := range patterns {
		if matchDomain(p, host) {
			return p, true
		}
	}
	return "", false
}

// Evaluate runs the deterministic rule pipeline (spec §4 order,
// short-circuit on the first failing rule): memo → destination →
// per-payment cap → velocity → daily budget → weekly budget → approval
// threshold. The returned trace lists every rule that was evaluated.
func (p *Policy) Evaluate(a Attempt, u Usage) Decision {
	d := Decision{Outcome: Allow}
	fail := func(rule, detail string) Decision {
		d.Trace = append(d.Trace, RuleResult{Rule: rule, Pass: false, Detail: detail})
		d.Outcome = Deny
		d.Reason = rule + ": " + detail
		d.RemainingDayAtoms = max64(0, p.DailyBudgetAtoms-u.DayAtoms)
		d.RemainingWeekAtoms = max64(0, p.WeeklyBudgetAtoms-u.WeekAtoms)
		return d
	}
	pass := func(rule, detail string) {
		d.Trace = append(d.Trace, RuleResult{Rule: rule, Pass: true, Detail: detail})
	}

	// 1. memo_required
	if p.MemoRequired {
		if strings.TrimSpace(a.Memo) == "" {
			return fail("memo_required", "every payment must carry a reason string")
		}
		pass("memo_required", "")
	}

	// 2. destination allow/deny by kind
	switch a.DestKind {
	case DestDomain:
		host := strings.ToLower(a.Dest)
		if pat, hit := matchAny(p.DenyDomains, host); hit {
			return fail("deny_domain", fmt.Sprintf("%s matches denylist %s", host, pat))
		}
		if p.Mode == ModeDefaultDeny {
			pat, hit := matchAny(p.AllowDomains, host)
			if !hit {
				return fail("allow_domain", fmt.Sprintf("%s is not on the domain allowlist (default-deny)", host))
			}
			pass("allow_domain", "matches "+pat)
		} else {
			pass("allow_domain", "default-allow")
		}
	case DestLNNode:
		node := strings.ToLower(a.Dest)
		if p.Mode == ModeDefaultDeny {
			if !p.AllowLNNodes[node] {
				return fail("allow_ln_node", "destination node is not on the allowlist (default-deny)")
			}
			pass("allow_ln_node", "pinned")
		} else {
			pass("allow_ln_node", "default-allow")
		}
	case DestDCRAddress:
		if p.Mode == ModeDefaultDeny {
			if !p.AllowDCRAddresses[a.Dest] {
				return fail("allow_dcr_address", "address is not on the allowlist (default-deny)")
			}
			pass("allow_dcr_address", "pinned")
		} else {
			pass("allow_dcr_address", "default-allow")
		}
	default:
		return fail("destination", fmt.Sprintf("unknown destination kind %q", a.DestKind))
	}

	// 3. per-payment cap
	if p.MaxPerPaymentAtoms > 0 {
		if a.AmountAtoms > p.MaxPerPaymentAtoms {
			return fail("max_per_payment",
				fmt.Sprintf("%d > %d atoms", a.AmountAtoms, p.MaxPerPaymentAtoms))
		}
		pass("max_per_payment", fmt.Sprintf("%d <= %d atoms", a.AmountAtoms, p.MaxPerPaymentAtoms))
	}

	// 4. velocity
	if p.MaxPaymentsPerHour > 0 {
		if u.HourCount >= p.MaxPaymentsPerHour {
			return fail("max_payments_per_hour",
				fmt.Sprintf("%d payments in the last hour (limit %d)", u.HourCount, p.MaxPaymentsPerHour))
		}
		pass("max_payments_per_hour", fmt.Sprintf("%d/%d", u.HourCount, p.MaxPaymentsPerHour))
	}

	// 5. daily budget
	if p.DailyBudgetAtoms > 0 {
		if u.DayAtoms+a.AmountAtoms > p.DailyBudgetAtoms {
			return fail("daily_budget",
				fmt.Sprintf("%d + %d exceeds %d atoms", u.DayAtoms, a.AmountAtoms, p.DailyBudgetAtoms))
		}
		pass("daily_budget", fmt.Sprintf("%d/%d after", u.DayAtoms+a.AmountAtoms, p.DailyBudgetAtoms))
	}

	// 6. weekly budget
	if p.WeeklyBudgetAtoms > 0 {
		if u.WeekAtoms+a.AmountAtoms > p.WeeklyBudgetAtoms {
			return fail("weekly_budget",
				fmt.Sprintf("%d + %d exceeds %d atoms", u.WeekAtoms, a.AmountAtoms, p.WeeklyBudgetAtoms))
		}
		pass("weekly_budget", fmt.Sprintf("%d/%d after", u.WeekAtoms+a.AmountAtoms, p.WeeklyBudgetAtoms))
	}

	d.RemainingDayAtoms = max64(0, p.DailyBudgetAtoms-u.DayAtoms-a.AmountAtoms)
	d.RemainingWeekAtoms = max64(0, p.WeeklyBudgetAtoms-u.WeekAtoms-a.AmountAtoms)

	// 7. approval threshold
	if p.ThresholdAtoms > 0 && a.AmountAtoms >= p.ThresholdAtoms {
		d.Trace = append(d.Trace, RuleResult{Rule: "approval_threshold", Pass: true,
			Detail: fmt.Sprintf("%d >= %d atoms — owner approval required", a.AmountAtoms, p.ThresholdAtoms)})
		d.Outcome = Escalate
		d.ApprovalTTL = p.ApprovalTTL
		d.ApprovalChannel = p.ApprovalChannel
		return d
	}
	if p.ThresholdAtoms > 0 {
		pass("approval_threshold", fmt.Sprintf("%d < %d atoms", a.AmountAtoms, p.ThresholdAtoms))
	}
	return d
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Engine holds the live policy with atomic hot reload (SIGHUP).
type Engine struct {
	path string
	cur  atomic.Pointer[Policy]
}

// NewEngine loads path and returns a reloadable engine.
func NewEngine(path string) (*Engine, error) {
	e := &Engine{path: path}
	if err := e.Reload(); err != nil {
		return nil, err
	}
	return e, nil
}

// Current returns the live policy.
func (e *Engine) Current() *Policy { return e.cur.Load() }

// Reload re-reads the policy file; on error the previous policy stays live.
func (e *Engine) Reload() error {
	raw, err := os.ReadFile(e.path)
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	p, err := Parse(raw)
	if err != nil {
		return err
	}
	e.cur.Store(p)
	return nil
}
