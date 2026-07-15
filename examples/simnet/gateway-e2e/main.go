// Command gateway-e2e is the live dcr402d smoke test: against a running
// gateway (stood up by harness.sh gateway-e2e in front of the toy
// upstream), it exercises HTTP per-call gating, the MCP x402 transport
// with a real Lightning payment, a Lightning top-up through the gateway,
// and MCP credit burns. Exit 0 = PASS.
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

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/x402"

	"github.com/karamble/dcr402/examples/simnet/internal/buyerclient"
)

func env(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fail("missing environment variable %s (run via harness.sh gateway-e2e)", key)
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

func getJSON(t *http.Response) map[string]any {
	defer t.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(t.Body).Decode(&v)
	return v
}

func postMCP(url string, body []byte, headers map[string]string) (*http.Response, map[string]any) {
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail("POST %s: %v", url, err)
	}
	return resp, getJSON(resp)
}

func toolCall(id int, tool string, meta map[string]any) []byte {
	params := map[string]any{"name": tool, "arguments": map[string]any{}}
	if meta != nil {
		params["_meta"] = meta
	}
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params,
	})
	return raw
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	gw := env("DCR402_GW_URL")

	buyer, err := buyerclient.Dial(env("DCR402_E2E_BUYER_RPC"),
		env("DCR402_E2E_BUYER_TLS"), env("DCR402_E2E_BUYER_ADMIN_MAC"))
	if err != nil {
		fail("buyer dial: %v", err)
	}
	defer buyer.Close()

	// 1. Gateway is up and self-describing.
	step("checking %s/_dcr402/info", gw)
	resp, err := http.Get(gw + "/_dcr402/info")
	if err != nil || resp.StatusCode != 200 {
		fail("info: %v status=%v", err, resp)
	}
	info := getJSON(resp)
	if info["network"] != "simnet" || info["creditsEnabled"] != true {
		fail("info wrong: %v", info)
	}

	// 2. HTTP per-call: 402 → pay real invoice → forwarded content.
	step("HTTP per-call: GET /api/hello")
	resp, err = http.Get(gw + "/api/hello")
	if err != nil {
		fail("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		fail("expected 402, got %d", resp.StatusCode)
	}
	var pr x402.PaymentRequired
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &pr); err != nil {
		fail("PAYMENT-REQUIRED: %v", err)
	}
	var extra x402.LightningExtra
	if err := json.Unmarshal(pr.Accepts[0].Extra, &extra); err != nil {
		fail("extra: %v", err)
	}
	preimage, err := buyerclient.PayInvoice(ctx, buyer, extra.Invoice)
	if err != nil {
		fail("pay: %v", err)
	}
	sig, _ := x402.EncodeHeader(x402.PaymentPayload{
		X402Version: 2, Accepted: pr.Accepts[0],
		Payload: x402.MustRaw(x402.LightningPayload{Preimage: preimage, PaymentHash: extra.PaymentHash}),
	})
	req, _ := http.NewRequest("GET", gw+"/api/hello", nil)
	req.Header.Set(x402.HeaderPaymentSignature, sig)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		fail("retry: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "HELLO PAID WORLD" {
		fail("paid GET: %d %q", resp.StatusCode, body)
	}
	step("paid content served through the proxy")

	// 3. MCP per-call (route /mcp2): challenge → pay → _meta settlement.
	step("MCP per-call: tools/call hello_tool on /mcp2")
	resp, out := postMCP(gw+"/mcp2", toolCall(1, "hello_tool", nil), nil)
	result, _ := out["result"].(map[string]any)
	if resp.StatusCode != 200 || result == nil || result["isError"] != true {
		fail("unpaid tools/call: %v", out)
	}
	scRaw, _ := json.Marshal(result["structuredContent"])
	var toolPR x402.PaymentRequired
	if err := json.Unmarshal(scRaw, &toolPR); err != nil || len(toolPR.Accepts) == 0 {
		fail("tool challenge: %s", scRaw)
	}
	if toolPR.Resource.URL != gw+"/mcp2" {
		fail("tool resource url %q, want the gateway's own MCP endpoint", toolPR.Resource.URL)
	}
	var toolExtra x402.LightningExtra
	_ = json.Unmarshal(toolPR.Accepts[0].Extra, &toolExtra)
	toolPre, err := buyerclient.PayInvoice(ctx, buyer, toolExtra.Invoice)
	if err != nil {
		fail("pay tool invoice: %v", err)
	}
	pp := x402.PaymentPayload{
		X402Version: 2, Accepted: toolPR.Accepts[0],
		Payload: x402.MustRaw(x402.LightningPayload{Preimage: toolPre, PaymentHash: toolExtra.PaymentHash}),
	}
	resp, out = postMCP(gw+"/mcp2", toolCall(2, "hello_tool",
		map[string]any{dcr402.MetaPayment: pp}), nil)
	result, _ = out["result"].(map[string]any)
	if resp.StatusCode != 200 || result == nil || result["isError"] != false {
		fail("paid tools/call: %v", out)
	}
	if !strings.Contains(fmt.Sprint(result["content"]), "executed hello_tool") {
		fail("tool not executed: %v", result)
	}
	meta, _ := result["_meta"].(map[string]any)
	if meta[dcr402.MetaPaymentResponse] == nil {
		fail("settlement missing from result._meta: %v", result)
	}
	step("MCP tool executed with settlement in result._meta")

	// 4. Lightning top-up through the gateway.
	step("topping up 500000 atoms over Lightning via /_dcr402/topup")
	resp, err = http.Get(gw + "/_dcr402/topup?atoms=500000")
	if err != nil {
		fail("topup GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var topupPR x402.PaymentRequired
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &topupPR); err != nil {
		fail("topup challenge: %v", err)
	}
	if len(topupPR.Accepts) != 2 {
		fail("expected lightning+onchain entries, got %d", len(topupPR.Accepts))
	}
	var lnEntry x402.PaymentRequirements
	for _, e := range topupPR.Accepts {
		if e.TransferMethod() == x402.MethodLightning {
			lnEntry = e
		}
	}
	var topupExtra x402.LightningExtra
	_ = json.Unmarshal(lnEntry.Extra, &topupExtra)
	topupPre, err := buyerclient.PayInvoice(ctx, buyer, topupExtra.Invoice)
	if err != nil {
		fail("pay topup: %v", err)
	}
	topupSig, _ := x402.EncodeHeader(x402.PaymentPayload{
		X402Version: 2, Accepted: lnEntry,
		Payload: x402.MustRaw(x402.LightningPayload{Preimage: topupPre, PaymentHash: topupExtra.PaymentHash}),
	})
	req, _ = http.NewRequest("GET", gw+"/_dcr402/topup", nil)
	req.Header.Set(x402.HeaderPaymentSignature, topupSig)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		fail("topup settle: %v", err)
	}
	topupBody := getJSON(resp)
	if resp.StatusCode != 200 || topupBody["status"] != "credited" ||
		topupBody["balanceAtoms"].(float64) != 500_000 ||
		topupBody["rail"] != x402.MethodLightning {
		fail("topup outcome: %v", topupBody)
	}
	var settle x402.SettlementResponse
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentResponse), &settle); err != nil {
		fail("topup PAYMENT-RESPONSE: %v", err)
	}
	var cred struct {
		Authorization string `json:"authorization"`
	}
	if err := json.Unmarshal(settle.Extensions[dcr402.ExtensionL402].Info, &cred); err != nil {
		fail("credential: %v", err)
	}
	step("credited over Lightning; account credential received")

	// 5. MCP credits (route /mcp): burn the balance per tool call.
	step("MCP credits: burning on /mcp")
	resp, out = postMCP(gw+"/mcp", toolCall(3, "hello_tool", nil), nil)
	result, _ = out["result"].(map[string]any)
	if result == nil || result["isError"] != true ||
		!strings.Contains(fmt.Sprint(result["structuredContent"]), "payment_required") {
		fail("uncredentialed tools/call: %v", out)
	}
	auth := map[string]string{"Authorization": cred.Authorization}
	for i, wantBal := range []string{"400000", "300000"} {
		resp, out = postMCP(gw+"/mcp", toolCall(4+i, "hello_tool", nil), auth)
		result, _ = out["result"].(map[string]any)
		if resp.StatusCode != 200 || result == nil || result["isError"] != false {
			fail("credit call %d: %v", i, out)
		}
		if got := resp.Header.Get("Dcr402-Balance"); got != wantBal {
			fail("credit call %d: balance %q want %s", i, got, wantBal)
		}
	}
	// Free tool needs no credential.
	resp, out = postMCP(gw+"/mcp", toolCall(6, "about", nil), nil)
	result, _ = out["result"].(map[string]any)
	if resp.StatusCode != 200 || result == nil || result["isError"] != false {
		fail("free tool: %v", out)
	}

	// 6. Balance endpoint agrees.
	req, _ = http.NewRequest("GET", gw+"/_dcr402/balance", nil)
	req.Header.Set("Authorization", cred.Authorization)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		fail("balance: %v", err)
	}
	bal := getJSON(resp)
	if bal["balanceAtoms"].(float64) != 300_000 {
		fail("balance endpoint: %v", bal)
	}

	fmt.Printf("\x1b[1;32mPASS\x1b[0m dcr402d live smoke on simnet: HTTP per-call, " +
		"MCP x402 transport, Lightning top-up, MCP credit burns\n")
}
