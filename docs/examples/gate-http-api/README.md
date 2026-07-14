# Example: gate an HTTP API (Lightning per-call)

The smallest seller integration. You wrap a normal `net/http` handler with
`gate.Require(price)` and it charges DCR over the Decred Lightning Network on
every call. No other code changes.

## What it shows

- Constructing a `dcr402.Gate` from a dcrlnd backend, a SQLite store, and a
  network.
- Pricing one route with `gate.Require(amountAtoms)`, leaving another free.
- The per-call flow (F1): 402 challenge, pay, retry with proof, serve plus a
  reusable credential.

## Prerequisites

- A running dcrlnd node and its `invoice.macaroon` (the stock macaroon; no
  spend permission needed). Path:
  `~/.dcrlnd/data/chain/decred/<network>/invoice.macaroon`.
- The node identity pubkey from `lncli getinfo` (`identity_pubkey`).

## Run

```
export NETWORK=simnet
export DCRLND_RPC=127.0.0.1:10009
export DCRLND_TLS_CERT=$HOME/.dcrlnd/tls.cert
export DCRLND_MACAROON=$HOME/.dcrlnd/data/chain/decred/simnet/invoice.macaroon
export PAYTO=<identity_pubkey from lncli getinfo>
go run .
```

The service listens on `127.0.0.1:8080`. A plain request to `/paid` returns
402 with the challenge:

```
curl -i http://127.0.0.1:8080/paid
# HTTP/1.1 402 Payment Required
# Www-Authenticate: LSAT macaroon="...", invoice="lnsdcr..."
# Www-Authenticate: L402 macaroon="...", invoice="lnsdcr..."
# Payment-Required: <base64 PaymentRequired>
```

To complete the flow, a buyer pays the invoice and retries with the
`PAYMENT-SIGNATURE` header (x402) or an `Authorization: L402 ...` header
(classic). See `../pay-by-hand/` for the client side, and `../pay-client-go/`
for a Go client.

## Notes

- `gate.Require` binds the invoice destination to `PayTo`, so every challenge
  invoice must be issued by that node.
- The store persists challenges, settlements, and minted credentials. A
  paid credential is reusable until its TTL, so repeat calls skip payment.

## End-to-end example

`../../../examples/simnet/e2e/main.go` runs this gate against dcrlnd nodes on
simnet and drives a full buyer payment (`examples/simnet/harness.sh e2e`).
