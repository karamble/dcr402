// Package x402 implements the x402 v2 data structures and HTTP header
// transport as used by the dcr402 scheme. Field names and semantics follow
// scheme/scheme_exact_dcr.md and the x402 v2 specification; the test suite
// pins them to the repository's test vectors.
package x402

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Version is the protocol version this package implements.
const Version = 2

// HTTP transport header names (specs/transports-v2/http.md). HTTP header
// names are case-insensitive; Go's http.Header canonicalizes them.
const (
	HeaderPaymentRequired  = "Payment-Required"
	HeaderPaymentSignature = "Payment-Signature"
	HeaderPaymentResponse  = "Payment-Response"
)

// Asset transfer methods defined by the Decred exact scheme.
const (
	MethodLightning = "lightning"
	MethodOnchain   = "onchain"
)

// SchemeExact is the payment scheme implemented by this library.
const SchemeExact = "exact"

// AssetDCR is the asset symbol for native DCR.
const AssetDCR = "DCR"

// ResourceInfo describes the protected resource (x402 v2 §5.1.3).
type ResourceInfo struct {
	URL         string   `json:"url"`
	Description string   `json:"description,omitempty"`
	MimeType    string   `json:"mimeType,omitempty"`
	ServiceName string   `json:"serviceName,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	IconURL     string   `json:"iconUrl,omitempty"`
}

// Extension is one entry of an extensions map: a payload plus the JSON
// schema that validates it.
type Extension struct {
	Info   json.RawMessage `json:"info"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// PaymentRequirements is one accepts[] entry (x402 v2 §5.1.2).
type PaymentRequirements struct {
	Scheme            string          `json:"scheme"`
	Network           string          `json:"network"`
	Amount            string          `json:"amount"`
	Asset             string          `json:"asset"`
	PayTo             string          `json:"payTo"`
	MaxTimeoutSeconds int             `json:"maxTimeoutSeconds"`
	Extra             json.RawMessage `json:"extra,omitempty"`
}

// LightningExtra is PaymentRequirements.extra for the lightning method.
type LightningExtra struct {
	AssetTransferMethod string `json:"assetTransferMethod"`
	Invoice             string `json:"invoice"`
	PaymentHash         string `json:"paymentHash"`
}

// OnchainExtra is PaymentRequirements.extra for the onchain method.
// Credential is a per-challenge secret (the account macaroon) delivered only
// to the 402 recipient. Because verification requires `accepted` to equal the
// offered entry (scheme rule 1), echoing it binds the settlement to the payer:
// on-chain data carries no payer secret, so only the 402 recipient can produce
// an `accepted` that matches the offered entry.
type OnchainExtra struct {
	AssetTransferMethod string `json:"assetTransferMethod"`
	Confirmations       int    `json:"confirmations,omitempty"`
	Credential          string `json:"credential,omitempty"`
}

// TransferMethod extracts extra.assetTransferMethod without committing to a
// method-specific structure. Returns "" if extra is absent or malformed.
func (pr PaymentRequirements) TransferMethod() string {
	var probe struct {
		Method string `json:"assetTransferMethod"`
	}
	if err := json.Unmarshal(pr.Extra, &probe); err != nil {
		return ""
	}
	return probe.Method
}

// PaymentRequired is the 402 challenge object (x402 v2 §5.1.1).
type PaymentRequired struct {
	X402Version int                   `json:"x402Version"`
	Error       string                `json:"error,omitempty"`
	Resource    ResourceInfo          `json:"resource"`
	Accepts     []PaymentRequirements `json:"accepts"`
	Extensions  map[string]Extension  `json:"extensions,omitempty"`
}

// PaymentPayload is the client's retry envelope (x402 v2 §5.2).
type PaymentPayload struct {
	X402Version int                  `json:"x402Version"`
	Resource    *ResourceInfo        `json:"resource,omitempty"`
	Accepted    PaymentRequirements  `json:"accepted"`
	Payload     json.RawMessage      `json:"payload"`
	Extensions  map[string]Extension `json:"extensions,omitempty"`
}

// LightningPayload is PaymentPayload.payload for the lightning method.
type LightningPayload struct {
	Preimage    string `json:"preimage"`
	PaymentHash string `json:"paymentHash"`
}

// OnchainPayload is PaymentPayload.payload for the onchain method.
type OnchainPayload struct {
	TxID string `json:"txid"`
}

// SettlementResponse reports settlement outcome (x402 v2 §5.4). Transaction
// has no omitempty: failure responses carry it as the empty string.
type SettlementResponse struct {
	Success     bool                 `json:"success"`
	ErrorReason string               `json:"errorReason,omitempty"`
	Payer       string               `json:"payer,omitempty"`
	Transaction string               `json:"transaction"`
	Network     string               `json:"network"`
	Amount      string               `json:"amount,omitempty"`
	Extensions  map[string]Extension `json:"extensions,omitempty"`
}

// VerifyResponse is the facilitator /verify result (x402 v2 facilitator
// API). Field names match the upstream reference implementation
// (x402-foundation/x402/go/types.go). On a rejected payment IsValid is false
// and InvalidReason carries the scheme errorReason. Payer is empty for the
// Decred lightning method: a preimage reveals no payer identity (the invoice
// binds only the payee).
type VerifyResponse struct {
	IsValid        bool   `json:"isValid"`
	InvalidReason  string `json:"invalidReason,omitempty"`
	InvalidMessage string `json:"invalidMessage,omitempty"`
	Payer          string `json:"payer,omitempty"`
}

// SupportedKind is one (scheme, network) pair a facilitator supports (x402 v2
// facilitator API; fields match x402-foundation/x402/go/types). Extra carries
// the Decred exact scheme's assetTransferMethod.
type SupportedKind struct {
	X402Version int             `json:"x402Version"`
	Scheme      string          `json:"scheme"`
	Network     string          `json:"network"`
	Extra       json.RawMessage `json:"extra,omitempty"`
}

// SupportedResponse is the GET /supported body: the kinds a facilitator
// verifies and settles, the protocol extensions it implements, and the CAIP
// family to signer-address map (empty for the Decred lightning method, which
// has no on-chain facilitator signer).
type SupportedResponse struct {
	Kinds      []SupportedKind     `json:"kinds"`
	Extensions []string            `json:"extensions"`
	Signers    map[string][]string `json:"signers"`
}

// EncodeHeader serializes v to UTF-8 JSON and applies the standard base64
// encoding (RFC 4648 §4, with padding) — the x402 v2 header transport.
func EncodeHeader(v any) (string, error) {
	j, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(j), nil
}

// DecodeHeader reverses EncodeHeader into v.
func DecodeHeader(header string, v any) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return fmt.Errorf("x402: header is not standard base64: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("x402: header JSON: %w", err)
	}
	return nil
}

// EqualSemantic reports whether two PaymentRequirements are equal as JSON
// values. Verification rule 1 compares the client's echoed `accepted` entry
// against the offered entry; clients may re-serialize with different key
// order, so equality is semantic, not byte-wise.
func EqualSemantic(a, b PaymentRequirements) bool {
	if a.Scheme != b.Scheme || a.Network != b.Network ||
		a.Amount != b.Amount || a.Asset != b.Asset ||
		a.PayTo != b.PayTo || a.MaxTimeoutSeconds != b.MaxTimeoutSeconds {
		return false
	}
	return jsonValueEqual(a.Extra, b.Extra)
}

func jsonValueEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == 0 && len(b) == 0
	}
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return deepEqualJSON(av, bv)
}

func deepEqualJSON(a, b any) bool {
	switch at := a.(type) {
	case map[string]any:
		bt, ok := b.(map[string]any)
		if !ok || len(at) != len(bt) {
			return false
		}
		for k, v := range at {
			bv, ok := bt[k]
			if !ok || !deepEqualJSON(v, bv) {
				return false
			}
		}
		return true
	case []any:
		bt, ok := b.([]any)
		if !ok || len(at) != len(bt) {
			return false
		}
		for i := range at {
			if !deepEqualJSON(at[i], bt[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// MustRaw marshals v to json.RawMessage, panicking on failure. Intended for
// static values (method extras, extension payloads) built by the library.
func MustRaw(v any) json.RawMessage {
	j, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return j
}
