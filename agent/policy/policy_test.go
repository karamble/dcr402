package policy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// specYAML is the dcrpay spec §4 example, near-verbatim.
const specYAML = `
mode: default-deny
limits:
  max_per_payment: 0.5 DCR
  daily_budget:    2.0 DCR
  weekly_budget:   8.0 DCR
  max_payments_per_hour: 30
approval:
  threshold: 0.1 DCR
  ttl: 15m
  channel: bisonrelay
allow:
  domains:      ["sat.urbandigital.cc", "*.example.com"]
  ln_nodes:     ["03e7156ae33b0a208d0744199163177e909e80176e55d97a2f221ede0f934dd9ad"]
  dcr_addresses: []
deny:
  domains: ["evil.example.com"]
memo_required: true
`

func mustParse(t *testing.T) *Policy {
	t.Helper()
	p, err := Parse([]byte(specYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestParseAmount(t *testing.T) {
	good := map[string]int64{
		"0.5 DCR": 50_000_000, "0.5": 50_000_000, "2": 200_000_000,
		"2.0 DCR": 200_000_000, "0.00000001": 1, "": 0,
	}
	for in, want := range good {
		got, err := ParseAmount(in)
		if err != nil || got != want {
			t.Fatalf("ParseAmount(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"-1", "0.000000001", "x DCR", "1.2.3"} {
		if _, err := ParseAmount(bad); err == nil {
			t.Fatalf("ParseAmount(%q) accepted", bad)
		}
	}
}

func TestParseSpecExample(t *testing.T) {
	p := mustParse(t)
	if p.MaxPerPaymentAtoms != 50_000_000 || p.DailyBudgetAtoms != 200_000_000 ||
		p.WeeklyBudgetAtoms != 800_000_000 || p.MaxPaymentsPerHour != 30 {
		t.Fatalf("limits: %+v", p)
	}
	if p.ThresholdAtoms != 10_000_000 || p.ApprovalTTL != 15*time.Minute ||
		p.ApprovalChannel != ChannelBisonRelay {
		t.Fatalf("approval: %+v", p)
	}
	if !p.MemoRequired || p.Mode != ModeDefaultDeny {
		t.Fatalf("mode/memo: %+v", p)
	}
}

func attempt(kind DestKind, dest string, atoms int64) Attempt {
	return Attempt{Tool: "fetch_paid", Rail: "ln", DestKind: kind,
		Dest: dest, AmountAtoms: atoms, Memo: "test memo"}
}

func lastRule(d Decision) RuleResult { return d.Trace[len(d.Trace)-1] }

func TestEvaluatePipeline(t *testing.T) {
	p := mustParse(t)
	u := Usage{}

	// Allowed small payment to an allowlisted domain.
	d := p.Evaluate(attempt(DestDomain, "sat.urbandigital.cc", 250_000), u)
	if d.Outcome != Allow {
		t.Fatalf("allow case: %+v", d)
	}
	if d.RemainingDayAtoms != 200_000_000-250_000 {
		t.Fatalf("remaining day: %d", d.RemainingDayAtoms)
	}

	// Wildcard allow.
	if d := p.Evaluate(attempt(DestDomain, "api.example.com", 1000), u); d.Outcome != Allow {
		t.Fatalf("wildcard: %+v", d)
	}

	// Denylist beats allowlist wildcard.
	d = p.Evaluate(attempt(DestDomain, "evil.example.com", 1000), u)
	if d.Outcome != Deny || lastRule(d).Rule != "deny_domain" {
		t.Fatalf("denylist: %+v", d)
	}

	// Default-deny unknown domain.
	d = p.Evaluate(attempt(DestDomain, "unknown.com", 1000), u)
	if d.Outcome != Deny || lastRule(d).Rule != "allow_domain" {
		t.Fatalf("unknown domain: %+v", d)
	}

	// Missing memo short-circuits FIRST (nothing else evaluated).
	a := attempt(DestDomain, "unknown.com", 999_999_999)
	a.Memo = "  "
	d = p.Evaluate(a, u)
	if d.Outcome != Deny || len(d.Trace) != 1 || d.Trace[0].Rule != "memo_required" {
		t.Fatalf("memo short-circuit: %+v", d)
	}

	// Per-payment cap.
	d = p.Evaluate(attempt(DestDomain, "sat.urbandigital.cc", 75_000_000), u)
	if d.Outcome != Deny || lastRule(d).Rule != "max_per_payment" {
		t.Fatalf("cap: %+v", d)
	}

	// Velocity.
	d = p.Evaluate(attempt(DestDomain, "sat.urbandigital.cc", 1000), Usage{HourCount: 30})
	if d.Outcome != Deny || lastRule(d).Rule != "max_payments_per_hour" {
		t.Fatalf("velocity: %+v", d)
	}

	// Daily budget (would exceed with this payment).
	d = p.Evaluate(attempt(DestDomain, "sat.urbandigital.cc", 6_000_000),
		Usage{DayAtoms: 195_000_000})
	if d.Outcome != Deny || lastRule(d).Rule != "daily_budget" {
		t.Fatalf("daily: %+v", d)
	}

	// Weekly budget.
	d = p.Evaluate(attempt(DestDomain, "sat.urbandigital.cc", 6_000_000),
		Usage{WeekAtoms: 795_000_000})
	if d.Outcome != Deny || lastRule(d).Rule != "weekly_budget" {
		t.Fatalf("weekly: %+v", d)
	}

	// Threshold escalation.
	d = p.Evaluate(attempt(DestDomain, "sat.urbandigital.cc", 35_000_000), u)
	if d.Outcome != Escalate || d.ApprovalTTL != 15*time.Minute ||
		d.ApprovalChannel != ChannelBisonRelay {
		t.Fatalf("escalate: %+v", d)
	}

	// LN node pinning.
	if d := p.Evaluate(attempt(DestLNNode,
		"03E7156AE33B0A208D0744199163177E909E80176E55D97A2F221EDE0F934DD9AD", 1000), u); d.Outcome != Allow {
		t.Fatalf("ln node (case-insensitive): %+v", d)
	}
	d = p.Evaluate(attempt(DestLNNode, "02aaaa", 1000), u)
	if d.Outcome != Deny || lastRule(d).Rule != "allow_ln_node" {
		t.Fatalf("ln node deny: %+v", d)
	}

	// DCR address allowlist is empty → default-deny blocks all.
	d = p.Evaluate(attempt(DestDCRAddress, "DsAddr", 1000), u)
	if d.Outcome != Deny || lastRule(d).Rule != "allow_dcr_address" {
		t.Fatalf("dcr address: %+v", d)
	}
}

func TestDefaultAllowMode(t *testing.T) {
	p, err := Parse([]byte(`
mode: default-allow
limits: {max_per_payment: "0.5"}
deny:
  domains: ["evil.com"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if d := p.Evaluate(attempt(DestDomain, "anywhere.net", 1000), Usage{}); d.Outcome != Allow {
		t.Fatalf("default-allow domain: %+v", d)
	}
	if d := p.Evaluate(attempt(DestDomain, "evil.com", 1000), Usage{}); d.Outcome != Deny {
		t.Fatalf("default-allow denylist: %+v", d)
	}
	if d := p.Evaluate(attempt(DestLNNode, "02bb", 1000), Usage{}); d.Outcome != Allow {
		t.Fatalf("default-allow node: %+v", d)
	}
	// No memo requirement, no budgets, no threshold → plain allow.
	a := attempt(DestDomain, "anywhere.net", 1000)
	a.Memo = ""
	if d := p.Evaluate(a, Usage{}); d.Outcome != Allow {
		t.Fatalf("memo not required: %+v", d)
	}
}

func TestUnlimitedZeros(t *testing.T) {
	p, err := Parse([]byte("mode: default-allow\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := p.Evaluate(attempt(DestDomain, "x.com", 1<<40), Usage{DayAtoms: 1 << 50, HourCount: 10_000})
	if d.Outcome != Allow {
		t.Fatalf("unlimited: %+v", d)
	}
}

func TestParseErrors(t *testing.T) {
	for name, yaml := range map[string]string{
		"bad mode":    "mode: sometimes\n",
		"bad ttl":     "approval: {ttl: soon}\n",
		"bad amount":  "limits: {daily_budget: lots}\n",
		"bad channel": "approval: {channel: carrier-pigeon}\n",
		"unknown key": "surprise: true\n",
	} {
		if _, err := Parse([]byte(yaml)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestEngineReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte("mode: default-deny\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(path)
	if err != nil {
		t.Fatal(err)
	}
	if e.Current().Mode != ModeDefaultDeny {
		t.Fatal("initial policy wrong")
	}
	// Reload picks up edits.
	if err := os.WriteFile(path, []byte("mode: default-allow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	if e.Current().Mode != ModeDefaultAllow {
		t.Fatal("reload did not apply")
	}
	// A broken edit keeps the previous policy live.
	if err := os.WriteFile(path, []byte("mode: nonsense\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.Reload(); err == nil {
		t.Fatal("broken policy accepted")
	}
	if e.Current().Mode != ModeDefaultAllow {
		t.Fatal("broken reload replaced the live policy")
	}
}
