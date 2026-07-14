package dcr402

import (
	"encoding/json"
	"regexp"

	"github.com/karamble/dcr402/lib/x402"
)

// ExtensionPaymentID is the extensions key for the payment-identifier
// idempotency extension (specs/extensions/payment_identifier.md). dcr402's
// per-call proofs are already single-use (consume-once by payment hash / txid),
// so an echoed id composes with that native idempotency; the extension lets a
// client that keys on the id interoperate.
const ExtensionPaymentID = "payment-identifier"

var paymentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

var paymentIDSchema = json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"required":{"type":"boolean"},"id":{"type":"string","minLength":16,"maxLength":128,"pattern":"^[A-Za-z0-9_-]+$"}}}`)

// PaymentIDConfig enables and configures the payment-identifier extension.
type PaymentIDConfig struct {
	// Enabled advertises the extension in the 402 and honors an echoed id.
	Enabled bool
	// Required rejects a retry that omits the id with 400.
	Required bool
}

func buildPaymentIDAdvert(required bool) x402.Extension {
	return x402.Extension{
		Info:   x402.MustRaw(map[string]bool{"required": required}),
		Schema: paymentIDSchema,
	}
}

// extractPaymentID reads a client-echoed payment-identifier id from a payload.
func extractPaymentID(pp x402.PaymentPayload) (id string, present bool) {
	ext, ok := pp.Extensions[ExtensionPaymentID]
	if !ok {
		return "", false
	}
	var info struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(ext.Info, &info) != nil || info.ID == "" {
		return "", false
	}
	return info.ID, true
}

// validPaymentID reports whether id matches the extension's format (16-128
// chars of [A-Za-z0-9_-]).
func validPaymentID(id string) bool {
	return paymentIDPattern.MatchString(id)
}
