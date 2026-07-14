package dcr402

import "fmt"

// Error reasons, exactly as specified in scheme/scheme_exact_dcr.md
// ("Failure Modes"). Scheme-specific codes are namespaced
// invalid_exact_decred_* following the upstream per-scheme convention.
const (
	ReasonInvalidVersion      = "invalid_x402_version"
	ReasonInvalidScheme       = "invalid_scheme"
	ReasonInvalidNetwork      = "invalid_network"
	ReasonInvalidRequirements = "invalid_payment_requirements"
	ReasonInvalidPayload      = "invalid_payload"

	ReasonPreimageMismatch   = "invalid_exact_decred_payload_preimage_mismatch"
	ReasonInvoiceDecode      = "invalid_exact_decred_invoice_decode"
	ReasonInvoiceNetwork     = "invalid_exact_decred_invoice_network_mismatch"
	ReasonInvoiceHash        = "invalid_exact_decred_invoice_hash_mismatch"
	ReasonInvoiceDestination = "invalid_exact_decred_invoice_destination_mismatch"
	ReasonInvoiceAmount      = "invalid_exact_decred_invoice_amount_mismatch"
	ReasonInvoiceExpired     = "invalid_exact_decred_invoice_expired"
	ReasonUnknownPaymentHash = "invalid_exact_decred_unknown_payment_hash"
	ReasonPaymentHashReused  = "invalid_exact_decred_payment_hash_reused"

	ReasonAddressMismatch = "invalid_exact_decred_address_mismatch"
	ReasonAmountMismatch  = "invalid_exact_decred_amount_mismatch"
	ReasonTxidReused      = "invalid_exact_decred_txid_reused"

	ReasonInsufficientConfs = "insufficient_confirmations"
	ReasonUnexpectedVerify  = "unexpected_verify_error"
	ReasonUnexpectedSettle  = "unexpected_settle_error"
)

// VerifyError is a verification failure with its wire errorReason.
type VerifyError struct {
	Reason string
	Detail string
}

func (e *VerifyError) Error() string {
	if e.Detail == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Detail
}

func verifyErrf(reason, format string, args ...any) *VerifyError {
	return &VerifyError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}
