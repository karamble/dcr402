# Example: credit accounts and top-ups (F2)

Prepaid balances. A buyer tops up an account once (over Lightning or
on-chain), then spends from the balance with no payment latency per call.
This is the "slow rail funds the fast rail" pattern.

## What it shows

- Enabling credits by passing a `Ledger` (and an `Onchain` backend for the
  on-chain top-up method) to `dcr402.New`.
- `gate.Topup(...)`: a 402 that offers both a Lightning invoice and a fresh
  on-chain deposit address, minted over one account. On settlement the buyer
  is granted credits and handed the account credential.
- `gate.RequireCredits(tool, amountAtoms)`: gate a route on the credit
  balance instead of a per-call payment. The remaining balance rides the
  `Dcr402-Balance` response header; an empty balance returns a
  machine-actionable `insufficient_credits` error with the shortfall and the
  top-up path.

## Prerequisites

Same as `../gate-http-api/`, plus the on-chain top-up method needs the
node's wallet (the stock `invoice.macaroon` already grants `address:write`
and `onchain:read`, which is enough to hand out deposit addresses and watch
confirmations).

## Run

```
export NETWORK=simnet
export DCRLND_RPC=127.0.0.1:10009
export DCRLND_TLS_CERT=$HOME/.dcrlnd/tls.cert
export DCRLND_MACAROON=$HOME/.dcrlnd/data/chain/decred/simnet/invoice.macaroon
export PAYTO=<identity_pubkey>
go run .
```

The service listens on `127.0.0.1:8082`.

```
# Ask for a 0.01 DCR top-up challenge:
curl -i "http://127.0.0.1:8082/topup?atoms=1000000"

# Spend a credit (needs the Authorization credential from a settled top-up):
curl -i http://127.0.0.1:8082/search -H "Authorization: L402 <macaroon>:<preimage>"
```

## Notes

- The ledger balance is keyed by the credential token id, so it survives
  credential rotation.
- On-chain top-ups return the retryable `insufficient_confirmations`
  response until the deposit reaches the configured depth.

## End-to-end example

`../../../examples/simnet/e2e/main.go` (the F2 section) and
`examples/simnet/harness.sh e2e` run an on-chain top-up followed by
credit-burning calls against dcrlnd nodes.
