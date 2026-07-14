package dcr402

import (
	"testing"
	"time"

	"github.com/karamble/dcr402/lib/x402"
)

// goldenOffered is the PaymentRequirements a seller offers for the golden
// vector: the exact entry the client must echo in accepted.
func goldenOffered() x402.PaymentRequirements {
	return x402.PaymentRequirements{
		Scheme:            x402.SchemeExact,
		Network:           Mainnet.CAIP2,
		Amount:            "250000",
		Asset:             x402.AssetDCR,
		PayTo:             goldenDest,
		MaxTimeoutSeconds: 3600,
		Extra: x402.MustRaw(x402.LightningExtra{
			AssetTransferMethod: x402.MethodLightning,
			Invoice:             goldenInvoice,
			PaymentHash:         goldenHash,
		}),
	}
}

func goldenProof() x402.PaymentPayload {
	offered := goldenOffered()
	return x402.PaymentPayload{
		X402Version: 2,
		Accepted:    offered,
		Payload:     x402.MustRaw(x402.LightningPayload{Preimage: goldenPre, PaymentHash: goldenHash}),
	}
}

// TestVerifyLightningProof exercises the shared stateless verifier the
// facilitator uses: no store, no node, just the payload, the offered
// requirements, and the network.
func TestVerifyLightningProof(t *testing.T) {
	offered := goldenOffered()

	t.Run("valid", func(t *testing.T) {
		pay, ve := VerifyLightningProof(goldenProof(), offered, Mainnet, time.Time{})
		if ve != nil {
			t.Fatalf("unexpected error: %v", ve)
		}
		if pay.Preimage != goldenPre {
			t.Fatalf("preimage %q", pay.Preimage)
		}
	})

	t.Run("wrong preimage", func(t *testing.T) {
		pp := goldenProof()
		pp.Payload = x402.MustRaw(x402.LightningPayload{
			Preimage:    "2222222222222222222222222222222222222222222222222222222222222222",
			PaymentHash: goldenHash,
		})
		if _, ve := VerifyLightningProof(pp, offered, Mainnet, time.Time{}); ve == nil || ve.Reason != ReasonPreimageMismatch {
			t.Fatalf("reason %v", ve)
		}
	})

	t.Run("accepted not equal to offered", func(t *testing.T) {
		pp := goldenProof()
		pp.Accepted.Amount = "1" // preimage still valid; equality must fail
		if _, ve := VerifyLightningProof(pp, offered, Mainnet, time.Time{}); ve == nil || ve.Reason != ReasonInvalidRequirements {
			t.Fatalf("reason %v", ve)
		}
	})

	t.Run("wrong network", func(t *testing.T) {
		// A mainnet invoice (lndcr...) verified against simnet (lnsdcr): the
		// facilitator resolves and passes the network, and the prefix binding
		// fails. accepted/offered are aligned to simnet so equality passes
		// first.
		pp := goldenProof()
		pp.Accepted.Network = Simnet.CAIP2
		sim := pp.Accepted
		if _, ve := VerifyLightningProof(pp, sim, Simnet, time.Time{}); ve == nil || ve.Reason != ReasonInvoiceNetwork {
			t.Fatalf("reason %v", ve)
		}
	})

	t.Run("rule 7 skipped when now is zero", func(t *testing.T) {
		// The golden invoice carries a fixed past timestamp; the zero Time
		// opts out of the embedded-expiry check (the embedded gate relies on
		// its challenge store for liveness instead).
		if _, ve := VerifyLightningProof(goldenProof(), offered, Mainnet, time.Time{}); ve != nil {
			t.Fatalf("unexpected error: %v", ve)
		}
	})

	t.Run("rule 7 enforced when now is supplied", func(t *testing.T) {
		future := time.Unix(1<<40, 0) // far past any test-vector expiry
		if _, ve := VerifyLightningProof(goldenProof(), offered, Mainnet, future); ve == nil || ve.Reason != ReasonInvoiceExpired {
			t.Fatalf("reason %v", ve)
		}
	})
}
