# Example: run a facilitator (dcrbazaar)

Run `dcrbazaar`, the dcr402 payment facilitator, so agents can verify DCR
payments and discover DCR-payable services through the standard x402 v2
facilitator API. dcrbazaar is non-custodial (trust topology T2): it verifies
payment proofs and notarizes settlements, but never holds keys or funds. This
is the "run the facilitator binary" path, parallel to running dcr402d.

## What it shows

- The standard facilitator endpoints: `POST /verify`, `POST /settle`, and
  `GET /supported`, from one YAML file.
- The discovery index: `GET /discovery/resources`, `GET /discovery/search`,
  and `POST /discovery/submit`, so a service can list itself for agents to
  find.
- How a seller registers a resource with the submit client.

## When you want one

In the default deployment there is no facilitator: the buyer pays the seller's
node and the seller verifies the preimage against its own node, the strongest
check. Run a facilitator when you want a shared discovery index, a third-party
settlement record, or interop with x402 tooling that expects a facilitator at
`/verify`. See `../../architecture.md` and `../../security.md` for the trust
model. Custodial receive (T3) is out of scope for dcrbazaar, which is
non-custodial.

## Prerequisites

- The `dcrbazaar` binary: `cd ../../../facilitator && go build ./cmd/dcrbazaar`.

That is all dcrbazaar needs to verify the lightning method: verification is
stateless (a valid preimage for an invoice signed by `payTo` is the proof), so
the facilitator holds no node and no keys.

## Run

```
dcrbazaar -config dcrbazaar.yaml
```

Check what it serves:

```
curl -s http://127.0.0.1:8444/supported
```

A resource server (or a client) verifies a payment by POSTing the client's
payload and the offered requirements:

```
curl -s -X POST http://127.0.0.1:8444/verify \
  -H 'Content-Type: application/json' \
  -d '{"x402Version":2,"paymentPayload":{...},"paymentRequirements":{...}}'
# -> {"isValid":true}   (or {"isValid":false,"invalidReason":"..."})
```

`/settle` takes the same body and returns a notarized SettlementResponse; it is
idempotent by payment hash and moves no funds.

## Register a resource

A dcr402 or dcr402d deployment lists itself in the discovery index once per
resource, with the submit client from the facilitator package:

```go
import (
    facilitator "github.com/karamble/dcr402/facilitator"
    "github.com/karamble/dcr402/lib/x402"
)

err := facilitator.Submit(ctx, "http://127.0.0.1:8444", "" /* api key */, facilitator.SubmitRequest{
    Resource: "https://api.example.com/weather",
    Accepts:  accepts, // the resource's 402 accepts[] entries
    Metadata: x402.ResourceInfo{ServiceName: "Example Weather", Tags: []string{"weather"}},
})
```

Then agents find it:

```
curl -s 'http://127.0.0.1:8444/discovery/search?query=weather'
```

## End-to-end example

`../../../examples/simnet/harness.sh bazaar-e2e` builds dcrbazaar, starts it
against simnet nodes, pays an invoice from the buyer node, and drives
`/verify`, `/settle` (with an idempotent replay), `/supported`, and the
discovery submit/list/search. The full reference is
`../../../bazaar/README.md` and `../../../bazaar/dcrbazaar.sample.yaml`.

## A note on payer identity

A settlement from dcrbazaar leaves `payer` empty. A Lightning preimage proves the
payee's node was paid but reveals no payer identity (the invoice binds only the
payee), so there is no payer address to report. This is a property of the
Lightning method, not a gap.
