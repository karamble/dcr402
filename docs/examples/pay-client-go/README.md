# Example: consume a paid resource in Go

Pay a 402-protected resource from a Go program by embedding the dcr402-agent
pay-client. The pay-client understands both challenge envelopes (x402 v2 and
classic L402), decodes the invoice locally so you can rule on it before
paying, pays over Lightning, retries with the proof, and caches the returned
credential so repeat access is free.

## What it shows

- Wiring `payclient.New` with three dependencies: a Lightning rail
  (`agent/rails/ln`, a dcrlnd client), a credential cache
  (`agent/store.NewMemory`), and a policy hook.
- The policy hook (`Gate`): called with the decoded amount and destination
  before any payment. This example allows everything and logs; a real client
  enforces caps or allowlists here.
- `client.FetchPaid` twice against the same URL: the first call pays
  (`rail=x402` or `rail=l402`), the second is served from the cached
  credential (`rail=cached`, `paid=0`).

## Prerequisites

- A buyer dcrlnd node with a channel that can route to the seller, reached
  with a payment-scoped macaroon (see the security chapter for the exact
  bake).
- A running paid endpoint. Point `URL` at one, for example the
  `../gate-http-api/` service or a dcr402d route.

## Run

```
export NETWORK=simnet
export DCRLND_RPC=127.0.0.1:10010
export DCRLND_TLS_CERT=$HOME/.dcrlnd-buyer/tls.cert
export DCRLND_MACAROON=/path/to/payment-scoped.macaroon
export URL=http://127.0.0.1:8080/paid
go run .
```

Expected output:

```
about to pay 250000 atoms to 127.0.0.1
first  call: rail=x402 paid=250000 atoms
  body: this response cost you 0.0025 DCR
second call: rail=cached paid=0 atoms (cached credential)
  body: this response cost you 0.0025 DCR
```

## Notes

- Set `FetchOptions.ForceLegacy` to use the classic L402 envelope when a
  service offers both, for testing that path.
- The pay-client never pays more than the challenge asks, and its `Estimate`
  method probes or decodes without paying.

## End-to-end example

`../../../examples/simnet/agent-e2e/main.go` drives the full agent (which uses
this pay-client) against dcrlnd nodes, including a `fetch_paid` against the
dcr402d gateway paid once and served cached on the second call
(`examples/simnet/harness.sh agent-e2e`).
