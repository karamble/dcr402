# Test vectors

Deterministic vectors for every wire artifact in
[`scheme_exact_dcr.md`](../scheme_exact_dcr.md) and
[`l402-dual-envelope.md`](../l402-dual-envelope.md). Verify everything with:

```
python3 tools/check_vectors.py
```

(standard library only - no dependencies).

> **Never fund anything in these files.** All Lightning destinations use the
> published BOLT11 test key (`e126f68f...db734` - public knowledge), and the
> on-chain address is documentation-only.

## Files

| File | Contents |
|---|---|
| `invoices.json` | Decred BOLT11 decode vectors: the golden invoice plus verbatim entries from the dcrlnd `zpay32` test suite, including an amountless invoice that MUST be rejected (verification rule 6). |
| `x402-lightning.json` | `PaymentRequired`, `PaymentPayload`, success/failure `SettlementResponse` for the `lightning` method, each with exact header base64; plus the MCP tool-result form. |
| `x402-onchain.json` | The same set for the `onchain` method (synthetic txid). |
| `l402.json` | L402 minting vectors: identifier bytes, full HMAC chain, macaroon V2 serialization, `WWW-Authenticate` and `Authorization` header lines. |

## Fixed constants

| Constant | Value | Used for |
|---|---|---|
| preimage | `0x11` x 32 | golden invoice, x402 payload, L402 credential |
| payment hash | `02d449a31fbb267c8f352e9968a79e3e5fc95c1bbeaa502fd6454ebde5a4bedc` | = SHA-256(preimage); anchors every file |
| L402 root key | `0x22` x 32 | macaroon HMAC chain |
| L402 token id | `0x33` x 32 | macaroon identifier |
| token expiry | `1893456000` (2030-01-01T00:00:00Z) | `_valid_until` caveat, `extensions.l402.info.validUntil` |
| onchain txid | SHA-256 of the ASCII string `dcr402 onchain vector txid` | synthetic structural vector |

The **golden invoice** (`invoices.json` -> `golden-mainnet-2500u`) is the only
vector whose preimage is known, which is what makes the payload and L402
vectors fully verifiable end-to-end: mainnet, 250,000 atoms (0.0025 DCR),
timestamp `1496314658`, expiry 3600 s, description `dcr402 golden vector`,
signed by the BOLT11 test key. ECDSA signing is RFC 6979 deterministic, so
regeneration is byte-identical.

The remaining `invoices.json` entries are copied verbatim from
`github.com/decred/dcrlnd` `zpay32/invoice_test.go`, along with their expected
decodes.

## Equality rules

- `base64` <-> `jsonText`: **byte-exact** - `base64` is the standard RFC 4648
  encoding (with padding) of `jsonText`'s UTF-8 bytes. This matches the
  reference x402 SDKs (Go `base64.StdEncoding`, TypeScript `btoa`).
- `jsonText`/`base64` <-> `json`: **semantic** - wire JSON has no canonical key
  order; parse and deep-compare.

## Regeneration

1. **Golden invoice** - `tools/gen_golden_invoice.go` (run instructions in
   its header: a scratch module with a `replace` to a dcrlnd checkout).
2. **L402 vectors** - `python3 tools/gen_l402.py` (rewrites `l402.json`).
3. **x402 vectors** - the JSON objects mirror the specification examples;
   after editing any `json` value, recompute the wrapper fields:
   `jsonText = json.dumps(obj, separators=(",", ":"))` and
   `base64 = base64.b64encode(jsonText.encode()).decode()`, then re-run
   `tools/check_vectors.py` until green.

## Serialization ground truth

The macaroon bytes in `l402.json` were validated against
`gopkg.in/macaroon.v2` v2.1.0 on 2026-07-10: `UnmarshalBinary` accepts them
and `MarshalBinary` reproduces them byte-identically (identifier, three
first-party caveats with no verification-id field, signature). See the
serialization errata note in
[`l402-dual-envelope.md`](../l402-dual-envelope.md) section 3 for why the
implementations - not the L402 macaroon specification's V2 table - are the
wire ground truth.
