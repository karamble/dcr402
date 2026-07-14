// Command upstream is the toy origin server behind dcr402d in the gateway
// smoke test: a paid HTTP endpoint plus a minimal Streamable-HTTP MCP
// responder (initialize, tools/list, tools/call). Everything it serves is
// unprotected — the gateway in front provides the payment gating.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func main() {
	listen := flag.String("listen", "127.0.0.1:20990", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/hello", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "HELLO PAID WORLD")
	})
	mux.HandleFunc("/paid", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "HELLO PAID WORLD")
	})
	// Everything else is treated as an MCP endpoint (the gateway forwards
	// /mcp and /mcp2 here).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST JSON-RPC only", http.StatusMethodNotAllowed)
			return
		}
		var msg rpcMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "bad JSON-RPC", http.StatusBadRequest)
			return
		}
		if len(msg.ID) == 0 { // notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "toy-upstream", "version": "0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{
				{"name": "hello_tool", "description": "says hello"},
				{"name": "about", "description": "free info"},
			}}
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			result = map[string]any{
				"isError": false,
				"content": []map[string]string{{"type": "text",
					"text": "executed " + params.Name}},
			}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": msg.ID, "result": result,
		})
	})

	log.Printf("toy upstream listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
