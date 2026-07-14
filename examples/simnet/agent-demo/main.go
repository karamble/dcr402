// Command agent-demo seeds watchable activity into a running dcr402-agent
// and then leaves it up: it drives a few payments, a policy denial (red
// trace), and finally an over-threshold payment that stays pending so you
// can approve it yourself in the dashboard and watch the feed update live
// over SSE. It does not tear anything down — run `harness.sh stop` when
// finished.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearerClient returns an HTTP client that attaches the agent's API token to
// every request. The MCP surface is authenticated (the agent fails closed), so
// the demo presents the token the same way a configured operator would.
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

// mine asks the harness to mine n blocks so on-chain sends confirm (the
// agent's spendable balance needs confirmations; Lightning does not).
func mine(n int) {
	cmd := os.Getenv("DCR402_E2E_MINE")
	if cmd == "" {
		return
	}
	_ = exec.Command("sh", "-c", fmt.Sprintf("%s %d", cmd, n)).Run()
	time.Sleep(1500 * time.Millisecond) // let the wallet see the blocks
}

func env(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Printf("missing %s (run via harness.sh agent-demo)\n", key)
		os.Exit(1)
	}
	return v
}

func step(format string, args ...any) {
	fmt.Printf("\x1b[1;34m==>\x1b[0m "+format+"\n", args...)
}

func call(ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		fmt.Printf("tool %s error: %v\n", name, err)
		return map[string]any{}
	}
	out := map[string]any{}
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok && tc.Text != "" {
			_ = json.Unmarshal([]byte(tc.Text), &out)
		}
	}
	return out
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	agentURL := env("DCR402_AGENT_URL")
	paidURL := env("DCR402_GW_PAID_URL")
	token := env("DCR402_AGENT_TOKEN")
	pause := 1800 * time.Millisecond

	client := mcp.NewClient(&mcp.Implementation{Name: "agent-demo", Version: "0"}, nil)
	cs, err := client.Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: agentURL + "/mcp", HTTPClient: bearerClient(token)}, nil)
	if err != nil {
		fmt.Printf("connect: %v\n", err)
		os.Exit(1)
	}
	defer cs.Close()

	fmt.Printf("\n  \x1b[1;32m▶  Open the dashboard now:  %s/\x1b[0m\n\n", agentURL)
	step("seeding activity in a few seconds — watch the feed fill in")
	time.Sleep(4 * time.Second)

	// A denial: no memo → memo_required (red trace in the feed).
	step("a memo-less payment (will be denied with a trace)")
	call(ctx, cs, "pay_dcr_address", map[string]any{
		"address": "SsPlaceholderAddr", "amount_atoms": 1000, "memo": "",
	})
	time.Sleep(pause)

	// A paid fetch, then the cached re-access.
	step("fetch_paid (real Lightning payment)")
	call(ctx, cs, "fetch_paid", map[string]any{"url": paidURL, "memo": "buy satellite scene"})
	time.Sleep(pause)
	step("fetch_paid again (served from the cached credential, free)")
	call(ctx, cs, "fetch_paid", map[string]any{"url": paidURL, "memo": "re-read scene"})
	time.Sleep(pause)

	// A small on-chain send (below threshold → completes), then mine so its
	// change confirms and the escalated send below has spendable funds.
	addr := fmt.Sprint(call(ctx, cs, "get_receive_address", map[string]any{})["address"])
	step("pay_dcr_address 0.001 DCR on-chain")
	oc := call(ctx, cs, "pay_dcr_address", map[string]any{
		"address": addr, "amount_atoms": 100_000, "memo": "small top-up",
	})
	if oc["txid"] != nil {
		step("  broadcast %.16s… — mining to confirm", fmt.Sprint(oc["txid"]))
	}
	mine(2)
	time.Sleep(pause)

	// The interactive moment: an over-threshold payment escalates and waits.
	step("pay_dcr_address 0.6 DCR — ABOVE the approval threshold → escalates")
	esc := call(ctx, cs, "pay_dcr_address", map[string]any{
		"address": addr, "amount_atoms": 60_000_000, "memo": "large purchase",
	})
	id := fmt.Sprint(esc["id"])

	fmt.Printf(`
  ┌─────────────────────────────────────────────────────────────┐
  │  A payment is now AWAITING YOUR APPROVAL (id %-6s)          │
  │                                                               │
  │  Open           %-44s│
  │  Click          the green "approve" button on the strip       │
  │  Watch          the payment settle and the feed update live   │
  │                                                               │
  │  (or reply over Bison Relay if configured: yes %-6s)        │
  └─────────────────────────────────────────────────────────────┘

  The agent, gateway, and wallets stay running. Stop everything with:
      ./examples/simnet/harness.sh stop

`, id, agentURL+"/", id)
	step("demo seeded — the dashboard is live and interactive")
}
