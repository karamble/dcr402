# Example: satisfy a 402 by hand (any language)

You do not need Go, or any dcr402 code, to pay a dcr402-gated resource. A
402 challenge is plain HTTP plus a BOLT11 invoice your own Lightning node can
pay. This walkthrough uses curl, jq, and lncli, but the same three steps
work from any HTTP client and any dcrlnd.

A gated resource answers an unpaid request with a 402 carrying two challenge
forms at once (the dual envelope). Pick whichever your tooling handles more
easily: the x402 headers or the classic L402 `WWW-Authenticate` line.

## Prerequisites

- curl and jq.
- A dcrlnd node you can pay from (`lncli` in the examples), with a channel
  that can route to the seller.

## Path A: x402 (header-based)

### 1. Get the challenge

```
curl -si https://api.example.com/paid | tee resp.txt
```

The `PAYMENT-REQUIRED` response header is base64 of a JSON object. Decode it
and read the first `accepts[]` entry:

```
grep -i '^payment-required:' resp.txt | cut -d' ' -f2 | base64 -d | jq .
```

```json
{
  "x402Version": 2,
  "accepts": [
    {
      "scheme": "exact",
      "network": "bip122:298e5cc3d985bfe7f81dc135f360abe0",
      "amount": "250000",
      "asset": "DCR",
      "payTo": "03e7156a...d9ad",
      "maxTimeoutSeconds": 60,
      "extra": {
        "assetTransferMethod": "lightning",
        "invoice": "lndcr2500u1p...",
        "paymentHash": "02d449a3...bedc"
      }
    }
  ]
}
```

Use the entry whose `extra.assetTransferMethod` is `lightning`. `amount` is
in atoms (1 DCR = 1e8 atoms); the invoice encodes the same value in
milli-atoms.

### 2. Pay the invoice

```
lncli payinvoice lndcr2500u1p... --json | jq -r '.payment_preimage'
# -> 1111111111111111111111111111111111111111111111111111111111111111
```

The 32-byte preimage (hex) is your proof of payment.

### 3. Retry with the proof

Build a `PaymentPayload`: `x402Version` 2, `accepted` set to the exact
`accepts[]` entry you chose (copied verbatim, invoice and all), and `payload`
holding the preimage and payment hash. Base64 the JSON and send it as the
`PAYMENT-SIGNATURE` header:

```
PAYLOAD=$(jq -nc \
  --argjson entry "$(grep -i '^payment-required:' resp.txt | cut -d' ' -f2 | base64 -d | jq '.accepts[0]')" \
  --arg pre 1111...1111 \
  --arg hash 02d449a3...bedc \
  '{x402Version:2, accepted:$entry, payload:{preimage:$pre, paymentHash:$hash}}')

curl -s https://api.example.com/paid \
  -H "PAYMENT-SIGNATURE: $(printf %s "$PAYLOAD" | base64 -w0)"
```

You get the response body plus a `PAYMENT-RESPONSE` header. Decode it to find
a reusable credential at `extensions.l402.info.authorization`; send it as an
`Authorization` header on later requests and skip payment until it expires.

## Path B: classic L402 (Authorization-based)

Some clients prefer the classic form. The same 402 carries:

```
Www-Authenticate: LSAT macaroon="<base64>", invoice="lndcr2500u1p..."
Www-Authenticate: L402 macaroon="<base64>", invoice="lndcr2500u1p..."
```

Pay the invoice as above, then retry with the macaroon and the hex preimage
joined by a colon:

```
curl -s https://api.example.com/paid \
  -H 'Authorization: L402 <base64 macaroon>:1111...1111'
```

## Notes

- Send credentials and preimages over TLS only; they are bearer secrets.
- The seller consumes a payment hash once. Re-presenting the same proof
  returns the original settlement; it does not pay again.
- The full wire specification is `../../../scheme/scheme_exact_dcr.md`, and
  the L402 header grammar is `../../../scheme/l402-dual-envelope.md`.

## End-to-end example

`../../../examples/simnet/gateway-e2e/main.go` performs this x402 loop against
a dcr402d gateway, in Go, without the dcr402 client library.
