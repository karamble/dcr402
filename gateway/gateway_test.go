package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/ledger"
	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

// Golden vector constants (scheme/test-vectors/).
const (
	goldenInvoice = "lndcr2500u1pvjluezpp5qt2yngclhvn8ere496vk3fu78e0ujhqmh649qt7kg48tmedyhmwqdpqv33hydpsxgsxwmmvv3jkugrkv43hgmmjxqrrssdztqxz9z9ys3q59ml9270ej0t9wt62442s3tzzldns0a6j247qkkqzakysm9yz75xqze4a7r3h7tsys8tcugay7f8sru2l8a7s07srcqc5t8f7"
	goldenHash    = "02d449a31fbb267c8f352e9968a79e3e5fc95c1bbeaa502fd6454ebde5a4bedc"
	goldenPre     = "1111111111111111111111111111111111111111111111111111111111111111"
	goldenDest    = "03e7156ae33b0a208d0744199163177e909e80176e55d97a2f221ede0f934dd9ad"
	goldenAtoms   = 250000
)

type fakeLN struct{ settled bool }

func (f *fakeLN) CreateInvoice(context.Context, int64, string, time.Duration) (string, [32]byte, error) {
	var hash [32]byte
	raw, _ := hex.DecodeString(goldenHash)
	copy(hash[:], raw)
	return goldenInvoice, hash, nil
}

func (f *fakeLN) LookupInvoice(context.Context, [32]byte) (dcr402.InvoiceStatus, error) {
	return dcr402.InvoiceStatus{Settled: f.settled, AmtPaidMAtoms: goldenAtoms * 1000}, nil
}

type fakeOnchain struct {
	mu       sync.Mutex
	nextAddr int
	deposits map[string]fakeDeposit
}

type fakeDeposit struct {
	address string
	confs   int32
	amount  int64
}

func (f *fakeOnchain) NewDepositAddress(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextAddr++
	return fmt.Sprintf("SsFakeDeposit%06d", f.nextAddr), nil
}

func (f *fakeOnchain) LookupDeposit(_ context.Context, txid, address string) (dcr402.DepositStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dep, ok := f.deposits[txid]
	if !ok {
		return dcr402.DepositStatus{}, nil
	}
	st := dcr402.DepositStatus{Found: true, Confirmations: dep.confs}
	if dep.address == address {
		st.AmountToAddressAtoms = dep.amount
	}
	return st, nil
}

const baseYAML = `
listen: ":0"
network: mainnet
service: gwtest
payto: %q
store: unused-in-tests
ln: {host: h, tls_cert: c, macaroon: m}
`

func testConfig(t *testing.T, upstreamYAML string, credits bool) *Config {
	t.Helper()
	yaml := fmt.Sprintf(baseYAML, goldenDest)
	if credits {
		yaml += "credits: {enabled: true, onchain: true, confirmations: 2}\n"
	}
	yaml += upstreamYAML
	cfg, err := parseConfig([]byte(yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

type rig struct {
	gw      *Gateway
	srv     *httptest.Server
	ln      *fakeLN
	onchain *fakeOnchain
	led     *ledger.Memory
}

func newRig(t *testing.T, cfg *Config) *rig {
	t.Helper()
	r := &rig{ln: &fakeLN{settled: true}, onchain: &fakeOnchain{deposits: map[string]fakeDeposit{}}, led: ledger.NewMemory()}
	deps := Deps{Backend: r.ln, Store: store.NewMemory()}
	if cfg.Credits.Enabled {
		deps.Ledger = r.led
		deps.Onchain = r.onchain
	}
	gw, err := Build(cfg, deps)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	r.gw = gw
	r.srv = httptest.NewServer(gw.Handler())
	t.Cleanup(r.srv.Close)
	return r
}

// lightningProof builds a PAYMENT-SIGNATURE for the golden invoice from a
// PaymentRequired header.
func lightningProof(t *testing.T, prHeader string) string {
	t.Helper()
	var pr x402.PaymentRequired
	if err := x402.DecodeHeader(prHeader, &pr); err != nil {
		t.Fatalf("PAYMENT-REQUIRED: %v", err)
	}
	sig, err := x402.EncodeHeader(x402.PaymentPayload{
		X402Version: 2,
		Accepted:    pr.Accepts[0],
		Payload: x402.MustRaw(x402.LightningPayload{
			Preimage:    goldenPre,
			PaymentHash: goldenHash,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

// topupCredential runs an on-chain top-up through the gateway and returns
// the account credential.
func (r *rig) topupCredential(t *testing.T, atoms int64, txid string) string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s%s?atoms=%d", r.srv.URL, TopupPath, atoms))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	var pr x402.PaymentRequired
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &pr); err != nil {
		t.Fatal(err)
	}
	var entry x402.PaymentRequirements
	for _, e := range pr.Accepts {
		if e.TransferMethod() == x402.MethodOnchain {
			entry = e
		}
	}
	if entry.PayTo == "" {
		t.Fatal("no onchain entry in topup challenge")
	}
	r.onchain.mu.Lock()
	r.onchain.deposits[txid] = fakeDeposit{address: entry.PayTo, confs: 2, amount: atoms}
	r.onchain.mu.Unlock()

	sig, err := x402.EncodeHeader(x402.PaymentPayload{
		X402Version: 2,
		Accepted:    entry,
		Payload:     x402.MustRaw(x402.OnchainPayload{TxID: txid}),
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", r.srv.URL+TopupPath, nil)
	req.Header.Set(x402.HeaderPaymentSignature, sig)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("topup settle: status %d", resp.StatusCode)
	}
	var settle x402.SettlementResponse
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentResponse), &settle); err != nil {
		t.Fatal(err)
	}
	var cred struct {
		Authorization string `json:"authorization"`
	}
	if err := json.Unmarshal(settle.Extensions[dcr402.ExtensionL402].Info, &cred); err != nil {
		t.Fatal(err)
	}
	return cred.Authorization
}

func TestBazaarExtensionEmitted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	cfg := testConfig(t, fmt.Sprintf(`
upstreams:
  - route: /api/
    target: %q
    prices: {"GET /api/paid": "0.0025"}
    discovery:
      service_name: "Test API"
      tags: [test, demo]
      icon_url: "https://example.com/i.png"
`, upstream.URL), false)
	r := newRig(t, cfg)

	resp, _ := http.Get(r.srv.URL + "/api/paid")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var pr x402.PaymentRequired
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentRequired), &pr); err != nil {
		t.Fatal(err)
	}
	ext, ok := pr.Extensions[dcr402.ExtensionBazaar]
	if !ok {
		t.Fatalf("no bazaar extension; got keys %v", pr.Extensions)
	}
	var info struct {
		Input struct {
			Type   string `json:"type"`
			Method string `json:"method"`
		} `json:"input"`
	}
	if err := json.Unmarshal(ext.Info, &info); err != nil {
		t.Fatal(err)
	}
	if info.Input.Type != "http" || info.Input.Method != "GET" {
		t.Fatalf("bazaar info wrong: %+v", info.Input)
	}
	if pr.Resource.ServiceName != "Test API" || len(pr.Resource.Tags) != 2 {
		t.Fatalf("service metadata not on resource: %+v", pr.Resource)
	}
}

func TestParseDCR(t *testing.T) {
	good := map[string]int64{
		"0.001":      100_000,
		"1":          100_000_000,
		"0.00000001": 1,
		"10.5":       1_050_000_000,
		"2500.25":    250_025_000_000,
	}
	for in, want := range good {
		got, err := ParseDCR(in)
		if err != nil || got != want {
			t.Fatalf("ParseDCR(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "0.000000001", "-1", "1.2.3", "abc", "0.0"} {
		if _, err := ParseDCR(bad); err == nil {
			t.Fatalf("ParseDCR(%q) accepted", bad)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	// A full valid config parses with resolved prices.
	cfg := testConfig(t, `
upstreams:
  - route: /api/
    target: http://127.0.0.1:9
    prices: {"get /api/x": "0.001"}
    price_default: "0.002"
`, false)
	ups, err := cfg.ResolveUpstreams()
	if err != nil {
		t.Fatal(err)
	}
	if ups[0].PriceFor("GET", "/api/x") != 100_000 {
		t.Fatal("method casing not normalized")
	}
	if ups[0].PriceFor("POST", "/api/other") != 200_000 {
		t.Fatal("default price not applied")
	}

	for name, yaml := range map[string]string{
		"credits without enable": `
upstreams: [{route: /a/, target: "http://h", mode: credits}]`,
		"reserved route": `
upstreams: [{route: /_dcr402/x, target: "http://h"}]`,
		"bad mode": `
upstreams: [{route: /a/, target: "http://h", mode: nonsense}]`,
		"unknown field": `
upstreams: [{route: /a/, target: "http://h"}]
nonsense: true`,
		"non-http target": `
upstreams: [{route: /a/, target: "file:///etc/passwd"}]`,
	} {
		yamlFull := fmt.Sprintf(baseYAML, goldenDest) + yaml
		if _, err := parseConfig([]byte(yamlFull)); err == nil {
			t.Fatalf("%s: config accepted", name)
		}
	}
}

// TestMCPBatchGuardRejectsEscaped checks the batch guard parses methods
// rather than substring-matching, so a JSON-escaped tools\/call in a batch is
// rejected (400) instead of being forwarded free to a lenient upstream.
func TestMCPBatchGuardRejectsEscaped(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached for a rejected batch")
	}))
	defer upstream.Close()
	cfg := testConfig(t, fmt.Sprintf(`
upstreams:
  - route: /mcp
    target: %q
    mcp: true
    mode: percall
    tool_price_default: "0.001"
`, upstream.URL), false)
	r := newRig(t, cfg)

	batch := `[{"jsonrpc":"2.0","id":1,"method":"tools\/call","params":{"name":"process"}}]`
	resp, err := http.Post(r.srv.URL+"/mcp", "application/json", strings.NewReader(batch))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("escaped tools/call batch: status %d, want 400", resp.StatusCode)
	}
}

func TestHTTPPerCall(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		fmt.Fprintf(w, "upstream:%s %s", r.Method, r.URL.Path)
	}))
	defer upstream.Close()

	cfg := testConfig(t, fmt.Sprintf(`
upstreams:
  - route: /api/
    target: %q
    prices: {"GET /api/paid": "0.0025"}
`, upstream.URL), false)
	r := newRig(t, cfg)

	// Free path forwards untouched.
	resp, _ := http.Get(r.srv.URL + "/api/free")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "upstream:GET /api/free" {
		t.Fatalf("free path: %d %q", resp.StatusCode, body)
	}

	// Priced path challenges, then serves against the golden proof.
	resp, _ = http.Get(r.srv.URL + "/api/paid")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("priced path: status %d", resp.StatusCode)
	}
	if len(resp.Header.Values("Www-Authenticate")) != 2 {
		t.Fatal("missing L402 challenges")
	}
	sig := lightningProof(t, resp.Header.Get(x402.HeaderPaymentRequired))
	req, _ := http.NewRequest("GET", r.srv.URL+"/api/paid", nil)
	req.Header.Set(x402.HeaderPaymentSignature, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "upstream:GET /api/paid" {
		t.Fatalf("paid path: %d %q", resp.StatusCode, body)
	}
	var settle x402.SettlementResponse
	if err := x402.DecodeHeader(resp.Header.Get(x402.HeaderPaymentResponse), &settle); err != nil || !settle.Success {
		t.Fatalf("settlement: %+v err=%v", settle, err)
	}
}

func TestHTTPCredits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "upstream-ok")
	}))
	defer upstream.Close()

	cfg := testConfig(t, fmt.Sprintf(`
upstreams:
  - route: /api/
    target: %q
    mode: credits
    prices: {"GET /api/paid": "0.002"}
`, upstream.URL), true)
	r := newRig(t, cfg)

	cred := r.topupCredential(t, 500_000, strings.Repeat("aa", 32))

	call := func(auth string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", r.srv.URL+"/api/paid", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	resp, body := call(cred)
	if resp.StatusCode != 200 || body != "upstream-ok" ||
		resp.Header.Get("Dcr402-Balance") != "300000" {
		t.Fatalf("credit call: %d %q balance=%s", resp.StatusCode, body, resp.Header.Get("Dcr402-Balance"))
	}
	resp, _ = call(cred) // 100000 left
	if resp.Header.Get("Dcr402-Balance") != "100000" {
		t.Fatal("second charge wrong")
	}
	resp, body = call(cred) // insufficient
	if resp.StatusCode != http.StatusPaymentRequired || !strings.Contains(body, "insufficient_credits") {
		t.Fatalf("insufficient: %d %q", resp.StatusCode, body)
	}
	resp, _ = call("")
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("no credential: %d", resp.StatusCode)
	}

	// Balance endpoint.
	req, _ := http.NewRequest("GET", r.srv.URL+"/_dcr402/balance", nil)
	req.Header.Set("Authorization", cred)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var bal map[string]any
	json.NewDecoder(resp2.Body).Decode(&bal)
	resp2.Body.Close()
	if bal["balanceAtoms"].(float64) != 100_000 {
		t.Fatalf("balance endpoint: %v", bal)
	}
}

// fake MCP upstream: JSON-RPC echo.
func newMCPUpstream(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var msg rpcMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			http.Error(w, "bad json-rpc", 400)
			return
		}
		var params toolCallParams
		json.Unmarshal(msg.Params, &params)
		writeJSON(w, 200, map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"result": map[string]any{
				"isError": false,
				"content": []map[string]string{{"type": "text",
					"text": "executed " + msg.Method + " " + params.Name}},
			},
		})
	}))
}

func toolCallBody(id int, tool string, meta map[string]any) []byte {
	params := map[string]any{"name": tool, "arguments": map[string]any{}}
	if meta != nil {
		params["_meta"] = meta
	}
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params,
	})
	return raw
}

func postMCP(t *testing.T, url string, body []byte, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		json.Unmarshal(raw, &out)
		if out == nil {
			out = map[string]any{"_raw": string(raw)}
		}
	}
	return resp, out
}

func TestMCPPerCall(t *testing.T) {
	var hits atomic.Int64
	upstream := newMCPUpstream(t, &hits)
	defer upstream.Close()

	cfg := testConfig(t, fmt.Sprintf(`
upstreams:
  - route: /mcp
    target: %q
    mcp: true
    tool_prices: {process: "0.0025"}
    free_tools: [about]
`, upstream.URL), false)
	r := newRig(t, cfg)
	mcpURL := r.srv.URL + "/mcp"

	// initialize forwarded free.
	initBody, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	resp, out := postMCP(t, mcpURL, initBody, nil)
	if resp.StatusCode != 200 || out["result"] == nil {
		t.Fatalf("initialize: %d %v", resp.StatusCode, out)
	}

	// Free tool forwarded.
	resp, out = postMCP(t, mcpURL, toolCallBody(2, "about", nil), nil)
	result := out["result"].(map[string]any)
	if resp.StatusCode != 200 || result["isError"] != false {
		t.Fatalf("free tool: %v", out)
	}

	// Priced tool, unpaid: payment-required tool result, upstream untouched.
	before := hits.Load()
	resp, out = postMCP(t, mcpURL, toolCallBody(3, "process", nil), nil)
	result = out["result"].(map[string]any)
	if resp.StatusCode != 200 || result["isError"] != true {
		t.Fatalf("unpaid tool: %v", out)
	}
	sc, _ := json.Marshal(result["structuredContent"])
	var pr x402.PaymentRequired
	if err := json.Unmarshal(sc, &pr); err != nil || pr.X402Version != 2 ||
		pr.Resource.URL != "mcp://tool/process" || pr.Accepts[0].Amount != "250000" {
		t.Fatalf("challenge wrong: %s", sc)
	}
	if hits.Load() != before {
		t.Fatal("unpaid tools/call reached the upstream")
	}

	// Paid: settled, forwarded, settlement injected into result._meta.
	pp := x402.PaymentPayload{
		X402Version: 2,
		Accepted:    pr.Accepts[0],
		Payload:     x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
	}
	resp, out = postMCP(t, mcpURL, toolCallBody(4, "process",
		map[string]any{dcr402.MetaPayment: pp}), nil)
	result = out["result"].(map[string]any)
	if resp.StatusCode != 200 || result["isError"] != false {
		t.Fatalf("paid tool: %v", out)
	}
	meta, _ := result["_meta"].(map[string]any)
	settleRaw, _ := json.Marshal(meta[dcr402.MetaPaymentResponse])
	var settle x402.SettlementResponse
	if err := json.Unmarshal(settleRaw, &settle); err != nil || !settle.Success ||
		settle.Transaction != goldenHash {
		t.Fatalf("injected settlement wrong: %s", settleRaw)
	}
	if resp.Header.Get(x402.HeaderPaymentResponse) == "" {
		t.Fatal("missing Payment-Response header")
	}

	// Replay: MUST NOT re-execute — upstream not hit again.
	before = hits.Load()
	resp, out = postMCP(t, mcpURL, toolCallBody(5, "process",
		map[string]any{dcr402.MetaPayment: pp}), nil)
	result = out["result"].(map[string]any)
	if hits.Load() != before {
		t.Fatal("replay reached the upstream")
	}
	if !strings.Contains(fmt.Sprint(result["content"]), "already_settled") {
		t.Fatalf("replay result: %v", result)
	}

	// Batch containing tools/call is refused.
	batch := []byte(`[{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"process"}}]`)
	resp, _ = postMCP(t, mcpURL, batch, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("batch: status %d", resp.StatusCode)
	}
}

func TestMCPCredits(t *testing.T) {
	var hits atomic.Int64
	upstream := newMCPUpstream(t, &hits)
	defer upstream.Close()

	cfg := testConfig(t, fmt.Sprintf(`
upstreams:
  - route: /mcp
    target: %q
    mcp: true
    mode: credits
    tool_price_default: "0.002"
`, upstream.URL), true)
	r := newRig(t, cfg)
	mcpURL := r.srv.URL + "/mcp"

	// No credential → machine-actionable tool error.
	resp, out := postMCP(t, mcpURL, toolCallBody(1, "anything", nil), nil)
	result := out["result"].(map[string]any)
	if resp.StatusCode != 200 || result["isError"] != true ||
		!strings.Contains(fmt.Sprint(result["structuredContent"]), "payment_required") {
		t.Fatalf("no credential: %v", out)
	}

	cred := r.topupCredential(t, 500_000, strings.Repeat("bb", 32))

	// Charged and forwarded.
	resp, out = postMCP(t, mcpURL, toolCallBody(2, "anything", nil),
		map[string]string{"Authorization": cred})
	result = out["result"].(map[string]any)
	if resp.StatusCode != 200 || result["isError"] != false {
		t.Fatalf("credit tool call: %v", out)
	}
	if resp.Header.Get("Dcr402-Balance") != "300000" {
		t.Fatalf("balance header %q", resp.Header.Get("Dcr402-Balance"))
	}

	// Burn down to insufficient.
	postMCP(t, mcpURL, toolCallBody(3, "anything", nil), map[string]string{"Authorization": cred})
	resp, out = postMCP(t, mcpURL, toolCallBody(4, "anything", nil), map[string]string{"Authorization": cred})
	result = out["result"].(map[string]any)
	if result["isError"] != true ||
		!strings.Contains(fmt.Sprint(result["structuredContent"]), "insufficient_credits") {
		t.Fatalf("insufficient: %v", out)
	}
	sc := result["structuredContent"].(map[string]any)
	if sc["shortfallAtoms"].(float64) != 100_000 {
		t.Fatalf("shortfall: %v", sc)
	}
}

func TestStatusEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	cfg := testConfig(t, fmt.Sprintf(`
web: {status_page: true}
upstreams:
  - route: /api/
    target: %q
    prices: {"GET /api/x": "0.001"}
`, upstream.URL), true)
	r := newRig(t, cfg)

	resp, err := http.Get(r.srv.URL + "/_dcr402/info")
	if err != nil {
		t.Fatal(err)
	}
	var info map[string]any
	json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info["service"] != "gwtest" || info["creditsEnabled"] != true {
		t.Fatalf("info: %v", info)
	}

	resp, err = http.Get(r.srv.URL + "/_dcr402/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "gwtest") {
		t.Fatalf("status page: %d", resp.StatusCode)
	}
}
