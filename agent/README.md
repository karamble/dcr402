# dcr402-agent

The agent-side wallet/policy daemon: a wallet-as-MCP-server signing broker
that lets any MCP-capable agent spend Decred - Lightning per-call, on-chain
for larger transfers, and automatic x402/L402 handling for paid APIs -
**within hard, owner-set policy**. The agent sees "paid" or "denied", never
a key.

## The security model (why this exists)

The agent never holds keys. Its authority is bounded to spending *within
policy* from scoped credentials:

- **Lightning** via dcrlnd behind a **payment-scoped macaroon**
  (`offchain:read+write`, `invoices:read+write`, `info:read` - no on-chain,
  no admin). The channel balance is the hot-wallet ceiling by construction.
- **On-chain** via a dedicated **dcrwallet account** over mutual-TLS gRPC;
  every transaction is sourced from that account alone, and its balance is
  the on-chain exposure ceiling. The passphrase is held in memory, never
  persisted.
- **Policy is code, not prompt** ([`policy.sample.yaml`](policy.sample.yaml)):
  default-deny destinations, per-payment caps, daily/weekly budgets,
  velocity limits, and a human-approval threshold. Every attempt - allowed
  or blocked - is written to the audit ledger with its full rule trace
  **before** anything is signed. No tool can change policy; that is the
  owner's file alone (SIGHUP reloads it).
- **Spending controls:** spending is governed by deterministic policy
  (default-deny, hard caps, human approval), not by the driving model's
  judgment.

## MCP tools

`wallet_status`, `estimate`, `pay_ln_invoice`, `pay_dcr_address`,
`fetch_paid`, `get_receive_address`, `create_invoice`, `payments_list`,
`receipt_get`, `approval_status`.

Deliberately **absent** (not restricted - not present): key export, macaroon
baking, channel management, policy mutation, seed operations.

`fetch_paid` is the headline: it requests a URL, and on a `402` it
understands **both** the x402 v2 `Payment-Required` envelope (preferred) and
the classic `WWW-Authenticate: L402`, decodes the invoice locally so policy
rules *before* paying, pays over Lightning, retries with the proof, and
caches the returned credential per host so repeat access is free. Denials
and escalations come back as machine-actionable structured tool errors
(rule trace + remaining budget, or a `pending_approval` id) - never a dead
end.

## Human approval over Bison Relay

Payments at or above the approval threshold escalate: the tool returns
`{status: "pending_approval", id, ttl}` immediately (no 15-minute block),
the daemon DMs the owner, and on `yes <id>` the payment executes
asynchronously (re-checked against policy). `no <id>` denies; `no <id>
freeze` denies and is the emergency-stop grammar; an unanswered TTL denies
(fail-closed). The channel is the generic Bison Relay clientrpc, so it works
against any brclient with `[clientrpc]` enabled - nothing bespoke required.

Approvals are also actionable from the web dashboard.

## Embedding

Everything is a library. The `cmd/dcr402-agent` binary is one thin consumer:

```go
d, _ := agent.New(ctx, agent.FromEnv())   // policy, rails, service, approvals
go d.Run(ctx)                             // background loops
srv := mcpserver.New(d.Service, "mainnet") // MCP tools over the go-sdk
// mount srv.MCP() on stdio or streamable HTTP; optionally:
web, _ := web.New(web.Config{Service: d.Service, Network: "mainnet"})
mux.Handle("/", web.Handler())            // the dashboard - or omit this line
```

Third parties can embed the whole daemon, just the `payclient` (the
dual-envelope paid-HTTP engine), or swap the `approve.Approver` for their
own channel. The **web dashboard is one optional package** over the JSON
API: mount it, replace it, or delete its mount line - the core never depends
on it.

## Run it

```
go build ./cmd/dcr402-agent
cp policy.sample.yaml policy.yaml    # edit allowlists, caps, threshold
# see the env below, then:
./dcr402-agent
```

In `http`/`both` mode the daemon serves MCP at `/mcp` and the **web
dashboard at `/` on `DCR402_AGENT_HTTP_ADDR` (default `127.0.0.1:9377`)** -
open `http://127.0.0.1:9377/` for the live payment feed, budget gauges, and
approve/deny of pending payments. (The simnet demo harness runs it on
`127.0.0.1:20970`; see [`../examples/simnet/`](../examples/simnet/) ->
`harness.sh agent-demo`.)

Register with an MCP client (stdio):
`claude mcp add dcr402-agent -- /path/to/dcr402-agent` (set
`DCR402_AGENT_MCP=stdio`).

## Configuration (env)

| Variable | Meaning |
|---|---|
| `DCR402_AGENT_NETWORK` | mainnet \| testnet3 \| simnet |
| `DCR402_AGENT_MCP` | stdio \| http \| both (default http) |
| `DCR402_AGENT_HTTP_ADDR` | serves MCP streamable at `/mcp`, dashboard + JSON API at `/` (default `127.0.0.1:9377`) |
| `DCR402_AGENT_HTTP_TOKEN` | bearer token for `/mcp` and the mutating web API |
| `DCR402_AGENT_DB` / `_POLICY` | SQLite ledger and policy file paths |
| `DCRLND_RPC` / `_TLS_CERT` / `_MACAROON` | Lightning node + payment-scoped macaroon |
| `DCRWALLET_RPC` / `_TLS_CERT` / `_CLIENT_CERT` / `_CLIENT_KEY` / `_ACCOUNT` / `_PASSPHRASE_FILE` | on-chain rail (optional; omit `DCRWALLET_RPC` to disable) |
| `BR_CLIENTRPC` / `_SERVER_CERT` / `_CLIENT_CERT` / `_CLIENT_KEY` / `_RPCUSER` / `_RPCPASS` / `_OWNER` | Bison Relay approvals (optional; owner nick or hex uid) |

### Baking the payment-scoped macaroon

```
lncli bakemacaroon offchain:read offchain:write invoices:read invoices:write info:read \
  --save_to dcr402-agent.macaroon
```

Bind it to `DCRLND_MACAROON`. It cannot move on-chain funds, bake macaroons,
or manage channels.

## Testing

```
go test ./...
```

covers the policy pipeline (every rule, trace snapshots, budget-window
math), the `fetch_paid` engine against a real dcr402 gate over fake backends
(both envelopes, cached-token path, deny/guard), the fail-closed approval
registry, and the MCP error shaping. The live end-to-end (real dcrlnd +
dcrwallet + a running dcr402d gateway, with a Bison Relay approval round
trip) is the harness's `agent-e2e`.
