# dcrbazaar

`dcrbazaar` is the dcr402 **discovery index** and **non-custodial
payment-verification service** for Decred. It is **not** a settlement
facilitator: Decred payments settle peer-to-peer at the seller's own node
(lightning) or by the payer's own on-chain broadcast, so dcrbazaar holds no
keys and moves no funds. It exposes the x402 v2 `/verify`, `/settle`,
`/supported` endpoints (so stock x402 tooling interoperates) as a *verification
service*, plus a Bazaar-style discovery index.

## What it does

- `GET /discovery/resources`, `GET /discovery/search`,
  `POST /discovery/submit` - the resource index agents query to find
  DCR-payable services, and sellers register themselves in (the network-effect
  asset, and dcrbazaar's primary reason to exist).
- `POST /verify` - verify-as-a-service. For the **lightning** method it applies
  the scheme's stateless cryptographic checks and reuses the exact same
  `lib.VerifyLightningProof` the embedded gate uses, so a verifier and a seller
  can never disagree. For the **onchain** method (when enabled) it confirms a
  payer-broadcast deposit by reading the chain through dcrdata - it checks the
  transaction pays the required amount to `payTo` at the required depth.
- `POST /settle` - re-checks the payment and records a notarized settlement,
  idempotent by payment hash (lightning) or txid (onchain). It moves no funds.
- `GET /supported` - advertises the `(scheme, network, method)` kinds this
  instance verifies, in the standard x402 shape. `signers` is always empty:
  neither Decred method has a settlement signer.

## Why this exists (and what it deliberately is not)

In the default deployment there is no third party at all: the buyer's node pays
the seller's node and the seller verifies the preimage against its own node -
the strongest possible check. dcrbazaar is useful for:

- **discovery**: one index agents can search across many services;
- **verify-as-a-service**: a seller that would rather not run chain-reading
  infrastructure can ask dcrbazaar "did this on-chain deposit confirm?" (the
  onchain method), or get a third-party notarized receipt for a lightning
  preimage;
- **x402 ecosystem interop**: tools that expect a facilitator at `/verify`.

It is **not** a settlement broker. A Lightning preimage for an invoice signed by
`payTo` is itself the proof, and an on-chain payment is broadcast by the payer;
dcrbazaar only reads and notarizes. Custodial receive (a node receiving on a
seller's behalf) is explicitly out of scope.

## Run it

```
go build ./cmd/dcrbazaar
./dcrbazaar -config dcrbazaar.yaml
```

See `dcrbazaar.sample.yaml` for the full configuration. A minimal open,
in-memory, single-network instance is:

```yaml
listen: ":8444"
networks: [mainnet]
discovery:
  enabled: true
  public_submit: true
```

To also confirm on-chain deposits, enable the onchain method:

```yaml
onchain:
  enabled: true
  dcrdata_url: "https://dcrdata.decred.org"
  min_confs: 1
```

## Run with Docker

The image is a fully static binary (pure-Go sqlite, no cgo). Build it from the
repository root - the build needs the sibling `lib/` module:

```
docker build -f bazaar/Dockerfile -t dcrbazaar:latest .
```

Or use the compose file, which sets the build context and a persistent volume
for the SQLite store:

```
cd bazaar
cp dcrbazaar.sample.yaml dcrbazaar.yaml   # then edit (set store: /var/lib/dcrbazaar/dcrbazaar.db)
docker compose up -d --build
```

It listens on `:8444` (published to `127.0.0.1` by default). Put your own
reverse proxy (traefik, nginx, caddy) in front for a public hostname and TLS -
dcrbazaar itself speaks plain HTTP unless you set `tls_cert`/`tls_key`.

## Register a resource

A dcr402 or dcr402d deployment lists itself in the index with the submit
client:

```go
import "github.com/karamble/dcr402/bazaar"

bazaar.Submit(ctx, "https://fac.example.com", "", bazaar.SubmitRequest{
    Resource: "https://api.example.com/weather",
    Accepts:  accepts, // the resource's 402 accepts[] entries
    Metadata: x402.ResourceInfo{ServiceName: "Example Weather", Tags: []string{"weather"}},
})
```

A runnable walkthrough is in
[`../docs/examples/run-dcrbazaar`](../docs/examples/run-dcrbazaar); the
simnet end-to-end driver is
[`../examples/simnet/bazaar-e2e`](../examples/simnet/bazaar-e2e).
