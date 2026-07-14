package dcr402

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"

	"github.com/karamble/dcr402/lib/x402"
)

// ExtensionOfferReceipt is the extensions key for signed offers (in the 402)
// and signed receipts (in the settlement response), per
// specs/extensions/extension-offer-and-receipt.md. dcr402 signs with Ed25519
// (JWS/EdDSA) and identifies the key with a self-resolving did:key kid, so a
// verifier needs no network key lookup.
const ExtensionOfferReceipt = "offer-receipt"

const b58alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58btc encodes bytes in Bitcoin base58 (used for the did:key multibase).
func base58btc(input []byte) string {
	zeros := 0
	for zeros < len(input) && input[zeros] == 0 {
		zeros++
	}
	size := (len(input)-zeros)*138/100 + 1
	buf := make([]byte, size)
	high := size - 1
	for i := zeros; i < len(input); i++ {
		carry := int(input[i])
		j := size - 1
		for ; j > high || carry != 0; j-- {
			carry += 256 * int(buf[j])
			buf[j] = byte(carry % 58)
			carry /= 58
		}
		high = j
	}
	j := 0
	for j < size && buf[j] == 0 {
		j++
	}
	out := make([]byte, 0, zeros+size-j)
	for i := 0; i < zeros; i++ {
		out = append(out, '1')
	}
	for ; j < size; j++ {
		out = append(out, b58alphabet[buf[j]])
	}
	return string(out)
}

// didKeyEd25519 returns the did:key identifier for an Ed25519 public key
// (multicodec 0xed01 + key, base58btc, multibase 'z'). The key is embedded, so
// the DID resolves without a network lookup.
func didKeyEd25519(pub ed25519.PublicKey) string {
	return "did:key:z" + base58btc(append([]byte{0xed, 0x01}, pub...))
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signJWS produces an EdDSA JWS compact serialization over payload with the
// given kid.
func signJWS(key ed25519.PrivateKey, kid string, payload any) string {
	hdr, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": kid})
	pj, _ := json.Marshal(payload)
	signing := b64url(hdr) + "." + b64url(pj)
	sig := ed25519.Sign(key, []byte(signing))
	return signing + "." + b64url(sig)
}

// signedObject is one JWS offer or receipt (payload omitted; it is inside the
// JWS compact string).
type signedObject struct {
	Format      string `json:"format"`
	Signature   string `json:"signature"`
	AcceptIndex *int   `json:"acceptIndex,omitempty"`
}

// offerExtension builds the 402 offer-receipt extension: one signed offer per
// accepts entry.
func (g *Gate) offerExtension(resourceURL string, accepts []x402.PaymentRequirements, validUntilUnix int64) (x402.Extension, bool) {
	if g.cfg.OfferReceiptKey == nil {
		return x402.Extension{}, false
	}
	kid := didKeyEd25519(g.cfg.OfferReceiptKey.Public().(ed25519.PublicKey))
	offers := make([]signedObject, 0, len(accepts))
	for i, a := range accepts {
		idx := i
		payload := map[string]any{
			"version":     1,
			"resourceUrl": resourceURL,
			"scheme":      a.Scheme,
			"network":     a.Network,
			"asset":       a.Asset,
			"payTo":       a.PayTo,
			"amount":      a.Amount,
			"validUntil":  validUntilUnix,
		}
		offers = append(offers, signedObject{
			Format:      "jws",
			Signature:   signJWS(g.cfg.OfferReceiptKey, kid, payload),
			AcceptIndex: &idx,
		})
	}
	return x402.Extension{Info: x402.MustRaw(map[string]any{"kid": kid, "offers": offers})}, true
}

// receiptExtension builds the settlement offer-receipt extension: one signed
// receipt for the completed payment.
func (g *Gate) receiptExtension(resourceURL, network, payer, transaction string, issuedAtUnix int64) (x402.Extension, bool) {
	if g.cfg.OfferReceiptKey == nil {
		return x402.Extension{}, false
	}
	kid := didKeyEd25519(g.cfg.OfferReceiptKey.Public().(ed25519.PublicKey))
	payload := map[string]any{
		"version":     1,
		"network":     network,
		"resourceUrl": resourceURL,
		"payer":       payer,
		"issuedAt":    issuedAtUnix,
		"transaction": transaction,
	}
	receipt := signedObject{Format: "jws", Signature: signJWS(g.cfg.OfferReceiptKey, kid, payload)}
	return x402.Extension{Info: x402.MustRaw(map[string]any{"kid": kid, "receipt": receipt})}, true
}
