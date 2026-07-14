# L402 Dual Envelope

Server behavior for serving one `402 Payment Required` response that
satisfies **both** client populations: x402 v2 agents (header envelope per
[`scheme_exact_dcr.md`](scheme_exact_dcr.md)) and classic
[L402](https://github.com/lightninglabs/L402)/LSAT clients
(`WWW-Authenticate` challenges). Same invoice, same verification, one
settlement record - two (in fact three) envelopes.

This annex is dcr402 server behavior, not part of the x402 scheme itself; it
is normative for `dcr402` (library), `dcr402d` (gateway), and `dcrbazaar`
(facilitator) implementations.

The key words "MUST", "MUST NOT", "SHOULD", "SHOULD NOT", "RECOMMENDED",
"MAY" are to be interpreted as described in
[RFC 2119](https://www.rfc-editor.org/rfc/rfc2119).

## 1. Purpose and Scope

x402 gives agents a machine-actionable payment envelope; L402 is the
established Lightning-native HTTP 402 convention with existing client
tooling. A dcr402-gated resource answers unpaid requests with one response
that either kind of client can satisfy, and accepts payment proof in either
form on the way back in. Nothing about the payment itself differs: one BOLT11
invoice, one preimage, one payment-hash-uniqueness record.

## 2. Challenge Emission

An unpaid request to a `lightning`-priced resource receives `402 Payment
Required` carrying, in this order:

```
WWW-Authenticate: LSAT macaroon="<base64>", invoice="<bolt11>"
WWW-Authenticate: L402 macaroon="<base64>", invoice="<bolt11>"
PAYMENT-REQUIRED: <base64 PaymentRequired>
```

- Both `WWW-Authenticate` challenges MUST be sent, `LSAT` first - the L402
  specification's own compatibility rule (older clients match on the first
  recognized scheme keyword). Values are identical between the two lines.
- Header value grammar is exactly
  [L402 protocol specification section 5.3](https://github.com/lightninglabs/L402/blob/master/protocol-specification.md):
  the macaroon is standard base64 (RFC 4648, with padding) in a quoted
  string; the invoice is the BOLT11 string in a quoted string.
- **One challenge, three views**: the macaroon identifier's payment hash,
  the invoice's `p` field, and the x402 `accepts` entry's
  `extra.paymentHash` MUST all be equal, and the invoice strings MUST be
  identical across envelopes. A response failing this is malformed.
- Response bodies remain free; servers SHOULD include a short human-readable
  explanation for browsers.

`onchain`-priced resources emit the x402 envelope only - L402 has no
on-chain challenge form.

## 3. Token Minting

The macaroon in the challenge is the reusable credential the client holds
after paying ("pay once, access N times within TTL"). Minting follows the
[L402 macaroon specification](https://github.com/lightninglabs/L402/blob/master/macaroon-spec.md)
exactly:

- **Identifier** (66 bytes): `version` (2-byte big-endian uint16, currently
  `0`) || `payment_hash` (32 bytes) || `token_id` (32 cryptographically random
  bytes).
- **Root key**: 32 cryptographically random bytes, fresh per macaroon, never
  disclosed; stored server-side keyed by `SHA-256(identifier)`.
- **Signature chain**: `sig_0 = HMAC-SHA256(root_key, identifier)`, then
  `sig_i = HMAC-SHA256(sig_{i-1}, caveat_i)` over each caveat's UTF-8 bytes;
  the final `sig_n` is the macaroon signature.
- **Serialization**: macaroon V2 binary format **as produced by the reference
  implementations** (libmacaroons, gopkg.in/macaroon.v2): version byte `0x02`;
  header = optional location (field 1), identifier (field 2), EOS; each
  first-party caveat = caveat identifier (field 2), EOS - the verification-id
  field (4) is omitted entirely for first-party caveats; one further EOS
  terminating the caveat list; signature (field 6). Every field is
  `varint(type) || varint(length) || bytes`; EOS is a single `0x00`; nothing
  follows the signature. Standard base64 (with padding) in headers.
- **Initial caveats**, in order: `services=<service>:<tier>`, then optional
  `<service>_capabilities=<cap>[,<cap>...]`, then
  `<service>_valid_until=<unix timestamp>`. The `_valid_until` timestamp is
  the **token TTL** - a service-policy lifetime independent of (and
  typically much longer than) the challenge's `maxTimeoutSeconds`.
- **Location**: SHOULD be the service hostname, or omitted. (aperture
  historically sets the literal `lsat`; clients MUST NOT interpret the
  location field.)
- **Mint at challenge time**: the macaroon is created and its root key
  stored when the 402 is emitted, before any payment - the buyer holds the
  durable credential first, then pays. Unpaid macaroons become
  dead weight and MAY be garbage-collected after invoice expiry.

> **Interoperability note - HMAC chain construction.** This annex and its
> test vectors follow the L402 macaroon specification as written:
> `sig_0 = HMAC(root_key, identifier)` directly. General-purpose macaroon
> libraries (gopkg.in/macaroon.v2, libmacaroons, pymacaroons) prepend a
> fixed key-derivation step (`HMAC("macaroons-key-generator", root_key)`)
> before the same chain. The two produce different signatures for the same
> root key. This is a **minter-internal** choice - clients never verify
> signatures, they only carry macaroons opaquely and attenuate by extending
> the chain - but a single deployment MUST pick one construction and use it
> for both minting and verification. dcr402 implementations use the direct
> construction, matching the specification text and the vectors in
> [`test-vectors/l402.json`](test-vectors/l402.json).
>
> **Errata - V2 serialization table.** The macaroon specification's
> "V2 Structure" table disagrees with the reference implementations it cites
> (libmacaroons `doc/format.txt`, gopkg.in/macaroon.v2) on the field tags:
> it shows the caveat identifier as tag `0x01`, an explicit empty
> verification-id, location tags `0x05`/`0x06`, a trailing EOS after the
> signature, and no caveat-list terminator. Existing L402 tooling parses the
> *implementations'* format. For wire compatibility, dcr402 and these test
> vectors serialize exactly as the implementations do (see the
> Serialization bullet above).

## 4. Authorization Acceptance

Clients holding a credential present:

```
Authorization: L402 <base64 macaroon>:<hex preimage>
```

Servers MUST:

- accept both `L402` and `LSAT` scheme keywords, case-insensitively;
- parse per L402 section 5.3 (comma-separated base64 macaroons before the single
  colon - attenuated macaroon stacks - hex preimage after it);
- verify the signature chain by recomputing it from the stored root key
  (lookup by `SHA-256(identifier)`; a missing root key means never-minted or
  revoked -> reject);
- verify `SHA-256(preimage) == identifier.payment_hash`;
- evaluate caveats with registered satisfiers (`services`,
  `<service>_capabilities`, `*_valid_until`), enforcing that repeated
  conditions are increasingly restrictive; **unknown caveat conditions MUST
  be skipped**, not rejected;
- use constant-time comparison for signatures and preimage hashes.

**Status semantics:**

| Credential state | Response |
|---|---|
| Valid | Serve the resource. |
| Present but invalid or expired | `401 Unauthorized` (with fresh `WWW-Authenticate` challenges) |
| Absent | `402 Payment Required` (full dual challenge, section 2) |

A broken token is an authorization failure, not a payment demand: `401`,
never `402`.

## 5. Precedence and Coexistence

A request may carry an L402 `Authorization` credential, an x402
`PAYMENT-SIGNATURE` proof, both, or neither:

| `Authorization` | `PAYMENT-SIGNATURE` | Server behavior |
|---|---|---|
| valid | absent | Serve. No payment consumed. |
| valid | present | **Token wins.** Serve on the token; the payment proof MUST NOT be consumed or recorded - protects eager clients from double-spending a proof alongside a live credential. |
| invalid/expired | valid proof | x402 path: verify per the scheme, record settlement, mint and deliver a fresh credential (section 6). Response includes `PAYMENT-RESPONSE`. |
| invalid/expired | absent/invalid | `401` (section 4). |
| absent | valid proof | x402 path, as above. |
| absent | invalid proof | `402` + `PAYMENT-RESPONSE` `{success:false, errorReason:...}` per the scheme. |
| absent | absent | `402` full dual challenge (section 2). |

Rule of thumb, stated normatively: **the response envelope follows the
request envelope** - L402 requests get L402 semantics (`401`/`WWW-Authenticate`),
x402 requests get x402 semantics (`PAYMENT-RESPONSE`), and the dual challenge
appears only when the client has shown neither.

By this rule a structurally undecodable `PAYMENT-SIGNATURE` is still an x402
request, so it is answered `402` + `PAYMENT-RESPONSE` with
`errorReason: invalid_payload` (the scheme's diagnostic code) rather than the
bare `400` that x402's HTTP transport table maps malformed payloads to: the
scheme response carries more detail for the client.

## 6. Credential Delivery to x402-Native Clients

An x402 client that paid via `PAYMENT-SIGNATURE` typically never stored the
challenge macaroon (it read only the `PAYMENT-REQUIRED` header). To give it
the same pay-once-access-N-times capability, dcr402 defines the `l402`
extension:

**Advertisement** - in `PaymentRequired.extensions`:

```json
{
  "l402": {
    "info": { "tokenTtlSeconds": 2592000 },
    "schema": {
      "$schema": "https://json-schema.org/draft/2020-12/schema",
      "type": "object",
      "properties": { "tokenTtlSeconds": { "type": "integer", "minimum": 0 } },
      "required": ["tokenTtlSeconds"]
    }
  }
}
```

**Delivery** - in `SettlementResponse.extensions` on success:

```json
{
  "l402": {
    "info": {
      "authorization": "L402 <base64 macaroon>:<hex preimage>",
      "validUntil": 1893456000
    },
    "schema": {
      "$schema": "https://json-schema.org/draft/2020-12/schema",
      "type": "object",
      "properties": {
        "authorization": { "type": "string" },
        "validUntil": { "type": "integer" }
      },
      "required": ["authorization"]
    }
  }
}
```

`info.authorization` is a complete, ready-to-send `Authorization` header
value. Because settlement responses ride in `_meta["x402/payment-response"]`
on the MCP transport, payable MCP tools deliver the credential through the
same mechanism with no extra plumbing. Clients SHOULD store it and present
it on subsequent requests (section 4), skipping payment until expiry.

The extension is OPTIONAL; servers not implementing the dual envelope simply
omit it.

## 7. Tokens and Credit Accounts

- The identifier's `token_id` is a stable account key: services running
  credit ledgers SHOULD key accounts by `token_id`, which survives macaroon
  rotation.
- A `credit_account=<id>` first-party caveat MAY bind a macaroon to a ledger
  account explicitly.
- **Tier upgrade**: revoke the old macaroon (delete its root key) and mint a
  fresh one carrying the *same* `token_id` with new caveats.
- **Revocation** is root-key deletion; it is immediate and unconditional
  (signature verification becomes impossible).

## 8. Security Considerations

- Macaroons and preimages are bearer credentials: servers MUST only accept
  them over TLS, and clients MUST NOT log complete `Authorization` values.
- Clients MAY attenuate before delegating (append caveats - e.g. a tighter
  `_valid_until` - without server involvement); servers enforce
  increasing restrictiveness per condition (section 4).
- Settlement records and root keys MUST be retained at least as long as any
  live credential referencing them (`_valid_until` horizon); deleting a root
  key is revocation (section 7).
- The scheme's payment-hash uniqueness rule and token authorization never
  conflict: the x402 proof path consumes the hash exactly once (minting the
  credential), and all subsequent access rides the token path, which
  consumes nothing.

## 9. Test Vectors

Deterministic minting vectors - identifier bytes, full HMAC chain, macaroon
V2 serialization, and complete header lines - are in
[`test-vectors/l402.json`](test-vectors/l402.json), generated by
[`test-vectors/tools/gen_l402.py`](test-vectors/tools/gen_l402.py) and
re-verified by [`test-vectors/tools/check_vectors.py`](test-vectors/tools/check_vectors.py).
