// gate-mcp-server shows how to make one MCP tool payable using the official
// modelcontextprotocol/go-sdk and the dcr402 MCP helpers. An unpaid
// tools/call returns the x402 payment-required tool result; a call carrying
// the payment in params._meta["x402/payment"] is settled and served, with
// the settlement echoed in result._meta["x402/payment-response"]. See
// README.md.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	dcr402 "github.com/karamble/dcr402/lib"
	dcr402lnd "github.com/karamble/dcr402/lib/dcrlnd"
	"github.com/karamble/dcr402/lib/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func network(name string) dcr402.Network {
	switch name {
	case "mainnet":
		return dcr402.Mainnet
	case "testnet3":
		return dcr402.Testnet3
	default:
		return dcr402.Simnet
	}
}

// metaToRaw adapts the go-sdk request _meta (map[string]any) to the
// map[string]json.RawMessage that dcr402.DecodeMetaPayment expects.
func metaToRaw(m map[string]any) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		if b, err := json.Marshal(v); err == nil {
			out[k] = b
		}
	}
	return out
}

func main() {
	net := network(env("NETWORK", "simnet"))
	backend, err := dcr402lnd.New(dcr402lnd.Config{
		Host:         env("DCRLND_RPC", "127.0.0.1:10009"),
		TLSCertPath:  os.Getenv("DCRLND_TLS_CERT"),
		MacaroonPath: os.Getenv("DCRLND_MACAROON"),
		ChainParams:  net.Params,
	})
	if err != nil {
		log.Fatalf("dcrlnd: %v", err)
	}
	defer backend.Close()

	st, err := store.OpenSQLite(env("DB", "gate-mcp-server.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	gate, err := dcr402.New(dcr402.Config{
		Backend: backend, Store: st, Network: net,
		PayTo: os.Getenv("PAYTO"), Service: "gate-mcp-server",
	})
	if err != nil {
		log.Fatalf("gate: %v", err)
	}
	price, _ := strconv.ParseInt(env("PRICE_ATOMS", "250000"), 10, 64)

	srv := mcp.NewServer(&mcp.Implementation{Name: "gate-mcp-server", Version: "0.1.0"}, nil)

	mcp.AddTool(srv, &mcp.Tool{Name: "process", Description: "A payable tool (0.0025 DCR)."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			pp, present, err := dcr402.DecodeMetaPayment(metaToRaw(req.Params.Meta))
			if !present || err != nil {
				// Unpaid: return the payment-required challenge as an
				// isError tool result carrying the PaymentRequired object.
				tr, cerr := gate.MCPChallenge(ctx, "process", "A payable tool", price)
				if cerr != nil {
					return nil, nil, cerr
				}
				var structured any
				_ = json.Unmarshal(tr.StructuredContent, &structured)
				return &mcp.CallToolResult{
					IsError:           true,
					StructuredContent: structured,
					Content:           []mcp.Content{&mcp.TextContent{Text: tr.Content[0].Text}},
				}, nil, nil
			}
			settle, already, vErr := gate.MCPSettle(ctx, pp, price)
			if vErr != nil {
				return &mcp.CallToolResult{IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "payment failed: " + vErr.Reason}}}, nil, nil
			}
			text := "processed your request"
			if already {
				text = "already settled; reuse the credential from extensions.l402"
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: text}},
				Meta:    mcp.Meta{dcr402.MetaPaymentResponse: settle},
			}, nil, nil
		})

	addr := env("LISTEN", "127.0.0.1:8081")
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	log.Printf("MCP server on http://%s/ (payable tool: process, %d atoms)", addr, price)
	log.Fatal(http.ListenAndServe(addr, handler))
}
