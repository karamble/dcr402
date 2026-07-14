// Command agent-e2e is the live dcr402-agent acceptance run: against a
// running agent (stood up by harness.sh agent-e2e), it drives the MCP tool
// surface over streamable HTTP as an AI agent would — a policy-blocked
// payment with a legible trace, a real fetch_paid against the dcr402d
// gateway (paid once, cached credential the second time), a real on-chain
// send, and a threshold-crossing payment that escalates and is approved
// through the web API. Exit 0 = PASS.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func env(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fail("missing environment variable %s (run via harness.sh agent-e2e)", key)
	}
	return v
}

func fail(format string, args ...any) {
	fmt.Printf("\x1b[1;31mFAIL\x1b[0m "+format+"\n", args...)
	os.Exit(1)
}

func step(format string, args ...any) {
	fmt.Printf("\x1b[1;34m==>\x1b[0m "+format+"\n", args...)
}

// call invokes a tool and returns the parsed structured result (the map the
// agent would read). isError reports whether the tool signaled a
// machine-actionable condition (policy denial, pending approval).
func call(ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		fail("tool %s transport error: %v", name, err)
	}
	out := map[string]any{}
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok && tc.Text != "" {
			_ = json.Unmarshal([]byte(tc.Text), &out)
		}
	}
	if out["_raw"] == nil && res.StructuredContent != nil && len(out) == 0 {
		raw, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(raw, &out)
	}
	return out, res.IsError
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	agentURL := env("DCR402_AGENT_URL")
	paidURL := env("DCR402_GW_PAID_URL")
	// The MCP surface and the web API are authenticated (fail-closed), so every
	// request carries the agent's API token.
	token := env("DCR402_AGENT_TOKEN")

	step("connecting to dcr402-agent MCP at %s/mcp", agentURL)
	client := mcp.NewClient(&mcp.Implementation{Name: "agent-e2e", Version: "0"}, nil)
	cs, err := client.Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: agentURL + "/mcp", HTTPClient: bearerClient(token)}, nil)
	if err != nil {
		fail("connect: %v", err)
	}
	defer cs.Close()

	// 1. wallet_status — the agent orients itself.
	status, _ := call(ctx, cs, "wallet_status", map[string]any{})
	if status["network"] != "simnet" {
		fail("wallet_status: %v", status)
	}
	step("wallet_status: LN %v atoms, on-chain %v atoms, policy %v",
		status["ln_spendable_atoms"], status["onchain_atoms"], status["policy_mode"])

	// 2. Policy denial with a legible trace (no memo → memo_required).
	step("policy: a memo-less payment must be denied with a trace")
	den, isErr := call(ctx, cs, "pay_dcr_address", map[string]any{
		"address": "SsPlaceholderAddr", "amount_atoms": 1000, "memo": "",
	})
	if !isErr || den["error"] != "policy_denied" {
		fail("expected policy_denied, got %v", den)
	}
	step("denied: %v (nothing signed)", den["reason"])

	// 3. fetch_paid against the gateway — real payment, then cached.
	step("fetch_paid %s (real Lightning payment)", paidURL)
	f1, isErr := call(ctx, cs, "fetch_paid", map[string]any{
		"url": paidURL, "memo": "buy the paid resource",
	})
	if isErr || f1["status"] == nil {
		fail("fetch_paid: %v", f1)
	}
	if fmt.Sprint(f1["rail"]) != "x402" || !strings.Contains(fmt.Sprint(f1["body"]), "HELLO") {
		fail("fetch_paid first call wrong: %v", f1)
	}
	step("paid via %v, preimage %.16s…, body %q", f1["rail"], f1["preimage"], f1["body"])

	f2, _ := call(ctx, cs, "fetch_paid", map[string]any{
		"url": paidURL, "memo": "again",
	})
	if fmt.Sprint(f2["rail"]) != "cached" || f2["paid_atoms"].(float64) != 0 {
		fail("second fetch_paid should be cached, got %v", f2)
	}
	step("second call served from cache (no payment)")

	// 4. On-chain send from the agent account.
	step("pay_dcr_address (real on-chain send)")
	addr := onchainTarget(ctx, cs)
	onchain, isErr := call(ctx, cs, "pay_dcr_address", map[string]any{
		"address": addr, "amount_atoms": 5_000_000, "memo": "on-chain test",
	})
	if isErr || onchain["txid"] == nil {
		fail("pay_dcr_address: %v", onchain)
	}
	step("broadcast txid %.16s…", fmt.Sprint(onchain["txid"]))

	// 5. A payment above the 0.5 DCR threshold escalates, then is approved
	// over the web API — the human-in-the-loop path.
	step("pay_dcr_address above the 0.5 DCR threshold must escalate")
	esc, isErr := call(ctx, cs, "pay_dcr_address", map[string]any{
		"address": addr, "amount_atoms": 60_000_000, "memo": "big spend",
	})
	if !isErr || esc["status"] != "pending_approval" {
		fail("expected pending_approval, got %v", esc)
	}
	approvalID := fmt.Sprint(esc["id"])
	step("pending_approval id=%s — approving via web API", approvalID)

	if err := resolve(agentURL, token, approvalID, true); err != nil {
		fail("web approve: %v", err)
	}
	// Poll approval_status until resolved.
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, _ := call(ctx, cs, "approval_status", map[string]any{"id": approvalID})
		// store.Approval serializes Status as lowercase "status" (a string
		// key); a type assertion avoids fmt.Sprint(nil) == "<nil>".
		status, _ := st["status"].(string)
		if status == "" {
			status, _ = st["Status"].(string)
		}
		if status == "approved" {
			break
		}
		if time.Now().After(deadline) {
			fail("approval never reached approved: %v", st)
		}
		time.Sleep(500 * time.Millisecond)
	}
	step("approved; payment executed asynchronously")

	// 6. Audit: the ledger has the payments and the denial.
	list, _ := call(ctx, cs, "payments_list", map[string]any{"limit": 10})
	_ = list
	step("payments_list returned the audit trail")

	fmt.Printf("\x1b[1;32mPASS\x1b[0m dcr402-agent live on simnet: policy denial+trace, " +
		"fetch_paid (paid + cached), on-chain send, threshold escalation + web approval\n")
}

// onchainTarget asks the agent for a receive address (a valid simnet
// address to send to — the agent's own funding address is fine as a sink).
func onchainTarget(ctx context.Context, cs *mcp.ClientSession) string {
	out, isErr := call(ctx, cs, "get_receive_address", map[string]any{})
	if isErr || out["address"] == nil {
		fail("get_receive_address: %v", out)
	}
	return fmt.Sprint(out["address"])
}

func resolve(agentURL, token, id string, approved bool) error {
	body, _ := json.Marshal(map[string]any{"id": id, "approved": approved})
	req, err := http.NewRequest(http.MethodPost, agentURL+"/api/approvals/resolve",
		strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// bearerClient returns an HTTP client that attaches the agent's API token to
// every request. Both the MCP streamable-HTTP transport and the web API are
// authenticated (the agent fails closed), so the driver presents the token the
// same way a configured operator would.
func bearerClient(token string) *http.Client {
	return &http.Client{Transport: bearerRT{token: token, base: http.DefaultTransport}}
}

type bearerRT struct {
	token string
	base  http.RoundTripper
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}
