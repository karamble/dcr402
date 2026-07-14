# dcr402 (library)

The embeddable Go payment gate: price HTTP handlers and MCP tools in DCR
over the Lightning Network. Implements [`../scheme/scheme_exact_dcr.md`](../scheme/scheme_exact_dcr.md)
(the `exact`/Decred scheme, lightning method) and
[`../scheme/l402-dual-envelope.md`](../scheme/l402-dual-envelope.md) (dual
x402 v2 + L402/LSAT envelope, token minting, precedence) - pinned to the
repository test vectors by the test suite.

```go
import (
    dcr402 "github.com/karamble/dcr402/lib"
    dcr402lnd "github.com/karamble/dcr402/lib/dcrlnd"
    "github.com/karamble/dcr402/lib/store"
)

ln, _ := dcr402lnd.New(dcr402lnd.Config{
    Host:         "localhost:10009",
    TLSCertPath:  "~/.dcrlnd/tls.cert",
    MacaroonPath: "~/.dcrlnd/data/chain/decred/mainnet/invoice.macaroon",
})
st, _ := store.OpenSQLite("dcr402.db")
gate, _ := dcr402.New(dcr402.Config{
    Backend: ln,
    Store:   st,
    Network: dcr402.Mainnet,
    PayTo:   "<dcrlnd identity pubkey, 33-byte compressed hex>",
    Service: "myservice",
})

// HTTP: gate a handler at 250,000 atoms (0.0025 DCR) per call.
mux.Handle("/api/v1/process", gate.Require(250000)(handler))

// MCP server: one reusable bracket runs the whole x402 MCP transport — no
// payment in _meta yields a spec-shaped challenge; a payment verifies+settles
// and hands back the receipt to attach under _meta["x402/payment-response"].
out, vErr, _ := gate.ServeMCPPayment(ctx, "process", "Example tool", 250000, req.Params.Meta)
if out.Challenge != nil {        // return it as the (isError) tool result
} else if vErr == nil {          // out.Settlement is verified; run your action,
    _ = out.ReceiptMeta          // then attach ReceiptMeta to result._meta
}
// (low-level halves — MCPChallenge / DecodeMetaPayment / MCPSettle /
// EncodeMetaPaymentResponse — remain available for custom servers.)

// MCP client: pay a seller's tool end to end. FetchPaidMCP probes, and on a
// payment-required challenge pays via your MCPPayer and retries with the proof
// in _meta["x402/payment"] — payment never rides in tool arguments.
res, _ := dcr402.FetchPaidMCP(ctx, caller, "process", args, pay)
```

## What a gated request looks like

- No credential, no proof -> `402` carrying `WWW-Authenticate: LSAT ...`,
  `WWW-Authenticate: L402 ...`, and `PAYMENT-REQUIRED` (one fresh invoice
  behind all three; L402 macaroon minted at challenge time).
- `PAYMENT-SIGNATURE` proof -> verification rules 1-8 (preimage, invoice
  decode via `zpay32`, network/destination/amount binding, challenge
  correlation, node-settled check), settled exactly once, answered with
  `PAYMENT-RESPONSE` including the reusable credential in
  `extensions.l402`. Re-presenting a consumed proof returns the original
  settlement idempotently and does **not** re-execute the handler.
- `Authorization: L402|LSAT <macaroon>:<preimage>` -> served on the token
  (payment proofs alongside a valid token are deliberately not consumed);
  broken or expired tokens get `401`, never `402`.

## Packages

| Package | Contents |
|---|---|
| `dcr402` (root) | `Gate`: config, challenge builders, verification rules 1-12, settlement, HTTP middleware (`Require`, `RequireCredits`, `Topup`), MCP helpers |
| `x402` | x402 v2 envelope types + standard-base64 header codec |
| `l402` | L402 macaroons: direct HMAC chain, macaroon-V2 wire format (byte-compatible with `gopkg.in/macaroon.v2`), caveat satisfiers, header grammar |
| `store` | `Store` interface + SQLite (pure Go, WAL) and in-memory implementations: lightning + onchain challenges, consume-once settlements, root keys (revocation = deletion) |
| `ledger` | Credit ledger: atoms-denominated balances keyed by the credential `token_id`; idempotent grants, atomic never-negative charges |
| `dcrlnd` | `InvoiceBackend` + `OnchainBackend` over dcrlnd gRPC |

## Credits - the slow rail funding the fast one

`Gate.Topup` serves a 402 offering **both** methods - a Lightning invoice
and a fresh on-chain deposit address - minted over one `token_id`, so
either rail funds the same account. Settlement grants atoms to the ledger
(idempotently, keyed by payment hash or txid) and returns the account's
L402 credential; on-chain deposits below the required depth answer with the
retryable `insufficient_confirmations` response until mined deep enough.
`Gate.RequireCredits(tool, atoms)` then burns the balance per call with no
payment in the hot path - the remaining balance rides the `Dcr402-Balance`
header, and shortfalls are machine-actionable
(`{"error":"insufficient_credits","shortfallAtoms":...,"topup":...}`).
On-chain payments have no Lightning preimage, so their account credential
is minted over a server-generated one, delivered at settlement - the L402
verification mechanics are identical either way.

## Node requirements

The gate needs the stock **`invoice.macaroon`** only (`invoices:read`,
`invoices:write`, `address:read/write`, `onchain:read`) - deliberately no
`offchain` permissions, so the gate holds no ability to spend. Bake nothing
custom:

```
~/.dcrlnd/data/chain/decred/<network>/invoice.macaroon
```

`Config.PayTo` is the node's identity pubkey (`lncli getinfo` ->
`identity_pubkey`); verification binds every challenge invoice's
destination to it.

## Testing

```
go test ./...
```

covers, among others: byte-exact re-derivation of the L402 vectors,
`gopkg.in/macaroon.v2` wire parity, decode verification of every BOLT11
vector in [`../scheme/test-vectors/invoices.json`](../scheme/test-vectors/invoices.json)
(the golden invoice's preimage relation included), and an end-to-end
httptest flow - challenge -> pay -> proof -> serve+credential -> token access ->
idempotent replay -> tampered/expired credential rejection - against a fake
Lightning backend.
