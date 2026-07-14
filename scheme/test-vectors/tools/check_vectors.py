#!/usr/bin/env python3
"""Re-derive and verify every assertion in the dcr402 scheme test vectors.

Python standard library only. Exits non-zero listing each failed check.
Covers: SHA-256 preimage relations, the full macaroon HMAC chain and V2
serialization, base64/jsonText/json consistency, x402 envelope field rules,
L402 header grammar (including LSAT-before-L402 ordering), amount
arithmetic, network/prefix binding, and cross-file anchoring on the golden
invoice.

Chain lookups and BOLT11 decoding are intentionally out of scope here (Go
territory, M2); invoice-level expectations come from the dcrlnd zpay32 test
suite and the deterministic golden generator.
"""

import base64
import hashlib
import hmac
import json
import os
import re
import sys

TV = os.path.normpath(os.path.join(os.path.dirname(__file__), ".."))

NETWORKS = {
    "bip122:298e5cc3d985bfe7f81dc135f360abe0": "lndcr",
    "bip122:a649dce53918caf422e9c711c858837e": "lntdcr",
    "bip122:6bef82c645999585f7255cb02672921a": "lnsdcr",
}
HEX64 = re.compile(r"^[0-9a-f]{64}$")
AMOUNT = re.compile(r"^[1-9][0-9]*$")
B64 = re.compile(r"^[A-Za-z0-9+/]+={0,2}$")
WWW_AUTH = re.compile(
    r'^WWW-Authenticate: (LSAT|L402) macaroon="([A-Za-z0-9+/]+={0,2})", '
    r'invoice="(ln[a-z0-9]+)"$'
)
AUTHZ = re.compile(
    r"^Authorization: L402 ([A-Za-z0-9+/]+={0,2}(?:,[A-Za-z0-9+/]+={0,2})*)"
    r":([0-9a-f]+)$"
)

failures = []
checks = 0


def check(cond, msg):
    global checks
    checks += 1
    if not cond:
        failures.append(msg)


def load(name):
    with open(os.path.join(TV, name), encoding="utf-8") as f:
        return json.load(f)


def varint(n):
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def packet(tag, data):
    return varint(tag) + varint(len(data)) + data


def serialize_v2(identifier, caveats, sig):
    out = bytearray(b"\x02")
    out += packet(2, identifier)
    out += b"\x00"
    for cav in caveats:
        out += packet(2, cav.encode("utf-8"))
        out += b"\x00"
    out += b"\x00"
    out += packet(6, sig)
    return bytes(out)


# --- l402.json -------------------------------------------------------------

l4 = load("l402.json")
c, d, h = l4["constants"], l4["derived"], l4["headers"]

preimage = bytes.fromhex(c["preimage"])
check(len(preimage) == 32, "l402: preimage not 32 bytes")
payment_hash = hashlib.sha256(preimage).hexdigest()
check(payment_hash == d["paymentHash"], "l402: sha256(preimage) != paymentHash")

ident = bytes.fromhex(d["identifier"])
check(len(ident) == 66, "l402: identifier not 66 bytes")
check(ident[0:2] == (0).to_bytes(2, "big"), "l402: identifier version != 0")
check(ident[2:34].hex() == d["paymentHash"], "l402: identifier[2:34] != paymentHash")
check(ident[34:66].hex() == c["tokenId"], "l402: identifier[34:66] != tokenId")

sig = hmac.new(bytes.fromhex(c["rootKey"]), ident, hashlib.sha256).digest()
chain = [sig]
for cav in c["caveats"]:
    sig = hmac.new(sig, cav.encode("utf-8"), hashlib.sha256).digest()
    chain.append(sig)
check(
    [s.hex() for s in chain] == d["hmacChain"],
    "l402: recomputed HMAC chain differs",
)
check(chain[-1].hex() == d["signature"], "l402: final signature differs")

mac = serialize_v2(ident, c["caveats"], chain[-1])
check(mac.hex() == d["macaroonHex"], "l402: V2 serialization differs")
check(
    base64.b64encode(mac).decode() == d["macaroonBase64"],
    "l402: macaroonBase64 differs",
)

chal = h["challenge"]
check(len(chal) == 2, "l402: expected exactly two challenge lines")
for i, (line, scheme) in enumerate(zip(chal, ["LSAT", "L402"])):
    m = WWW_AUTH.match(line)
    check(m is not None, f"l402: challenge[{i}] fails grammar")
    if m:
        check(m.group(1) == scheme, f"l402: challenge[{i}] scheme order (LSAT first)")
        check(m.group(2) == d["macaroonBase64"], f"l402: challenge[{i}] macaroon differs")
        check(m.group(3) == c["invoice"], f"l402: challenge[{i}] invoice differs")
m = AUTHZ.match(h["authorization"])
check(m is not None, "l402: authorization fails grammar")
if m:
    check(m.group(1) == d["macaroonBase64"], "l402: authorization macaroon differs")
    check(m.group(2) == c["preimage"], "l402: authorization preimage differs")

valid_until = [cav for cav in c["caveats"] if "_valid_until=" in cav]
check(len(valid_until) == 1, "l402: expected one _valid_until caveat")
token_expiry = int(valid_until[0].split("=", 1)[1]) if valid_until else 0

# --- invoices.json ----------------------------------------------------------

inv = load("invoices.json")
golden = None
for v in inv["vectors"]:
    n = v["name"]
    check(v["network"] in NETWORKS, f"invoices/{n}: unknown network")
    check(NETWORKS.get(v["network"]) == v["hrp"], f"invoices/{n}: hrp/network mismatch")
    check(v["invoice"].startswith(v["hrp"]), f"invoices/{n}: invoice prefix != hrp")
    check(HEX64.match(v["paymentHash"]), f"invoices/{n}: bad paymentHash")
    check(
        re.match(r"^0[23][0-9a-f]{64}$", v["destination"]),
        f"invoices/{n}: bad destination pubkey",
    )
    if v.get("mustReject"):
        check(
            "milliAtoms" not in v and "amountAtoms" not in v,
            f"invoices/{n}: mustReject vector carries amounts",
        )
        check(bool(v.get("mustRejectReason")), f"invoices/{n}: missing mustRejectReason")
    else:
        check(
            int(v["milliAtoms"]) == int(v["amountAtoms"]) * 1000,
            f"invoices/{n}: milliAtoms != amountAtoms*1000",
        )
        check(AMOUNT.match(v["amountAtoms"]), f"invoices/{n}: bad amountAtoms")
    if "preimage" in v:
        check(
            hashlib.sha256(bytes.fromhex(v["preimage"])).hexdigest() == v["paymentHash"],
            f"invoices/{n}: sha256(preimage) != paymentHash",
        )
    if n == "golden-mainnet-2500u":
        golden = v
check(golden is not None, "invoices: golden vector missing")
if golden:
    check(golden["invoice"] == c["invoice"], "invoices: golden invoice != l402 invoice")
    check(golden["paymentHash"] == d["paymentHash"], "invoices: golden hash != l402 hash")


# --- x402 envelope files -----------------------------------------------------


def check_requirements_entry(e, where):
    check(e["scheme"] == "exact", f"{where}: scheme != exact")
    check(e["network"] in NETWORKS, f"{where}: unknown network")
    check(AMOUNT.match(e["amount"]), f"{where}: bad amount")
    check(e["asset"] == "DCR", f"{where}: asset != DCR")
    check(bool(e["payTo"]), f"{where}: missing payTo")
    check(isinstance(e["maxTimeoutSeconds"], int), f"{where}: bad maxTimeoutSeconds")
    method = e["extra"].get("assetTransferMethod")
    check(method in ("lightning", "onchain"), f"{where}: bad assetTransferMethod")
    if method == "lightning":
        check(
            e["extra"]["invoice"].startswith(NETWORKS[e["network"]]),
            f"{where}: invoice prefix != network",
        )
        check(HEX64.match(e["extra"]["paymentHash"]), f"{where}: bad extra.paymentHash")
    else:
        conf = e["extra"].get("confirmations")
        check(conf is None or isinstance(conf, int), f"{where}: bad confirmations")


def check_wrapped(entry, fname):
    n = f"{fname}/{entry['name']}"
    text = entry["jsonText"]
    check(
        base64.b64decode(entry["base64"]) == text.encode("utf-8"),
        f"{n}: base64 does not decode to jsonText bytes",
    )
    check(B64.match(entry["base64"]), f"{n}: base64 not standard alphabet/padding")
    check(json.loads(text) == entry["json"], f"{n}: jsonText not semantically equal to json")


for fname in ("x402-lightning.json", "x402-onchain.json"):
    data = load(fname)
    by_name = {e["name"]: e for e in data["vectors"]}
    for entry in data["vectors"]:
        if "base64" in entry:
            check_wrapped(entry, fname)

    req = by_name["payment-required"]["json"]
    check(req["x402Version"] == 2, f"{fname}: x402Version != 2")
    check(bool(req["resource"]["url"]), f"{fname}: missing resource.url")
    check(len(req["accepts"]) >= 1, f"{fname}: empty accepts")
    for i, e in enumerate(req["accepts"]):
        check_requirements_entry(e, f"{fname}/accepts[{i}]")

    pay = by_name["payment-payload"]["json"]
    check(pay["x402Version"] == 2, f"{fname}: payload x402Version != 2")
    check(pay["accepted"] == req["accepts"][0], f"{fname}: accepted != offered entry")

    ok = by_name["settlement-success"]["json"]
    check(ok["success"] is True, f"{fname}: settlement-success not success")
    check(HEX64.match(ok["transaction"]), f"{fname}: bad settlement transaction")
    check(ok["network"] == req["accepts"][0]["network"], f"{fname}: settlement network")

    fail_name = "settlement-failure" if "settlement-failure" in by_name else "settlement-pending"
    fl = by_name[fail_name]["json"]
    check(fl["success"] is False, f"{fname}: {fail_name} not failure")
    check(fl["transaction"] == "", f"{fname}: {fail_name} transaction not empty")
    check(bool(fl["errorReason"]), f"{fname}: {fail_name} missing errorReason")

    if fname == "x402-lightning.json":
        method_entry = req["accepts"][0]
        check(
            method_entry["extra"]["invoice"] == c["invoice"],
            f"{fname}: challenge invoice != golden",
        )
        p = pay["payload"]
        check(
            hashlib.sha256(bytes.fromhex(p["preimage"])).hexdigest()
            == p["paymentHash"]
            == method_entry["extra"]["paymentHash"],
            f"{fname}: preimage/hash relation broken",
        )
        check(
            ok["transaction"] == method_entry["extra"]["paymentHash"],
            f"{fname}: settlement transaction != payment hash",
        )
        check(
            ok["transaction"] != p["preimage"],
            f"{fname}: settlement transaction leaks preimage",
        )
        l402ext = ok["extensions"]["l402"]["info"]
        check(
            l402ext["authorization"] == f"L402 {d['macaroonBase64']}:{c['preimage']}",
            f"{fname}: extensions.l402 authorization differs from l402.json",
        )
        check(
            l402ext["validUntil"] == token_expiry,
            f"{fname}: extensions.l402 validUntil != _valid_until caveat",
        )
        check(fl["errorReason"] == "invalid_exact_decred_payload_preimage_mismatch",
              f"{fname}: unexpected failure errorReason")

        mcp = by_name["mcp-payment-required"]["json"]
        check(mcp["isError"] is True, "mcp: isError != true")
        sc = mcp["structuredContent"]
        check(sc["resource"]["url"].startswith("mcp://tool/"), "mcp: resource.url convention")
        check(
            json.loads(mcp["content"][0]["text"]) == sc,
            "mcp: content[0].text != structuredContent",
        )
        for i, e in enumerate(sc["accepts"]):
            check_requirements_entry(e, f"mcp/accepts[{i}]")
    else:
        check(
            hashlib.sha256(b"dcr402 onchain vector txid").hexdigest()
            == pay["payload"]["txid"],
            f"{fname}: synthetic txid derivation differs",
        )
        check(fl["errorReason"] == "insufficient_confirmations",
              f"{fname}: unexpected pending errorReason")

# --- report ------------------------------------------------------------------

if failures:
    print(f"FAIL: {len(failures)} of {checks} checks failed")
    for f in failures:
        print("  -", f)
    sys.exit(1)
print(f"PASS: all {checks} checks passed")
