# The dcr402 scheme specifications

These documents are the wire protocol for dcr402: the `exact` payment scheme on
Decred, the L402 dual-envelope server behavior, and the test vectors that pin
every artifact.

## Documents

| Document | What it specifies |
|---|---|
| [`scheme_exact_dcr.md`](scheme_exact_dcr.md) | The `exact` payment scheme on Decred for x402 v2: `lightning` + `onchain` transfer methods, challenge/payload/settlement wire formats, verification rules, failure codes, security model. Written to the x402 house style for upstreaming. |
| [`l402-dual-envelope.md`](l402-dual-envelope.md) | dcr402 server behavior: emitting L402/LSAT and x402 challenges from one 402, L402 token minting, credential precedence, the `l402` settlement extension. |
| [`test-vectors/`](test-vectors/) | Deterministic vectors for every wire artifact, with self-contained verification tooling (`python3 test-vectors/tools/check_vectors.py`). |

## Network identifiers (normative constants)

| Network | CAIP-2 | BOLT11 prefix |
|---|---|---|
| Decred mainnet | `bip122:298e5cc3d985bfe7f81dc135f360abe0` | `lndcr` |
| Decred testnet3 | `bip122:a649dce53918caf422e9c711c858837e` | `lntdcr` |
| Decred simnet (dev only) | `bip122:6bef82c645999585f7255cb02672921a` | `lnsdcr` |

## Upstream

`scheme_exact_dcr.md` follows the structure and register of
[`specs/schemes/exact/`](https://github.com/x402-foundation/x402/tree/main/specs/schemes/exact)
in the x402-foundation repository (per-network implementations of the
`exact` scheme; the XRPL document is the closest structural sibling, a
native-asset chain with two `extra.assetTransferMethod` values). It is written
as a copy-ready contribution, `specs/schemes/exact/scheme_exact_dcr.md`, per the
upstream [contribution process](https://github.com/x402-foundation/x402/blob/main/specs/CONTRIBUTING.md).

The L402 annex is dcr402-specific server behavior and is not part of the
upstream submission.

## Verifying the vectors

```
python3 test-vectors/tools/check_vectors.py
```

re-derives every cryptographic assertion (SHA-256 preimage relations, the
full macaroon HMAC chain, base64 round-trips, amount arithmetic) from the
JSON vector files using only the Python standard library. See
[`test-vectors/README.md`](test-vectors/README.md) for the file format and
regeneration instructions.
