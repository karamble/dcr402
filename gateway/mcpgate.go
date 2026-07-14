package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/ledger"
	"github.com/karamble/dcr402/lib/x402"
)

// rpcMessage is the minimal JSON-RPC 2.0 view the gateway needs.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// toolCallParams is the tools/call params view: the tool name and _meta
// (where the x402 MCP transport carries the payment).
type toolCallParams struct {
	Name string                     `json:"name"`
	Meta map[string]json.RawMessage `json:"_meta"`
}

// duplicateKey reports whether a JSON object carries the given top-level key
// more than once. json.Unmarshal silently keeps the last occurrence, but a
// lenient upstream might keep the first, so a duplicate is a gating hazard.
func duplicateKey(obj json.RawMessage, key string) bool {
	dec := json.NewDecoder(bytes.NewReader(obj))
	t, err := dec.Token()
	if err != nil {
		return false
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return false
	}
	count := 0
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return false
		}
		if k, ok := keyTok.(string); ok && k == key {
			count++
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return false
		}
	}
	return count > 1
}

// mcpHandler gates a Streamable-HTTP MCP upstream: everything except
// tools/call — initialize, notifications, tools/list, resource reads, the
// GET/SSE listen channel, session teardown — is forwarded untouched. A
// tools/call is priced by tool name and answered per the official x402 MCP
// transport: unpaid calls receive the payment-required tool result, paid
// calls are settled and forwarded with the settlement injected into
// result._meta["x402/payment-response"].
func (gw *Gateway) mcpHandler(u *Upstream, proxy http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			proxy.ServeHTTP(w, r)
			return
		}
		const maxBody = 8 << 20
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		r.Body.Close()
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		if len(body) > maxBody {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		restore := func() {
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}

		trimmed := bytes.TrimLeft(body, " \t\r\n")
		if len(trimmed) > 0 && trimmed[0] == '[' {
			// JSON-RPC batching was removed from MCP; a batch that hides a
			// tools/call cannot be gated per call. Parse each element's method
			// (a substring match would miss a JSON-escaped "tools\/call") and
			// refuse any batch carrying a tools/call.
			var batch []rpcMessage
			if err := json.Unmarshal(body, &batch); err != nil {
				http.Error(w, "malformed JSON-RPC batch", http.StatusBadRequest)
				return
			}
			for _, m := range batch {
				if m.Method == "tools/call" {
					http.Error(w, "JSON-RPC batches containing tools/call are not supported by this gateway",
						http.StatusBadRequest)
					return
				}
			}
			restore()
			proxy.ServeHTTP(w, r)
			return
		}

		var msg rpcMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			// Fail closed: an unparseable message is not forwarded free, since
			// a lenient upstream might still execute a paid tool from it.
			http.Error(w, "malformed JSON-RPC message", http.StatusBadRequest)
			return
		}
		if msg.Method != "tools/call" {
			restore()
			proxy.ServeHTTP(w, r) // not ours to gate; upstream handles it
			return
		}
		var params toolCallParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			http.Error(w, "malformed tools/call params", http.StatusBadRequest)
			return
		}
		// A duplicate `name` key lets the gateway price one tool (last wins)
		// while a first-wins upstream executes another; refuse the ambiguity.
		if duplicateKey(msg.Params, "name") {
			http.Error(w, "ambiguous tools/call name", http.StatusBadRequest)
			return
		}

		price := u.ToolPriceFor(params.Name)
		if price <= 0 {
			restore()
			proxy.ServeHTTP(w, r)
			return
		}

		if u.Mode == ModeCredits {
			// No idempotency key: the JSON-RPC msg.ID is only unique within a
			// session (clients restart at 1,2,3...), so using it across an
			// account's lifetime would wrongly dedupe distinct calls and
			// under-charge. MCP credits idempotency awaits an explicit
			// client-supplied key; the HTTP path uses the Idempotency-Key header.
			balance, err := gw.gate.ChargeCredential(r.Context(),
				r.Header.Get("Authorization"), "mcp:"+params.Name, price, "")
			if err != nil {
				gw.writeMCPCreditError(w, msg.ID, err)
				return
			}
			w.Header().Set("Dcr402-Balance", strconv.FormatInt(balance, 10))
			restore()
			proxy.ServeHTTP(w, r)
			return
		}

		// Per-call mode: the x402 MCP transport.
		pp, present, err := dcr402.DecodeMetaPayment(params.Meta)
		if err != nil {
			gw.writeMCPToolChallenge(w, r, msg.ID, params.Name, price,
				"malformed x402/payment: "+err.Error())
			return
		}
		if !present {
			gw.writeMCPToolChallenge(w, r, msg.ID, params.Name, price, "")
			return
		}
		settle, already, vErr := gw.gate.MCPSettle(r.Context(), pp, price)
		if vErr != nil {
			gw.writeMCPToolChallenge(w, r, msg.ID, params.Name, price,
				"payment verification failed: "+vErr.Reason)
			return
		}
		if already {
			// Rule 8: the paid operation MUST NOT be re-executed — answer
			// with the stored settlement instead of forwarding.
			gw.writeMCPResult(w, msg.ID, map[string]any{
				"isError": false,
				"content": []x402ToolContent{{Type: "text",
					Text: `{"status":"already_settled","hint":"use the credential from extensions.l402"}`}},
				"_meta": map[string]any{dcr402.MetaPaymentResponse: settle},
			})
			return
		}
		restore()
		gw.forwardSettled(w, r, proxy, settle)
	})
}

type x402ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// maxToolResponse bounds the buffered JSON tool response so a huge upstream
// body cannot exhaust gateway memory.
const maxToolResponse = 8 << 20

// forwardSettled proxies the paid tools/call and returns the settlement to the
// caller: always as the Payment-Response header, and injected into
// result._meta["x402/payment-response"] for a buffered JSON response. A
// text/event-stream response is streamed through unbuffered, with the
// settlement carried on the header. Upstream response headers are captured
// separately and re-emitted through an allow-list, and the gateway sets the
// Payment-Response header itself.
func (gw *Gateway) forwardSettled(w http.ResponseWriter, r *http.Request, proxy http.Handler, settle x402.SettlementResponse) {
	iw := &injectingWriter{dst: w, settle: settle}
	proxy.ServeHTTP(iw, r)
	if iw.stream {
		return // streamed live; nothing to inject
	}

	copyResponseHeaders(w.Header(), iw.Header())
	setSettlementHeader(w.Header(), settle)
	body := iw.buf.Bytes()
	if !iw.truncated && strings.HasPrefix(iw.Header().Get("Content-Type"), "application/json") {
		body = injectSettlement(body, settle)
	}
	w.Header().Del("Content-Length") // injection changed the length
	code := iw.code
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// copyResponseHeaders forwards upstream headers, dropping the ones the gateway
// owns or recomputes (Content-Length, Set-Cookie, Payment-Response).
func copyResponseHeaders(dst, src http.Header) {
	for k, vals := range src {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length", "Set-Cookie", x402.HeaderPaymentResponse:
			continue // length is recomputed; cookies dropped; settlement is ours
		}
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

func setSettlementHeader(h http.Header, settle x402.SettlementResponse) {
	if header, err := x402.EncodeHeader(settle); err == nil {
		h.Set(x402.HeaderPaymentResponse, header)
	}
}

// injectSettlement puts the settlement into result._meta of a JSON-RPC
// response, returning body unchanged if it is not the expected shape.
func injectSettlement(body []byte, settle x402.SettlementResponse) []byte {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return body
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return body
	}
	meta, _ := result["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[dcr402.MetaPaymentResponse] = settle
	result["_meta"] = meta
	resp["result"] = result
	if injected, err := json.Marshal(resp); err == nil {
		return injected
	}
	return body
}

// injectingWriter buffers a JSON tool response (capped) so the settlement can
// be injected, but streams a text/event-stream response verbatim. It captures
// the upstream's headers in its own map so forwardSettled can allow-list them.
type injectingWriter struct {
	dst        http.ResponseWriter
	settle     x402.SettlementResponse
	hdr        http.Header
	hdrWritten bool
	stream     bool
	code       int
	buf        bytes.Buffer
	truncated  bool
}

func (iw *injectingWriter) Header() http.Header {
	if iw.hdr == nil {
		iw.hdr = http.Header{}
	}
	return iw.hdr
}

func (iw *injectingWriter) WriteHeader(code int) {
	if iw.hdrWritten {
		return
	}
	iw.hdrWritten = true
	iw.code = code
	if strings.HasPrefix(iw.Header().Get("Content-Type"), "text/event-stream") {
		iw.stream = true
		copyResponseHeaders(iw.dst.Header(), iw.hdr)
		setSettlementHeader(iw.dst.Header(), iw.settle)
		iw.dst.WriteHeader(code)
	}
}

func (iw *injectingWriter) Write(p []byte) (int, error) {
	if !iw.hdrWritten {
		iw.WriteHeader(http.StatusOK)
	}
	if iw.stream {
		return iw.dst.Write(p)
	}
	if room := maxToolResponse - iw.buf.Len(); room <= 0 {
		iw.truncated = true
		return len(p), nil
	} else if len(p) > room {
		iw.truncated = true
		iw.buf.Write(p[:room])
		return len(p), nil
	}
	return iw.buf.Write(p)
}

// Flush only forwards while streaming; buffering must not flush the deferred
// JSON response early.
func (iw *injectingWriter) Flush() {
	if iw.stream {
		if f, ok := iw.dst.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// writeMCPToolChallenge answers a tools/call with the payment-required tool
// result (a fresh challenge). note, when set, is appended as an extra
// content item — content[0] stays the transport-required serialization of
// structuredContent.
func (gw *Gateway) writeMCPToolChallenge(w http.ResponseWriter, r *http.Request, id json.RawMessage, tool string, price int64, note string) {
	result, err := gw.gate.MCPChallenge(r.Context(), tool, "", price)
	if err != nil {
		http.Error(w, "building payment challenge", http.StatusInternalServerError)
		return
	}
	out := map[string]any{
		"isError":           true,
		"structuredContent": result.StructuredContent,
		"content":           result.Content,
	}
	if note != "" {
		out["content"] = append(result.Content, dcr402.ToolContent{Type: "text", Text: note})
	}
	gw.writeMCPResult(w, id, out)
}

// writeMCPCreditError renders credit failures as machine-actionable tool
// errors (JSON-RPC result with isError, per MCP conventions).
func (gw *Gateway) writeMCPCreditError(w http.ResponseWriter, id json.RawMessage, chargeErr error) {
	var structured map[string]any
	var insufficient *ledger.InsufficientBalanceError
	switch {
	case errors.As(chargeErr, &insufficient):
		structured = map[string]any{
			"error":          "insufficient_credits",
			"balanceAtoms":   insufficient.Balance,
			"requiredAtoms":  insufficient.Required,
			"shortfallAtoms": insufficient.Shortfall(),
			"topup":          TopupPath,
		}
	case errors.Is(chargeErr, dcr402.ErrNoCredential):
		structured = map[string]any{
			"error":  "payment_required",
			"detail": "credit account required — top up to obtain a credential",
			"topup":  TopupPath,
		}
	case errors.Is(chargeErr, dcr402.ErrInvalidCredential):
		structured = map[string]any{
			"error": "invalid_credential",
			"topup": TopupPath,
		}
	default:
		http.Error(w, "ledger charge failed", http.StatusInternalServerError)
		return
	}
	text, _ := json.Marshal(structured)
	gw.writeMCPResult(w, id, map[string]any{
		"isError":           true,
		"structuredContent": structured,
		"content":           []x402ToolContent{{Type: "text", Text: string(text)}},
	})
}

// writeMCPResult writes a JSON-RPC 2.0 response envelope.
func (gw *Gateway) writeMCPResult(w http.ResponseWriter, id json.RawMessage, result any) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}
