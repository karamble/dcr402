# dcr402d (gateway)

A standalone reverse-proxy payment gateway for operators who won't touch
code: put dcr402d in front of any HTTP API or Streamable-HTTP MCP server
and price it in DCR - per-call Lightning payments or prepaid credit
accounts - from one YAML file. Single binary, SQLite state, and the stock
dcrlnd `invoice.macaroon` (no spend permissions).

## Quickstart

```
cd gateway && go build ./cmd/dcr402d
cp dcr402d.sample.yaml dcr402d.yaml   # edit: network, payto, ln paths, upstreams
./dcr402d -config dcr402d.yaml
```

## What it does per route

| Upstream kind | Free | Gated |
|---|---|---|
| HTTP (`mcp: false`) | any path without a price entry (unless `price_default` is set) | exact `"METHOD /path"` entries from `prices` |
| MCP (`mcp: true`) | everything that is not `tools/call` - `initialize`, `notifications/*`, `tools/list`, resource reads, the GET/SSE channel | `tools/call`, priced by tool name (`tool_prices`, `tool_price_default`, minus `free_tools`) |

Two pricing modes per upstream:

- **`percall`** - each priced request answers `402` with the dual envelope
  (`WWW-Authenticate: LSAT`/`L402` + `PAYMENT-REQUIRED`) until a payment
  proof or a valid L402 credential is presented. On MCP upstreams this is
  the official x402 MCP transport: unpaid `tools/call` returns the
  payment-required tool result; a call carrying `_meta["x402/payment"]` is
  verified, settled exactly once, and forwarded with the settlement
  injected into `result._meta["x402/payment-response"]` (and mirrored in
  the `Payment-Response` HTTP header). Replays of a consumed proof are
  answered idempotently without re-executing the tool.
- **`credits`** - requests carry an L402 credential; the gateway charges
  the account atomically and forwards (remaining balance in the
  `Dcr402-Balance` header). Insufficient balances are machine-actionable -
  exact `shortfallAtoms` plus the top-up path - as JSON over HTTP and as a
  structured tool error over MCP.

## Built-in endpoints

| Path | Purpose |
|---|---|
| `/_dcr402/topup` | credit top-up: `402` offering Lightning **and** (when `credits.onchain`) an on-chain deposit address, both funding one account; settlement grants atoms and returns the account credential |
| `/_dcr402/balance` | account balance for the presented credential |
| `/_dcr402/` + `/_dcr402/info` | human status page + machine-readable pricing/network info (`web.status_page`) |

## Notes and limits

- Prices are decimal DCR strings parsed exactly (max 8 decimals, no
  floats).
- JSON-RPC batches containing `tools/call` are refused with `400` (MCP
  removed batching; a batched call can't be gated per tool).
- Paid `tools/call` responses are buffered to inject the settlement
  `_meta`; SSE responses are forwarded verbatim with the settlement in the
  `Payment-Response` header only.
- The gateway needs exactly one node artifact: `invoice.macaroon`
  (invoices + addresses + on-chain read). It cannot spend.

Wire behavior is specified in [`../scheme/`](../scheme/); the gating engine
is [`../lib/`](../lib/).
