# Example: gate an MCP tool

Make one MCP tool payable, using the official modelcontextprotocol/go-sdk and
the dcr402 MCP helpers. The tool follows the x402 MCP transport, so any
x402-aware MCP client can pay it.

## What it shows

- Building an MCP server with the go-sdk and registering a payable tool.
- The x402 MCP transport, both directions:
  - Unpaid: the tool returns a result with `isError: true` whose
    `structuredContent` is the `PaymentRequired` object (built by
    `gate.MCPChallenge`).
  - Paid: the client resends the call with the payment in
    `params._meta["x402/payment"]`; `gate.MCPSettle` verifies it, the tool
    runs, and the settlement is returned in
    `result._meta["x402/payment-response"]`.
- Adapting the go-sdk request `_meta` (a `map[string]any`) to
  `dcr402.DecodeMetaPayment` with a two-line helper.

## Prerequisites

Same as `../gate-http-api/`: a dcrlnd node, its `invoice.macaroon`, and the
node `identity_pubkey` as `PAYTO`.

## Run

```
export NETWORK=simnet
export DCRLND_RPC=127.0.0.1:10009
export DCRLND_TLS_CERT=$HOME/.dcrlnd/tls.cert
export DCRLND_MACAROON=$HOME/.dcrlnd/data/chain/decred/simnet/invoice.macaroon
export PAYTO=<identity_pubkey>
go run .
```

The MCP server listens on `http://127.0.0.1:8081/`. Point an MCP client at
it, list tools, and call `process`. The first call returns the
payment-required result; pay the invoice it carries and call again with the
payment in `_meta`.

## Notes

- `gate.MCPSettle` returns an `already` flag on a re-presented proof. In that
  case do not re-run the tool; tell the caller to reuse the credential.
- Free tools need no dcr402 wiring at all; only the tools you price call the
  gate.

## End-to-end example

`../../../examples/simnet/gateway-e2e/main.go` drives an MCP tool through the
dcr402d gateway with a payment (`examples/simnet/harness.sh gateway-e2e`). The
dcr402d gateway (`../../../gateway/`) gates MCP servers this way from YAML,
with no code, if you prefer not to embed the go-sdk yourself; see
`../gateway-proxy/`.
