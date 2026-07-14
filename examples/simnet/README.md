# Simnet harness

A fully local test rig for the dcr402 gate: dcrd (simnet) + a voting
dcrwallet + two dcrlnd nodes (seller and buyer) with a funded channel, plus
an end-to-end run that pays a dcr402-gated endpoint with a **real**
Lightning payment.

```
./harness.sh start        # bring everything up (idempotent-ish; ~30 s warm)
./harness.sh e2e          # library end-to-end (F1 + F2) - exits 0 on PASS
./harness.sh gateway-e2e  # live dcr402d smoke (see below)
./harness.sh bazaar-e2e  # live dcrbazaar smoke (verify/settle/supported/discovery)
./harness.sh status       # heights, sync, channel state
./harness.sh mine [n]     # mine n blocks (used by the e2e polling flows)
./harness.sh stop         # stop all processes
./harness.sh clean        # stop + delete the working directory
```

Everything lives under `~/.dcr402-simnet` (override with
`DCR402_SIMNET_ROOT`): binaries in `bin/`, logs in `logs/`, node state per
component, and `env.sh` with the endpoints/credentials the e2e consumes.

## What `e2e` does

The [e2e program](e2e/main.go) starts a dcr402-gated HTTP endpoint backed by
the **seller's** dcrlnd (stock `invoice.macaroon` only), then acts as the
buyer:

1. bare request -> `402` with the triple challenge (LSAT, L402,
   `PAYMENT-REQUIRED`) carrying a freshly issued `lnsdcr` invoice;
2. pays the invoice through the **buyer's** dcrlnd over the channel
   (`routerrpc.SendPaymentV2`) and obtains the preimage;
3. redeems the proof via `PAYMENT-SIGNATURE` -> `200` + settlement
   (`transaction` = payment hash) + the reusable L402 credential in
   `extensions.l402`;
4. reuses the credential without paying;
5. re-presents the consumed proof -> idempotent `already_settled`, the paid
   handler is not re-executed.

## What `gateway-e2e` does

`gateway-e2e` builds [`dcr402d`](../../gateway/), generates a simnet config
against the live seller node, stands up the [toy upstream](upstream/)
(HTTP + MCP), and runs the [driver](gateway-e2e/) as the buyer:

1. `GET /api/hello` through the proxy: `402` -> real Lightning payment ->
   origin content served;
2. MCP per-call on `/mcp2`: unpaid `tools/call` answers the
   payment-required tool result, a real payment in `_meta["x402/payment"]`
   settles it, and the executed tool's result carries the settlement in
   `result._meta["x402/payment-response"]`;
3. a Lightning top-up through `/_dcr402/topup` returning the account
   credential;
4. credit-gated `tools/call` on `/mcp` burning that balance
   (`Dcr402-Balance` header), free tools passing without a credential, and
   `/_dcr402/balance` agreeing.

The gateway and upstream are torn down afterwards; their logs land in
`logs/dcr402d.log` and `logs/upstream.log`.

## What `bazaar-e2e` does

`bazaar-e2e` builds [`dcrbazaar`](../../bazaar/), starts it on simnet
(port 20960), and runs the [driver](bazaar-e2e/main.go) as both seller
and buyer:

1. the seller creates a real invoice on its dcrlnd; the buyer pays it over the
   channel (`routerrpc.SendPaymentV2`) and obtains the preimage;
2. `POST /verify` with the payload and offered requirements returns
   `isValid:true`, using the same verification code the embedded gate uses;
3. `POST /settle` returns a notarized settlement (transaction = the payment
   hash) and a second settle replays it idempotently, with no funds moved;
4. `GET /supported` advertises the `exact` scheme on the simnet CAIP-2 id;
5. a resource is submitted to the discovery index and then listed and searched
   through `/discovery/resources` and `/discovery/search`;
6. a payment for a network the facilitator does not serve returns
   `isValid:false` with `invalid_network`.

dcrbazaar is torn down afterwards; its log lands in `logs/dcrbazaar.log`.

## Watching the web dashboard (`agent-demo`)

`agent-demo` is `agent-e2e`'s interactive sibling: it stands up the same
stack, seeds watchable activity (a paid `fetch_paid` + its cached re-read,
an on-chain send, a memo-less **denial with a red rule trace**), leaves one
**over-threshold payment awaiting your approval**, and then **leaves
everything running** so you can drive it from the browser.

```
./examples/simnet/harness.sh agent-demo
# -> open  http://127.0.0.1:20970/  in a browser
# -> click "approve" on the pending 0.6 DCR payment and watch it settle live
./examples/simnet/harness.sh stop     # when you're done
```

The **dashboard is served on `127.0.0.1:20970`** (the harness's agent HTTP
port - the standalone binary defaults to `127.0.0.1:9377`). It renders live
over SSE: the activity feed, budget gauges, rail balance cards, and the
pending-approval strip all update as payments happen. Clicking **approve**
resolves the escalation and fires the (re-policy-checked) payment
asynchronously.

The demo mines simnet blocks between on-chain sends so their change confirms
- on-chain payments need confirmations, Lightning does not.

## Prerequisites

- `dcrd` and `dcrwallet` binaries in `~/go/bin` or `PATH` (recent builds).
- Local checkout of [decred/dcrlnd](https://github.com/decred/dcrlnd) at
  `~/go/src/github.com/decred/dcrlnd` - `dcrlnd`/`dcrlncli` are built from
  it on first start (dcrlnd must match the dcrd RPC API version, so the
  harness builds from source rather than a release tag).
- `dcrctl` is `go install`ed automatically (`decred.org/dcrctl@master`).
- A [decred/dcrdex](https://github.com/decred/dcrdex) checkout for
  `dex/testing/dcr/harnesschain.tar.gz` (override location with
  `DCRDEX_TESTING_DCR`) - see below.
- `jq`.

## How the chain stays alive (and the dcrdex credit)

Decred simnet needs stakeholder votes past stake validation height
(height 144 on simnet), so a naive from-genesis setup stalls. This harness
adopts the approach of dcrdex's `dex/testing/dcr` harness (ISC): seed the
chain from their prebaked snapshot (already past SVH with a live ticket
pool) and run their alpha wallet - same public dev seed - with
`enablevoting` + `enableticketbuyer`, so every mined block gets votes and
the ticket pool replenishes. Mining is `regentemplate` + `generate 1` with
a short pause for votes to land.

The alpha seed and mining address are public development constants for
simnet use only.

## Ports

| Component | P2P | RPC |
|---|---|---|
| dcrd | 20560 | 20561 |
| voting wallet | - | 20562 |
| agent dcrwallet | - | 20563 (JSON-RPC), 20564 (gRPC) |
| seller dcrlnd | 20735 | 20009 |
| buyer dcrlnd | 20736 | 20010 |

HTTP services stood up by the e2e drivers: facilitator (dcrbazaar) 20960,
dcr402-agent 20970, gateway (dcr402d) 20980, toy upstream 20990.

(Chosen to not collide with the dcrdex harness's 195xx range, so both can
run side by side.)

## Troubleshooting

- Component logs: `~/.dcr402-simnet/logs/*.log`.
- `harness.sh status` for a quick health read.
- A wedged state is cheap to discard: `harness.sh clean && harness.sh start`
  (the chain snapshot makes a fresh start fast).
- If mining seems stuck, check the wallet log for voting errors - the
  voting wallet must be running before blocks are mined.
