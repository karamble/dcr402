package dcr402

import (
	"context"
	"time"
)

// InvoiceStatus reports a payee node's view of an invoice.
type InvoiceStatus struct {
	// Settled is true once the invoice's HTLCs have irrevocably settled.
	Settled bool
	// AmtPaidMAtoms is the settled amount in milli-atoms.
	AmtPaidMAtoms int64
}

// DepositStatus reports the wallet's view of an on-chain deposit
// transaction relative to one deposit address.
type DepositStatus struct {
	// Found is false while the wallet has not seen the transaction —
	// treated as the retryable pending condition, not a terminal failure.
	Found bool
	// Confirmations is the transaction's current depth (0 = mempool).
	Confirmations int32
	// AmountToAddressAtoms is the exact sum of the transaction's outputs
	// paying the deposit address (verification rule 10).
	AmountToAddressAtoms int64
}

// OnchainBackend abstracts on-chain deposit handling for the onchain
// transfer method: fresh single-use deposit addresses and transaction
// lookups. The dcrlnd implementation covers both with the stock
// invoice.macaroon (address:write + onchain:read).
type OnchainBackend interface {
	// NewDepositAddress returns a fresh, never-before-used address
	// (issuers MUST NOT reuse addresses across challenges — rule 9).
	NewDepositAddress(ctx context.Context) (string, error)
	// LookupDeposit locates txid in the wallet and reports its depth and
	// the exact amount it pays to address.
	LookupDeposit(ctx context.Context, txid, address string) (DepositStatus, error)
}

// InvoiceBackend abstracts the seller-side Lightning node (or a facilitator
// acting for it). The dcrlnd implementation lives in the dcrlnd subpackage
// and requires only the stock invoice.macaroon permissions.
type InvoiceBackend interface {
	// CreateInvoice creates a fresh invoice for exactly amountAtoms atoms
	// with the given memo and expiry, returning the BOLT11 string and its
	// payment hash.
	CreateInvoice(ctx context.Context, amountAtoms int64, memo string, expiry time.Duration) (invoice string, paymentHash [32]byte, err error)
	// LookupInvoice reports the node's settlement state for a payment
	// hash — the "mint of record" check of verification rule 8.
	LookupInvoice(ctx context.Context, paymentHash [32]byte) (InvoiceStatus, error)
}
