# Example: front a service with dcr402d (no code)

Charge for an HTTP API or an MCP server you already run, without changing it.
The dcr402d reverse proxy sits in front, reads one YAML file, and gates the
routes you price. This is the zero-code seller path; the same payment engine
as the lib examples, driven by config.

## What it shows

- A complete `dcr402d.yaml` with two upstreams: a plain HTTP API gated per
  call by exact `"METHOD /path"` price, and an MCP server where only
  `tools/call` is priced, by tool name.
- The MCP-aware routing: `initialize`, `tools/list`, notifications, and the
  GET/SSE channel pass through free; only priced tool calls are gated.
- The built-in endpoints: a status page at `/_dcr402/` and machine-readable
  pricing at `/_dcr402/info`.

## Prerequisites

- The `dcr402d` binary: `cd ../../../gateway && go build ./cmd/dcr402d`.
- A dcrlnd node with `invoice.macaroon`, and its `identity_pubkey` as
  `payto`.
- A service to front. For a quick test, `../../../examples/simnet/upstream`
  is a toy origin that serves a paid HTTP route and a minimal MCP endpoint.

## Run

Edit `dcr402d.yaml` (set `payto`, the `ln` paths, and your upstream
`target`s), then:

```
dcr402d -config dcr402d.yaml
```

Requests to a priced route return 402 until paid; unpriced routes and
non-`tools/call` MCP methods pass straight through. Check the pricing the
gateway advertises:

```
curl -s http://127.0.0.1:8443/_dcr402/info
```

## Credit mode

To charge from prepaid balances instead of per call, add a `credits` block
and set an upstream `mode: credits`:

```yaml
credits: { enabled: true, onchain: true, confirmations: 2 }
topup:    { min: "0.001", max: "10", default: "0.01" }
```

dcr402d then serves `/_dcr402/topup` and charges each priced call against the
caller's account. See `../credit-accounts/` for the same flow embedded in Go.

## End-to-end example

`../../../examples/simnet/harness.sh gateway-e2e` builds dcr402d, generates a
config like this one against simnet nodes, and drives HTTP per-call, the MCP
transport, a top-up, and credit burns. The full config reference is
`../../../gateway/README.md` and `../../../gateway/dcr402d.sample.yaml`.
