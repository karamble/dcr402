# Glossary

Terms used across the dcr402 suite. Where a term has a precise normative
definition, the authoritative source is named.

## Amounts and money

- atom: Decred's atomic unit. 1 DCR = 100,000,000 atoms (1e8). All amounts
  on the wire are atoms, as decimal strings.
- milli-atom (m-atom): one thousandth of an atom. 1 DCR = 1e11 m-atoms.
  BOLT11 Lightning invoices encode milli-atoms. The equality rule: a decoded
  invoice amount in m-atoms must equal the offer's `amount` (atoms) times
  1000. See `../scheme/scheme_exact_dcr.md`, Amount Formatting.
- credit: a prepaid, atoms-denominated balance held by a seller for a buyer
  account. Funded by a top-up (F2) and burned per call with no payment
  latency. See `../lib/README.md`, Credits.
- top-up: a payment that grants credits. Offered on both the Lightning and
  on-chain methods so the slow rail can fund the fast one.

## Lightning and cryptographic proof

- BOLT11: the Lightning invoice encoding. A dcr402 challenge carries a Decred
  BOLT11 invoice; the buyer pays it and gets a preimage.
- preimage: the 32-byte secret revealed to the payer when a Lightning HTLC
  settles. Proof of a completed payment. `SHA-256(preimage)` equals the
  payment hash.
- payment hash: the SHA-256 of the preimage, committed in the invoice.
  Identifies a payment and is the seller's replay key. See scheme rules 3
  and 8.
- HRP: the human-readable prefix of a BOLT11 invoice, network-bound:
  `lndcr` (mainnet), `lntdcr` (testnet3), `lnsdcr` (simnet). Verifiers reject
  a prefix that does not match the offer's network.
- MPP: multi-path payment. A payment split across several routes; the
  preimage is released only when the full amount is assembled.

## L402 credentials

- L402: the Lightning-native HTTP 402 convention (invoice, macaroon,
  preimage). Formerly named LSAT. See `../scheme/l402-dual-envelope.md`.
- LSAT: the former name of L402. Servers emit both keywords in the
  `WWW-Authenticate` challenge (LSAT first) and accept both in
  `Authorization`, for compatibility.
- macaroon: an HMAC-chain bearer credential. In dcr402 it is minted at
  challenge time; after payment the buyer presents it plus the preimage as
  the reusable credential for a service.
- caveat: a first-party predicate appended to a macaroon that restricts its
  authority (for example `services=name:tier`, `<service>_valid_until`).
  Caveats can only narrow a credential, never widen it.
- dual envelope: emitting an x402 v2 challenge and a classic L402 challenge
  from one 402, behind one invoice, so both client populations work. See
  the annex in `../scheme/l402-dual-envelope.md`.

## x402 and the scheme

- x402: the HTTP 402 payment protocol this suite rides. Version 2 carries all
  payment data in headers. Governed by the Linux Foundation x402 project.
- exact: the x402 payment scheme dcr402 implements, meaning the buyer pays a
  specific amount known in advance. See scheme Scheme Name.
- scheme, network, asset: the three identifying fields of an offer. dcr402
  uses `scheme: "exact"`, `network:` a Decred CAIP-2 id, `asset: "DCR"`.
- assetTransferMethod: the field in an offer's `extra` that selects the
  transfer method, `lightning` or `onchain`. Required on every dcr402 offer.
- CAIP-2: the Chain Agnostic network identifier format, `namespace:reference`.
- bip122: the CAIP-2 namespace for Bitcoin-family chains, whose reference is
  the first 32 hex characters of the genesis block hash. Decred uses it, for
  example `bip122:298e5cc3d985bfe7f81dc135f360abe0` (mainnet).
- PAYMENT-REQUIRED, PAYMENT-SIGNATURE, PAYMENT-RESPONSE: the three x402 v2
  HTTP headers, each base64 of a UTF-8 JSON object. See scheme x402 v2
  Headers.

## Trust and roles

- T1, T2, T3: the three trust topologies (peer-to-peer, verify-assisted,
  custodial receive). See `architecture.md`, Trust topologies.
- facilitator (dcrbazaar): an optional service that verifies and settles proofs
  and hosts a discovery index. Enables T2 and T3.
- escalation, approval: when a buyer payment exceeds the owner's approval
  threshold, the agent parks it as a pending approval and asks the owner to
  approve or deny (F4). Fails closed on timeout.
- Bison Relay: an end-to-end encrypted, self-hosted messaging network on
  Decred. dcr402-agent uses its clientrpc interface to send approval requests
  to the owner and receive yes/no replies.
- clientrpc: Bison Relay's JSON-RPC-over-WebSocket control interface, secured
  with mutual TLS. The agent connects to any brclient that exposes it.

## Decred node software

- dcrd: the Decred full node. Provides block and transaction data; sellers or
  facilitators use it to confirm on-chain deposits.
- dcrlnd: the Decred Lightning Network daemon. Both sides use it: the seller
  to issue and check invoices, the buyer to pay them.
- dcrwallet: the Decred wallet daemon. The buyer's on-chain rail spends from
  a dedicated dcrwallet account over gRPC.
