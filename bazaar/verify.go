package bazaar

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/x402"
)

// verifyRequest is the POST /verify and POST /settle body (x402 v2
// facilitator API): the client's payload plus the requirements the resource
// server offered for the resource.
type verifyRequest struct {
	X402Version         int                      `json:"x402Version"`
	PaymentPayload      x402.PaymentPayload      `json:"paymentPayload"`
	PaymentRequirements x402.PaymentRequirements `json:"paymentRequirements"`
}

// settleIdentity is the method-agnostic result of a successful verification:
// what to notarize and echo. transaction is the wire identifier (payment hash
// for lightning, txid for onchain); key is its lowercase idempotency/store key.
type settleIdentity struct {
	method      string
	transaction string
	key         string
	receipt     map[string]any
}

// checkAny resolves the network, applies the envelope checks, and dispatches
// verification by transfer method. The lightning method runs the same stateless
// lib.VerifyLightningProof the embedded gate uses. The onchain method reads the
// chain (see checkOnchain). It returns the settlement identity or the first
// failure.
func (s *Service) checkAny(ctx context.Context, req verifyRequest) (settleIdentity, dcr402.Network, *dcr402.VerifyError) {
	var id settleIdentity
	pp := req.PaymentPayload

	net, ok := s.networkFor(pp.Accepted.Network)
	if !ok {
		return id, net, &dcr402.VerifyError{
			Reason: dcr402.ReasonInvalidNetwork,
			Detail: "unsupported network " + pp.Accepted.Network,
		}
	}
	if ve := dcr402.CheckEnvelope(pp, net); ve != nil {
		return id, net, ve
	}

	switch pp.Accepted.TransferMethod() {
	case x402.MethodLightning:
		pay, ve := dcr402.VerifyLightningProof(pp, req.PaymentRequirements, net, s.now())
		if ve != nil {
			return id, net, ve
		}
		var extra x402.LightningExtra
		_ = json.Unmarshal(pp.Accepted.Extra, &extra)
		id = settleIdentity{
			method:      x402.MethodLightning,
			transaction: pay.PaymentHash,
			key:         strings.ToLower(pay.PaymentHash),
			receipt: map[string]any{
				"method":    x402.MethodLightning,
				"invoice":   extra.Invoice,
				"preimage":  pay.Preimage,
				"amount":    pp.Accepted.Amount,
				"network":   net.CAIP2,
				"settledAt": s.now().UTC().Format(time.RFC3339),
			},
		}
		return id, net, nil

	case x402.MethodOnchain:
		txid, ve := s.checkOnchain(ctx, req, net)
		if ve != nil {
			return id, net, ve
		}
		id = settleIdentity{
			method:      x402.MethodOnchain,
			transaction: txid,
			key:         txid, // already lowercased
			receipt: map[string]any{
				"method":    x402.MethodOnchain,
				"txid":      txid,
				"address":   pp.Accepted.PayTo,
				"amount":    pp.Accepted.Amount,
				"network":   net.CAIP2,
				"settledAt": s.now().UTC().Format(time.RFC3339),
			},
		}
		return id, net, nil

	default:
		return id, net, &dcr402.VerifyError{
			Reason: dcr402.ReasonInvalidPayload,
			Detail: "unknown assetTransferMethod",
		}
	}
}

// checkOnchain confirms, by reading the chain, that the payload's transaction
// pays the required amount to payTo at the required depth. dcrbazaar did not
// issue the challenge, so it verifies against the requirements the resource
// server offered (payTo, amount, extra.confirmations), not a stored challenge.
// It is stateless beyond the chain read and moves no funds. On success it
// returns the lowercase txid.
func (s *Service) checkOnchain(ctx context.Context, req verifyRequest, net dcr402.Network) (string, *dcr402.VerifyError) {
	pp := req.PaymentPayload
	if s.onchain == nil {
		return "", &dcr402.VerifyError{
			Reason: dcr402.ReasonInvalidPayload,
			Detail: "onchain method is not enabled on this dcrbazaar",
		}
	}
	var pay x402.OnchainPayload
	if err := json.Unmarshal(pp.Payload, &pay); err != nil || pay.TxID == "" {
		return "", &dcr402.VerifyError{Reason: dcr402.ReasonInvalidPayload, Detail: "malformed onchain payload"}
	}
	if raw, err := hex.DecodeString(pay.TxID); err != nil || len(raw) != 32 {
		return "", &dcr402.VerifyError{Reason: dcr402.ReasonInvalidPayload, Detail: "txid is not 32 hex bytes"}
	}
	address := pp.Accepted.PayTo
	if address == "" {
		return "", &dcr402.VerifyError{Reason: dcr402.ReasonInvalidRequirements, Detail: "payTo is required"}
	}
	requiredAtoms, err := strconv.ParseInt(pp.Accepted.Amount, 10, 64)
	if err != nil || requiredAtoms <= 0 {
		return "", &dcr402.VerifyError{
			Reason: dcr402.ReasonInvalidRequirements,
			Detail: "amount is not a positive atoms integer",
		}
	}
	var extra x402.OnchainExtra
	_ = json.Unmarshal(pp.Accepted.Extra, &extra)
	requiredConfs := int32(extra.Confirmations)
	if s.cfg.Onchain.MinConfs > requiredConfs {
		requiredConfs = s.cfg.Onchain.MinConfs
	}

	dep, err := s.onchain.lookupDeposit(ctx, pay.TxID, address)
	if err != nil {
		return "", &dcr402.VerifyError{Reason: dcr402.ReasonUnexpectedVerify, Detail: err.Error()}
	}
	if !dep.Found {
		return "", &dcr402.VerifyError{
			Reason: dcr402.ReasonInsufficientConfs,
			Detail: "transaction not yet visible on-chain",
		}
	}
	if dep.AmountToAddressAtoms == 0 {
		return "", &dcr402.VerifyError{Reason: dcr402.ReasonAddressMismatch, Detail: "transaction pays no output to payTo"}
	}
	if dep.AmountToAddressAtoms != requiredAtoms {
		return "", &dcr402.VerifyError{
			Reason: dcr402.ReasonAmountMismatch,
			Detail: fmt.Sprintf("outputs to payTo sum to %d atoms, want %d", dep.AmountToAddressAtoms, requiredAtoms),
		}
	}
	if dep.Confirmations < requiredConfs {
		return "", &dcr402.VerifyError{
			Reason: dcr402.ReasonInsufficientConfs,
			Detail: fmt.Sprintf("%d of %d confirmations", dep.Confirmations, requiredConfs),
		}
	}
	return strings.ToLower(pay.TxID), nil
}

// verify maps a check result to a VerifyResponse. It never errors: a rejected
// payment is a successful HTTP 200 carrying isValid:false (the x402
// facilitator convention). Payer is left empty (a Decred lightning preimage or
// an on-chain txid reveals no payer identity to dcrbazaar).
func (s *Service) verify(ctx context.Context, req verifyRequest) x402.VerifyResponse {
	if _, _, ve := s.checkAny(ctx, req); ve != nil {
		return x402.VerifyResponse{IsValid: false, InvalidReason: ve.Reason, InvalidMessage: ve.Detail}
	}
	return x402.VerifyResponse{IsValid: true}
}

// settle verifies then notarizes a settlement. It moves no funds (dcrbazaar
// holds no keys); it records the settlement under the payment hash (lightning)
// or txid (onchain) so re-presentations are idempotent, and returns the wire
// response.
func (s *Service) settle(ctx context.Context, req verifyRequest) x402.SettlementResponse {
	pp := req.PaymentPayload

	// Network and envelope first, so a malformed or wrong-network request is
	// rejected before it is treated as a replay.
	net, ok := s.networkFor(pp.Accepted.Network)
	if !ok {
		return x402.SettlementResponse{Success: false,
			ErrorReason: dcr402.ReasonInvalidNetwork, Network: pp.Accepted.Network}
	}
	if ve := dcr402.CheckEnvelope(pp, net); ve != nil {
		return x402.SettlementResponse{Success: false, ErrorReason: ve.Reason, Network: net.CAIP2}
	}

	// Idempotency: a key that already settled returns the stored response even
	// if the invoice has since expired, so a client that lost the first
	// response can retry (the embedded gate does the same).
	if key := idempotencyKey(pp); key != "" {
		if stored, ok, err := s.store.GetSettlement(ctx, key); err == nil && ok {
			var resp x402.SettlementResponse
			if json.Unmarshal(stored, &resp) == nil {
				return resp
			}
		}
	}

	id, _, ve := s.checkAny(ctx, req)
	if ve != nil {
		return x402.SettlementResponse{Success: false, ErrorReason: ve.Reason, Network: net.CAIP2}
	}

	resp := x402.SettlementResponse{
		Success:     true,
		Transaction: id.transaction,
		Network:     net.CAIP2,
		Amount:      pp.Accepted.Amount,
	}
	respJSON, err := json.Marshal(resp)
	if err != nil {
		return failSettle(net.CAIP2)
	}

	stored, _, err := s.store.Settle(ctx, id.key, respJSON, x402.MustRaw(id.receipt), s.now())
	if err != nil {
		return failSettle(net.CAIP2)
	}
	if err := json.Unmarshal(stored, &resp); err != nil {
		return failSettle(net.CAIP2)
	}
	return resp
}

// idempotencyKey is the settlement store key for a payload, by method: the
// lowercase payment hash (lightning) or lowercase txid (onchain), case-
// normalized so the same logical payment cannot create two rows.
func idempotencyKey(pp x402.PaymentPayload) string {
	switch pp.Accepted.TransferMethod() {
	case x402.MethodLightning:
		var pay x402.LightningPayload
		if json.Unmarshal(pp.Payload, &pay) == nil && pay.PaymentHash != "" {
			return strings.ToLower(pay.PaymentHash)
		}
	case x402.MethodOnchain:
		var pay x402.OnchainPayload
		if json.Unmarshal(pp.Payload, &pay) == nil && pay.TxID != "" {
			return strings.ToLower(pay.TxID)
		}
	}
	return ""
}

func failSettle(networkCAIP2 string) x402.SettlementResponse {
	return x402.SettlementResponse{
		Success:     false,
		ErrorReason: dcr402.ReasonUnexpectedSettle,
		Transaction: "",
		Network:     networkCAIP2,
	}
}
