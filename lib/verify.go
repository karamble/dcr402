package dcr402

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/decred/dcrlnd/zpay32"

	"github.com/karamble/dcr402/lib/l402"
	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

// settleOutcome is everything one settlement produces: the wire response
// plus the ledger hooks (Topup grants credits from these).
type settleOutcome struct {
	Response    x402.SettlementResponse
	Already     bool
	AccountID   string // hex token_id of the challenge credential
	Rail        string
	ExternalRef string
	AmountAtoms int64
	Receipt     []byte
}

// CheckEnvelope applies the method-independent half of verification rule 1
// (x402Version, scheme, and network) against one expected network. Both the
// embedded gate and a facilitator use it; a facilitator that supports several
// networks resolves net from accepted.network before calling.
func CheckEnvelope(pp x402.PaymentPayload, net Network) *VerifyError {
	if pp.X402Version != x402.Version {
		return verifyErrf(ReasonInvalidVersion, "x402Version %d", pp.X402Version)
	}
	if pp.Accepted.Scheme != x402.SchemeExact {
		return verifyErrf(ReasonInvalidScheme, "scheme %q", pp.Accepted.Scheme)
	}
	if pp.Accepted.Network != net.CAIP2 {
		return verifyErrf(ReasonInvalidNetwork, "network %q", pp.Accepted.Network)
	}
	return nil
}

// envelopeChecks applies rule 1's method-independent half for this gate's
// configured network.
func (g *Gate) envelopeChecks(pp x402.PaymentPayload) *VerifyError {
	return CheckEnvelope(pp, g.cfg.Network)
}

// VerifyLightningProof applies the stateless cryptographic checks of the
// lightning method that a facilitator /verify and the embedded gate share:
// rule 1's equality half (accepted equals the offered requirements), rule 3
// (preimage), rule 4 (invoice decode and network binding), rule 5
// (destination), and rule 6 (amount). When now is non-zero it also applies
// rule 7 against the invoice's own embedded expiry (creation timestamp plus
// its x field); the embedded gate passes the zero Time because it enforces
// liveness against its challenge store instead.
//
// It performs no challenge correlation, replay accounting, or mint-of-record
// node lookup (rule 8): those belong to the issuer. offered is the
// PaymentRequirements the resource server offered - the facilitator receives
// it in the request, the gate loads it from its challenge store. The parsed
// lightning payload is returned for the caller's receipt and credential.
func VerifyLightningProof(pp x402.PaymentPayload, offered x402.PaymentRequirements, net Network, now time.Time) (x402.LightningPayload, *VerifyError) {
	var pay x402.LightningPayload
	acc := pp.Accepted

	var extra x402.LightningExtra
	if err := json.Unmarshal(acc.Extra, &extra); err != nil ||
		extra.Invoice == "" || extra.PaymentHash == "" {
		return pay, verifyErrf(ReasonInvalidPayload, "malformed lightning extra")
	}
	if err := json.Unmarshal(pp.Payload, &pay); err != nil ||
		pay.Preimage == "" || pay.PaymentHash == "" {
		return pay, verifyErrf(ReasonInvalidPayload, "malformed lightning payload")
	}
	claimedHash, err := hex.DecodeString(extra.PaymentHash)
	if err != nil || len(claimedHash) != 32 {
		return pay, verifyErrf(ReasonInvalidPayload, "paymentHash is not 32 hex bytes")
	}

	// Rule 3 - preimage.
	preimage, err := hex.DecodeString(pay.Preimage)
	if err != nil || len(preimage) != 32 {
		return pay, verifyErrf(ReasonInvalidPayload, "preimage is not 32 hex bytes")
	}
	sum := sha256.Sum256(preimage)
	if !strings.EqualFold(pay.PaymentHash, extra.PaymentHash) ||
		!hmac.Equal(sum[:], claimedHash) {
		return pay, verifyErrf(ReasonPreimageMismatch,
			"sha256(preimage) does not match payment hash")
	}

	// Rule 1 (equality half) - accepted must echo the offered entry.
	if !x402.EqualSemantic(acc, offered) {
		return pay, verifyErrf(ReasonInvalidRequirements,
			"accepted does not match the offered entry")
	}

	// Rule 4 - invoice decode and network binding.
	if !strings.HasPrefix(extra.Invoice, net.HRP) {
		return pay, verifyErrf(ReasonInvoiceNetwork,
			"invoice prefix does not match %s", net.CAIP2)
	}
	inv, err := zpay32.Decode(extra.Invoice, net.Params)
	if err != nil {
		return pay, verifyErrf(ReasonInvoiceDecode, "%v", err)
	}
	if inv.PaymentHash == nil || !hmac.Equal(inv.PaymentHash[:], claimedHash) {
		return pay, verifyErrf(ReasonInvoiceHash,
			"invoice p field differs from extra.paymentHash")
	}

	// Rule 5 - destination binding.
	if inv.Destination == nil {
		return pay, verifyErrf(ReasonInvoiceDestination, "invoice has no destination")
	}
	dest := hex.EncodeToString(inv.Destination.SerializeCompressed())
	if !strings.EqualFold(dest, acc.PayTo) {
		return pay, verifyErrf(ReasonInvoiceDestination,
			"invoice destination %s != payTo", dest)
	}

	// Rule 6 - amount equality in milli-atoms.
	amountAtoms, err := strconv.ParseInt(acc.Amount, 10, 64)
	if err != nil || amountAtoms <= 0 {
		return pay, verifyErrf(ReasonInvalidRequirements, "amount %q", acc.Amount)
	}
	if inv.MilliAt == nil {
		return pay, verifyErrf(ReasonInvoiceAmount, "invoice carries no amount")
	}
	if int64(*inv.MilliAt) != amountAtoms*1000 {
		return pay, verifyErrf(ReasonInvoiceAmount,
			"invoice m-atoms %d != amount %d x 1000", int64(*inv.MilliAt), amountAtoms)
	}

	// Rule 7 - invoice expiry, only when the caller supplies a clock. A
	// Lightning node never settles an expired invoice, so a stateless verifier
	// still refuses stale re-presentations by the invoice's own timestamp.
	if !now.IsZero() {
		expiresAt := inv.Timestamp.Add(inv.Expiry())
		if now.After(expiresAt) {
			return pay, verifyErrf(ReasonInvoiceExpired,
				"invoice expired at %s", expiresAt.UTC().Format(time.RFC3339))
		}
	}
	return pay, nil
}

// verifyLightning applies the lightning method for the embedded gate: the
// shared cryptographic proof (VerifyLightningProof) wrapped in the issuer's
// rule 8 duties - challenge correlation and liveness against the gate's store,
// then the mint-of-record settlement check against its node.
func (g *Gate) verifyLightning(ctx context.Context, pp x402.PaymentPayload) (store.Challenge, x402.LightningPayload, *VerifyError) {
	var (
		none store.Challenge
		pay  x402.LightningPayload
	)

	// Parse the claimed payment hash to key the challenge lookup.
	var extra x402.LightningExtra
	if err := json.Unmarshal(pp.Accepted.Extra, &extra); err != nil || extra.PaymentHash == "" {
		return none, pay, verifyErrf(ReasonInvalidPayload, "malformed lightning extra")
	}
	claimedHash, err := hex.DecodeString(extra.PaymentHash)
	if err != nil || len(claimedHash) != 32 {
		return none, pay, verifyErrf(ReasonInvalidPayload, "paymentHash is not 32 hex bytes")
	}
	var paymentHash [32]byte
	copy(paymentHash[:], claimedHash)

	// Rule 8 (correlation half) - the challenge must be ours.
	ch, err := g.cfg.Store.GetChallenge(ctx, paymentHash)
	if errors.Is(err, store.ErrNotFound) {
		return none, pay, verifyErrf(ReasonUnknownPaymentHash, "no challenge record")
	}
	if err != nil {
		return none, pay, verifyErrf(ReasonUnexpectedVerify, "challenge lookup: %v", err)
	}

	// Rule 7 - challenge liveness against the gate's own clock (not the
	// invoice's embedded expiry; VerifyLightningProof is called with the zero
	// Time so it skips that check).
	if g.now().After(ch.ExpiresAt) {
		// An already-consumed hash past its window is a reuse; an
		// unconsumed one is a plain expiry.
		if settled, _ := g.cfg.Store.Settled(ctx, paymentHash); settled {
			return none, pay, verifyErrf(ReasonPaymentHashReused, "payment hash already consumed")
		}
		return none, pay, verifyErrf(ReasonInvoiceExpired, "challenge expired")
	}

	var offered x402.PaymentRequirements
	if err := json.Unmarshal(ch.Requirements, &offered); err != nil {
		return none, pay, verifyErrf(ReasonUnexpectedVerify, "stored requirements: %v", err)
	}

	// Rules 1 (equality), 3, 4, 5, 6 - shared with the facilitator.
	pay, ve := VerifyLightningProof(pp, offered, g.cfg.Network, time.Time{})
	if ve != nil {
		return none, pay, ve
	}

	// Rule 8 (mint-of-record half) - confirm the node actually settled.
	st, err := g.cfg.Backend.LookupInvoice(ctx, paymentHash)
	if err != nil {
		return none, pay, verifyErrf(ReasonUnexpectedVerify, "invoice lookup: %v", err)
	}
	if !st.Settled {
		return none, pay, verifyErrf(ReasonUnexpectedVerify,
			"valid preimage but invoice not settled on the payee node")
	}
	return ch, pay, nil
}

// verifyOnchain applies verification rules 9–12 (onchain method) and
// returns the matching challenge record plus the parsed payload. A valid
// transaction below the required depth (or not yet visible to the wallet)
// fails with the retryable ReasonInsufficientConfs.
func (g *Gate) verifyOnchain(ctx context.Context, pp x402.PaymentPayload) (store.OnchainChallenge, x402.OnchainPayload, *VerifyError) {
	var (
		none store.OnchainChallenge
		pay  x402.OnchainPayload
	)
	acc := pp.Accepted

	var extra x402.OnchainExtra
	if err := json.Unmarshal(acc.Extra, &extra); err != nil {
		return none, pay, verifyErrf(ReasonInvalidPayload, "malformed onchain extra")
	}
	if err := json.Unmarshal(pp.Payload, &pay); err != nil || pay.TxID == "" {
		return none, pay, verifyErrf(ReasonInvalidPayload, "malformed onchain payload")
	}
	if raw, err := hex.DecodeString(pay.TxID); err != nil || len(raw) != 32 {
		return none, pay, verifyErrf(ReasonInvalidPayload, "txid is not 32 hex bytes")
	}

	// Rules 1 (equality) + 9 (address correlation): the deposit address is
	// the challenge key — an unknown payTo means the entry was never
	// offered.
	oc, err := g.cfg.Store.GetOnchainChallenge(ctx, acc.PayTo)
	if errors.Is(err, store.ErrNotFound) {
		return none, pay, verifyErrf(ReasonInvalidRequirements,
			"no challenge was issued for this payTo")
	}
	if err != nil {
		return none, pay, verifyErrf(ReasonUnexpectedVerify, "challenge lookup: %v", err)
	}
	var offered x402.PaymentRequirements
	if err := json.Unmarshal(oc.Requirements, &offered); err != nil {
		return none, pay, verifyErrf(ReasonUnexpectedVerify, "stored requirements: %v", err)
	}
	// Rule 1 equality binds the settlement to the party that received the
	// 402: the onchain offered extra carries a per-challenge `credential`
	// secret (the account macaroon, delivered only to the 402 recipient over
	// TLS). On-chain data carries no payer secret, so only the 402 recipient
	// can echo an `accepted` that equals the offered entry; this check rejects
	// any that does not.
	if !x402.EqualSemantic(pp.Accepted, offered) {
		return none, pay, verifyErrf(ReasonInvalidRequirements,
			"accepted does not match the offered entry")
	}

	if g.cfg.Onchain == nil {
		return none, pay, verifyErrf(ReasonUnexpectedVerify,
			"gate has no onchain backend configured")
	}
	dep, err := g.cfg.Onchain.LookupDeposit(ctx, pay.TxID, oc.Address)
	if err != nil {
		return none, pay, verifyErrf(ReasonUnexpectedVerify, "deposit lookup: %v", err)
	}
	if !dep.Found {
		// No transaction yet. Only here does the challenge TTL apply: an
		// expired challenge with no deposit is dead. Otherwise this is the
		// retryable pending condition, never a consumed proof. A deposit that
		// IS found is never stranded on the TTL (the funds are committed
		// on-chain), so the depth check below can wait as long as it needs.
		if g.now().After(oc.ExpiresAt) {
			return none, pay, verifyErrf(ReasonInvoiceExpired,
				"challenge expired with no deposit")
		}
		return none, pay, verifyErrf(ReasonInsufficientConfs,
			"transaction not (yet) known to the wallet")
	}

	// Rule 9 — the transaction must pay the deposit address at all.
	if dep.AmountToAddressAtoms == 0 {
		return none, pay, verifyErrf(ReasonAddressMismatch,
			"transaction pays no output to payTo")
	}
	// Rule 10 — exact amount.
	if dep.AmountToAddressAtoms != oc.AmountAtoms {
		return none, pay, verifyErrf(ReasonAmountMismatch,
			"outputs to payTo sum to %d atoms, want %d",
			dep.AmountToAddressAtoms, oc.AmountAtoms)
	}
	// Rule 11 — confirmation depth (re-checked at settle by design: this
	// path runs on every presentation).
	if dep.Confirmations < oc.Confirmations {
		return none, pay, verifyErrf(ReasonInsufficientConfs,
			"%d of %d confirmations", dep.Confirmations, oc.Confirmations)
	}
	return oc, pay, nil
}

// Settle verifies a PaymentPayload and records its settlement atomically.
// requiredAtoms binds the proof to the resource's price (0 = unpriced, e.g. a
// buyer-chosen top-up): a challenge minted for a cheaper endpoint is rejected,
// so a proof for one route cannot unlock a more expensive one. See settleFull.
func (g *Gate) Settle(ctx context.Context, pp x402.PaymentPayload, requiredAtoms int64) (x402.SettlementResponse, bool, *VerifyError) {
	out, vErr := g.settleFull(ctx, pp, requiredAtoms)
	return out.Response, out.Already, vErr
}

// settleFull verifies and settles, returning the ledger hooks alongside the
// wire response. On success the settlement is recorded exactly once
// (rules 8/12); re-presentations return the stored response with
// Already=true and MUST NOT re-execute the paid operation.
func (g *Gate) settleFull(ctx context.Context, pp x402.PaymentPayload, requiredAtoms int64) (settleOutcome, *VerifyError) {
	fail := func(ve *VerifyError) (settleOutcome, *VerifyError) {
		return settleOutcome{Response: g.failureResponse(ve)}, ve
	}
	if ve := g.envelopeChecks(pp); ve != nil {
		return fail(ve)
	}

	var (
		out       settleOutcome
		settleKey [32]byte
		credAuth  string
		credUntil int64
		credOK    bool
	)
	now := g.now()

	switch pp.Accepted.TransferMethod() {
	case x402.MethodLightning:
		ch, pay, ve := g.verifyLightning(ctx, pp)
		if ve != nil {
			return fail(ve)
		}
		settleKey = ch.PaymentHash
		out.Rail = x402.MethodLightning
		out.AmountAtoms = ch.AmountAtoms
		out.ExternalRef = "ln:" + hex.EncodeToString(ch.PaymentHash[:])
		out.Response = x402.SettlementResponse{
			Success:     true,
			Transaction: hex.EncodeToString(ch.PaymentHash[:]),
			Network:     g.cfg.Network.CAIP2,
			Amount:      strconv.FormatInt(ch.AmountAtoms, 10),
		}
		out.Receipt = x402.MustRaw(map[string]any{
			"invoice":   ch.Invoice,
			"preimage":  pay.Preimage,
			"settledAt": now.UTC().Format(time.RFC3339),
			"amount":    strconv.FormatInt(ch.AmountAtoms, 10),
		})
		acct, aerr := accountFromMacaroon(ch.MacaroonB64)
		if aerr != nil {
			return fail(verifyErrf(ReasonUnexpectedSettle, "derive account id: %v", aerr))
		}
		out.AccountID = acct
		credAuth, credUntil, credOK = credentialFor(ch.MacaroonB64, pay.Preimage, g.cfg.Service)

	case x402.MethodOnchain:
		oc, pay, ve := g.verifyOnchain(ctx, pp)
		if ve != nil {
			return fail(ve)
		}
		settleKey = sha256.Sum256([]byte("dcr402/onchain/" + pay.TxID + "/" + oc.Address))
		out.Rail = x402.MethodOnchain
		out.AmountAtoms = oc.AmountAtoms
		out.ExternalRef = "onchain:" + pay.TxID + ":" + oc.Address
		out.Response = x402.SettlementResponse{
			Success:     true,
			Transaction: pay.TxID,
			Network:     g.cfg.Network.CAIP2,
			Amount:      strconv.FormatInt(oc.AmountAtoms, 10),
		}
		out.Receipt = x402.MustRaw(map[string]any{
			"txid":      pay.TxID,
			"payTo":     oc.Address,
			"settledAt": now.UTC().Format(time.RFC3339),
			"amount":    strconv.FormatInt(oc.AmountAtoms, 10),
		})
		acct, aerr := accountFromMacaroon(oc.MacaroonB64)
		if aerr != nil {
			return fail(verifyErrf(ReasonUnexpectedSettle, "derive account id: %v", aerr))
		}
		out.AccountID = acct
		credAuth, credUntil, credOK = credentialFor(oc.MacaroonB64, oc.CredentialPreimage, g.cfg.Service)

	default:
		return fail(verifyErrf(ReasonInvalidPayload, "unknown assetTransferMethod"))
	}

	// Bind the proof to this resource's price. A challenge minted for a
	// cheaper endpoint must not settle a more expensive one (requiredAtoms 0
	// means unpriced, e.g. a buyer-chosen top-up).
	if requiredAtoms > 0 && out.AmountAtoms < requiredAtoms {
		return fail(verifyErrf(ReasonInvalidRequirements,
			"settled amount %d is below the %d required for this resource",
			out.AmountAtoms, requiredAtoms))
	}

	if credOK {
		info := map[string]any{"authorization": credAuth}
		if credUntil > 0 {
			info["validUntil"] = credUntil
		}
		out.Response.Extensions = map[string]x402.Extension{
			ExtensionL402: {Info: x402.MustRaw(info), Schema: l402DeliverSchema},
		}
	}

	if g.cfg.OfferReceiptKey != nil {
		resURL := ""
		if pp.Resource != nil {
			resURL = pp.Resource.URL
		}
		if ext, ok := g.receiptExtension(resURL, out.Response.Network, out.Response.Payer, out.Response.Transaction, now.Unix()); ok {
			if out.Response.Extensions == nil {
				out.Response.Extensions = map[string]x402.Extension{}
			}
			out.Response.Extensions[ExtensionOfferReceipt] = ext
		}
	}

	respJSON, err := json.Marshal(out.Response)
	if err != nil {
		return fail(verifyErrf(ReasonUnexpectedSettle, "marshal response: %v", err))
	}
	stored, already, err := g.cfg.Store.Settle(ctx, settleKey, respJSON, now)
	if err != nil {
		return fail(verifyErrf(ReasonUnexpectedSettle, "settle: %v", err))
	}
	if err := json.Unmarshal(stored, &out.Response); err != nil {
		return fail(verifyErrf(ReasonUnexpectedSettle, "stored response: %v", err))
	}
	out.Already = already
	return out, nil
}

// failureResponse renders a VerifyError as the wire SettlementResponse.
func (g *Gate) failureResponse(vErr *VerifyError) x402.SettlementResponse {
	return x402.SettlementResponse{
		Success:     false,
		ErrorReason: vErr.Reason,
		Transaction: "",
		Network:     g.cfg.Network.CAIP2,
	}
}

// accountFromMacaroon derives the ledger account id — the hex token_id —
// from a stored challenge macaroon.
func accountFromMacaroon(macB64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(macB64)
	if err != nil {
		return "", err
	}
	mac, err := l402.UnmarshalBinary(raw)
	if err != nil {
		return "", err
	}
	id, err := mac.Identifier()
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(id.TokenID[:]), nil
}

// credentialFor assembles the ready-to-send Authorization value for the
// challenge-minted macaroon (annex §6) plus its valid-until timestamp.
func credentialFor(macB64, preimageHex, service string) (string, int64, bool) {
	raw, err := base64.StdEncoding.DecodeString(macB64)
	if err != nil {
		return "", 0, false
	}
	mac, err := l402.UnmarshalBinary(raw)
	if err != nil {
		return "", 0, false
	}
	var validUntil int64
	prefix := service + l402.CaveatValidUntilSfx + "="
	for _, cav := range mac.Caveats {
		if strings.HasPrefix(cav, prefix) {
			if ts, err := strconv.ParseInt(strings.TrimPrefix(cav, prefix), 10, 64); err == nil {
				validUntil = ts // last = most restrictive
			}
		}
	}
	return l402.SchemeL402 + " " + macB64 + ":" + preimageHex, validUntil, true
}
