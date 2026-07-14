# Security model

This chapter consolidates the security properties of the dcr402 suite that
are otherwise spread across the scheme specification and the component
READMEs. It is descriptive: the normative rules live in
`../scheme/scheme_exact_dcr.md` (verification rules and security
considerations) and `../scheme/l402-dual-envelope.md`.

## Key isolation on both sides

Neither the seller service nor the buyer agent is trusted with a spending
key. Each holds a least-privilege dcrlnd macaroon, and the on-chain buyer
holds a scoped wallet account.

### Seller: invoice-scoped macaroon

A dcr402 or dcr402d seller needs only the stock `invoice.macaroon` that
dcrlnd bakes by default:

```
invoices:read, invoices:write, address:read, address:write, onchain:read
```

Path: `~/.dcrlnd/data/chain/decred/<network>/invoice.macaroon`.

This covers the entire seller surface: create invoices, look up settlement
state, generate on-chain deposit addresses, and watch confirmations. It
carries no `offchain` permission and cannot send Lightning payments.
`Config.PayTo` (or the gateway `payto`) is the node's identity pubkey from
`lncli getinfo`; verification binds every challenge invoice's destination to
it.

### Buyer: payment-scoped macaroon

A dcr402-agent buyer bakes a custom macaroon limited to paying and reading:

```
lncli bakemacaroon offchain:read offchain:write invoices:read invoices:write info:read \
  --save_to dcr402-agent.macaroon
```

It can send and track Lightning payments, decode invoices, create receive
invoices, and read node info. It cannot move on-chain funds, bake further
macaroons, or manage channels. The channel balance is the agent's hot-wallet
ceiling by construction: fund channels with what you are willing to expose.

### Buyer: on-chain account and mutual TLS

The on-chain rail spends from a dedicated dcrwallet account (default name
`agent`) over gRPC. dcrwallet gRPC uses mutual TLS, not macaroons: the client
presents a certificate whose CA is listed in the wallet's `clients.pem`.
dcrwallet will not serve gRPC at all unless a trusted client CA exists, so
the account setup includes writing that CA. See
`examples/run-agent/README.md` and `examples/simnet/walletcert`.

Every constructed transaction sources from the dedicated account only, so the
account balance is the on-chain exposure ceiling. The wallet passphrase is
supplied per signing call and held in memory; it is never written to the
store or the logs.

## Replay protection

A payment proof is single-use. The seller consumes it exactly once and is
idempotent to honest retries.

- Lightning: the payment hash is the key. The first successful presentation
  consumes it. A later `PAYMENT-SIGNATURE` with the same hash does not
  re-execute the paid operation; within a bounded window it returns the
  original `SettlementResponse`, after which it is rejected as reused. The
  seller also correlates the hash with a challenge it actually issued for
  that resource at that price, so a valid preimage for some other invoice
  does not verify.
- On-chain: the pair (txid, deposit address) is the key, with the same
  consume-once and idempotent-replay behavior. Deposit addresses are fresh
  per challenge and never reused.

The seller enforces this in its store (the consume-once settlement record).
Settlement records are retained at least as long as any credential minted
against them remains valid.

## Pay-first (TOCTOU)

For both methods the funds move before the retry request. The proof is
evidence of a completed payment, not an authorization to execute one later.
Consequently the buyer bears delivery risk, the inverse of schemes where the
seller broadcasts at settlement time and risks serving before funds land.

The buyer's position is backed structurally:

- A valid (paymentHash, preimage) presentation is honored for at least the
  challenge and credential TTL, across seller restarts, because the
  settlement store is durable.
- Receipts are self-certifying (below), so a buyer can prove payment offline
  in a dispute.
- The L402 credential is minted at challenge time, so the buyer holds a
  durable bearer credential before paying.

The seller-side verify-then-deliver race does not exist here: payment is
final at verification time.

## Multi-path payments and hold invoices

Decred Lightning supports multi-path payments. MPP is safe: the payee node
releases the preimage only once the full amount is assembled, and timed-out
partial shards refund with no preimage. There is no partial-payment hazard.
Challenge invoices must not be hold (hodl) invoices, which would let the
recipient delay settlement after the payer commits and break the premise
that holding the preimage means the payment completed.

## On-chain reorganizations

The seller re-checks confirmation depth at settlement. Residual reorg risk at
the chosen depth is accepted by policy. Decred's hybrid proof-of-work and
proof-of-stake consensus makes even shallow reorganizations very costly (a
block needs stakeholder votes to be extended), which is why 1 to 2
confirmations is the recommended default.

## Cross-network isolation

A proof from one Decred network cannot verify on another. Invoice human
readable prefixes are network-bound (`lndcr`, `lntdcr`, `lnsdcr`), address
encodings are network-specific, and the CAIP-2 network identifier is part of
the envelope checks. No additional binding is needed.

## Custody model

T1 and T2 are non-custodial: funds settle directly to the seller. T3
(facilitator custodial receive) is an optional mode where the facilitator
briefly holds the seller's float between receiving a payment and sweeping it to
the seller's address; low sweep thresholds keep that window short. T1 and T2
sellers hold their own funds throughout.

## Agent spending controls

Agent spending is governed by deterministic, owner-controlled policy rather
than by the driving model's judgment:

- Policy is code, not prompt. Spending limits, destination allowlists,
  velocity, and the approval threshold live in a file the owner edits
  (`policy.yaml`, reloaded on SIGHUP). See `../agent/policy.sample.yaml`.
- Default-deny destinations. In default-deny mode a payment matches an
  allowlist for its kind (domain, LN node, or address) to proceed.
- Human approval above a threshold, failing closed on timeout or an
  unreachable channel.
- The MCP surface exposes payment tools only. Policy mutation, key export,
  macaroon baking, and channel management are not part of it.

Every attempt, allowed or held, is written to the audit ledger with its full
rule trace before anything is signed.

## Receipts

Receipts are self-certifying and verifiable offline.

- Lightning: `{invoice, preimage, settledAt, amount}`. The invoice signature
  proves the payee's node issued it; `SHA-256(preimage) == paymentHash`
  proves it was paid. No server cooperation and no trusted timestamp are
  needed.
- On-chain: `{txid, payTo, amount, settledAt}`, verifiable against any copy
  of the chain.

Sellers may additionally emit signed offers and receipts per the x402
offer-and-receipt extension; that mechanism is complementary.

## System-wide invariants

- Least-privilege macaroons on both sides, with the exact permission lists
  documented above.
- Amounts are always re-verified server-side against the issued challenge;
  preimage verification is never delegated to a client claim.
- Replay is prevented by payment-hash and (txid, address) uniqueness;
  credential caveats bind service, tier, and expiry.
- Agent spending follows deterministic policy (default-deny, human approval);
  the seller verifies every proof server-side, independent of client claims.
- Receipts everywhere, exportable as JSON on both sides.

## Bearer credentials

The L402 credential and the raw preimage are bearer secrets. Serve and
accept them over TLS only. Clients must not log complete `Authorization`
values. Credential revocation is root-key deletion in the seller store, and
is immediate. A client may attenuate a credential before delegating it (add
tighter caveats) without contacting the seller.

## Deployment

How the components are configured in production.

Agent (dcr402-agent):

- The MCP surface and the dashboard API are authenticated. Set
  `DCR402_AGENT_HTTP_TOKEN`; if it is unset, the daemon generates a token and
  logs it once at startup.
- `fetch_paid` reaches external endpoints. Loopback, private, and link-local
  addresses are out of scope; allow a specific local target (a development
  gateway) with `DCR402_AGENT_FETCH_ALLOW=host:port`.
- The Bison Relay owner is identified by their 64-hex user id.
- The owner controls a freeze switch: reply `freeze` over Bison Relay or post
  to the authenticated `/api/freeze` to halt all spending, and `unfreeze` to
  resume. Unfreeze is owner-only and is not an MCP tool.

Seller (lib and dcr402d):

- Settlement binds the proof to the resource's price: a proof or credential for
  one endpoint settles that endpoint.
- The L402 credential is service-scoped. One service name is one price tier;
  charge different prices with distinct service names (or the credit-account
  path).
- On-chain top-up settlement is bound to the challenge recipient: the challenge
  carries a per-challenge credential secret in its `extra`, so the funded
  credit is claimed by the party that received the challenge. See `../scheme/`
  rule 9.
- Credit-account charges honor an `Idempotency-Key` request header: a retry
  carrying the same key is charged at most once. Without the header every call
  charges.

Facilitator (dcrbazaar):

- A public instance sets `api_keys`, which gate `/verify`, `/settle`, and
  non-public submit.
- The discovery index is a hint: a buyer re-fetches the origin's own 402 and
  pays what the origin offers. The index is bounded by a per-instance resource
  cap and a per-request page cap.
