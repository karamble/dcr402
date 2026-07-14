#!/usr/bin/env python3
"""Deterministically generate scheme/test-vectors/l402.json.

Python standard library only. Reproduces the L402 macaroon for the golden
invoice from fixed constants: the HMAC-SHA256 signature chain exactly as the
L402 macaroon specification defines it (sig_0 = HMAC(root_key, identifier),
no key-derivation prefix), and the macaroon V2 binary serialization exactly
as the reference implementations (libmacaroons, gopkg.in/macaroon.v2)
produce it — see the errata note in ../../l402-dual-envelope.md §3.

Run from anywhere: writes ../l402.json relative to this file.
"""

import base64
import hashlib
import hmac
import json
import os

# --- Fixed constants (documented in ../README.md) ------------------------

# The golden invoice (tools/gen_golden_invoice.go, deterministic): mainnet,
# 250,000 atoms, preimage 0x11 x 32, published BOLT11 test key. NEVER FUND.
GOLDEN_INVOICE = (
    "lndcr2500u1pvjluezpp5qt2yngclhvn8ere496vk3fu78e0ujhqmh649qt7kg48tmedyhmwq"
    "dpqv33hydpsxgsxwmmvv3jkugrkv43hgmmjxqrrssdztqxz9z9ys3q59ml9270ej0t9wt6244"
    "2s3tzzldns0a6j247qkkqzakysm9yz75xqze4a7r3h7tsys8tcugay7f8sru2l8a7s07srcqc"
    "5t8f7"
)

PREIMAGE = bytes([0x11]) * 32
ROOT_KEY = bytes([0x22]) * 32
TOKEN_ID = bytes([0x33]) * 32
IDENTIFIER_VERSION = 0

CAVEATS = [
    "services=example:0",
    "example_capabilities=read,write",
    "example_valid_until=1893456000",
]

# --- Macaroon V2 binary serialization (libmacaroons / macaroon.v2) --------
# Packet: varint(fieldType) || varint(len) || bytes. EOS: single 0x00.
# Field types: EOS=0, location=1, identifier=2, verificationId=4, signature=6.
# Layout: version byte 0x02; [location] identifier EOS; per caveat:
# [location] cid [vid] EOS (first-party caveats omit vid); EOS ending the
# caveat list; signature. Nothing follows the signature.


def varint(n: int) -> bytes:
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def packet(field_type: int, data: bytes) -> bytes:
    return varint(field_type) + varint(len(data)) + data


def serialize_v2(identifier: bytes, caveats: list[str], sig: bytes) -> bytes:
    out = bytearray(b"\x02")           # version byte
    out += packet(2, identifier)       # header: identifier (location omitted)
    out += b"\x00"                     # end of header
    for cav in caveats:                # first-party caveats: cid only
        out += packet(2, cav.encode("utf-8"))
        out += b"\x00"                 # end of this caveat
    out += b"\x00"                     # end of caveat list
    out += packet(6, sig)              # signature
    return bytes(out)


def main() -> None:
    payment_hash = hashlib.sha256(PREIMAGE).digest()
    identifier = (
        IDENTIFIER_VERSION.to_bytes(2, "big") + payment_hash + TOKEN_ID
    )
    assert len(identifier) == 66

    # HMAC chain per the L402 macaroon specification (direct construction).
    chain = [hmac.new(ROOT_KEY, identifier, hashlib.sha256).digest()]
    for cav in CAVEATS:
        chain.append(
            hmac.new(chain[-1], cav.encode("utf-8"), hashlib.sha256).digest()
        )
    signature = chain[-1]

    macaroon = serialize_v2(identifier, CAVEATS, signature)
    mac_b64 = base64.b64encode(macaroon).decode("ascii")
    preimage_hex = PREIMAGE.hex()

    vectors = {
        "description": (
            "Deterministic L402 minting vectors for the dual envelope "
            "(../l402-dual-envelope.md). All values re-derivable from the "
            "constants; regenerate with tools/gen_l402.py."
        ),
        "constants": {
            "preimage": preimage_hex,
            "rootKey": ROOT_KEY.hex(),
            "tokenId": TOKEN_ID.hex(),
            "identifierVersion": IDENTIFIER_VERSION,
            "caveats": CAVEATS,
            "invoice": GOLDEN_INVOICE,
        },
        "derived": {
            "paymentHash": payment_hash.hex(),
            "identifier": identifier.hex(),
            "hmacChain": [s.hex() for s in chain],
            "signature": signature.hex(),
            "macaroonHex": macaroon.hex(),
            "macaroonBase64": mac_b64,
        },
        "headers": {
            "challenge": [
                f'WWW-Authenticate: LSAT macaroon="{mac_b64}", invoice="{GOLDEN_INVOICE}"',
                f'WWW-Authenticate: L402 macaroon="{mac_b64}", invoice="{GOLDEN_INVOICE}"',
            ],
            "authorization": f"Authorization: L402 {mac_b64}:{preimage_hex}",
        },
    }

    out_path = os.path.join(os.path.dirname(__file__), "..", "l402.json")
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(vectors, f, indent=2)
        f.write("\n")
    print(f"wrote {os.path.normpath(out_path)}")
    print(f"paymentHash   {payment_hash.hex()}")
    print(f"identifier    {identifier.hex()[:24]}… ({len(identifier)} bytes)")
    print(f"signature     {signature.hex()}")
    print(f"macaroon      {len(macaroon)} bytes, base64 {len(mac_b64)} chars")


if __name__ == "__main__":
    main()
