package bazaar

import (
	"context"
	"strings"
	"testing"

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/x402"
)

func TestDcrToAtoms(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"0", 0, false},
		{"0.00000001", 1, false},
		{"0.05399233", 5399233, false},
		{"1", 100000000, false},
		{"12.5", 1250000000, false},
		{"21000000.00000000", 2100000000000000, false},
		{"0.000000001", 0, true}, // 9 decimals: rejected
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := dcrToAtoms(c.in)
		if c.err {
			if err == nil {
				t.Errorf("dcrToAtoms(%q): want error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("dcrToAtoms(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("dcrToAtoms(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

type stubLooker struct {
	dep dcr402.DepositStatus
	err error
}

func (s stubLooker) lookupDeposit(context.Context, string, string) (dcr402.DepositStatus, error) {
	return s.dep, s.err
}

// onchainReq builds a minimal onchain verifyRequest requiring amountAtoms to
// payTo at confs confirmations.
func onchainReq(amount, payTo string, confs int) verifyRequest {
	txid := strings.Repeat("ab", 32)
	return verifyRequest{
		PaymentPayload: x402.PaymentPayload{
			X402Version: x402.Version,
			Accepted: x402.PaymentRequirements{
				Scheme:  x402.SchemeExact,
				Network: dcr402.Mainnet.CAIP2,
				Amount:  amount,
				PayTo:   payTo,
				Extra: x402.MustRaw(map[string]any{
					"assetTransferMethod": x402.MethodOnchain,
					"confirmations":       confs,
				}),
			},
			Payload: x402.MustRaw(x402.OnchainPayload{TxID: txid}),
		},
	}
}

func TestCheckOnchain(t *testing.T) {
	const addr = "DsExampleAddr00000000000000000000000"
	req := onchainReq("100000", addr, 2)

	cases := []struct {
		name    string
		svc     *Service
		wantErr string // "" means success
	}{
		{
			name:    "valid",
			svc:     &Service{onchain: stubLooker{dep: dcr402.DepositStatus{Found: true, Confirmations: 2, AmountToAddressAtoms: 100000}}, cfg: Config{Onchain: OnchainConfig{Enabled: true, MinConfs: 1}}},
			wantErr: "",
		},
		{
			name:    "amount mismatch",
			svc:     &Service{onchain: stubLooker{dep: dcr402.DepositStatus{Found: true, Confirmations: 2, AmountToAddressAtoms: 99999}}, cfg: Config{Onchain: OnchainConfig{Enabled: true, MinConfs: 1}}},
			wantErr: dcr402.ReasonAmountMismatch,
		},
		{
			name:    "insufficient confs",
			svc:     &Service{onchain: stubLooker{dep: dcr402.DepositStatus{Found: true, Confirmations: 1, AmountToAddressAtoms: 100000}}, cfg: Config{Onchain: OnchainConfig{Enabled: true, MinConfs: 1}}},
			wantErr: dcr402.ReasonInsufficientConfs,
		},
		{
			name:    "not yet visible",
			svc:     &Service{onchain: stubLooker{dep: dcr402.DepositStatus{Found: false}}, cfg: Config{Onchain: OnchainConfig{Enabled: true, MinConfs: 1}}},
			wantErr: dcr402.ReasonInsufficientConfs,
		},
		{
			name:    "no output to payTo",
			svc:     &Service{onchain: stubLooker{dep: dcr402.DepositStatus{Found: true, Confirmations: 5, AmountToAddressAtoms: 0}}, cfg: Config{Onchain: OnchainConfig{Enabled: true, MinConfs: 1}}},
			wantErr: dcr402.ReasonAddressMismatch,
		},
		{
			name:    "disabled",
			svc:     &Service{cfg: Config{Onchain: OnchainConfig{Enabled: false}}},
			wantErr: dcr402.ReasonInvalidPayload,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			txid, ve := c.svc.checkOnchain(context.Background(), req, dcr402.Mainnet)
			if c.wantErr == "" {
				if ve != nil {
					t.Fatalf("want success, got %v", ve)
				}
				if txid != strings.Repeat("ab", 32) {
					t.Fatalf("txid = %q", txid)
				}
				return
			}
			if ve == nil {
				t.Fatalf("want %s, got success", c.wantErr)
			}
			if ve.Reason != c.wantErr {
				t.Fatalf("reason = %q, want %q", ve.Reason, c.wantErr)
			}
		})
	}
}

// TestMinConfsFloor: the effective requirement is the larger of the challenge's
// confirmations and the config floor.
func TestMinConfsFloor(t *testing.T) {
	req := onchainReq("100000", "DsAddr", 1) // challenge asks for 1
	svc := &Service{
		onchain: stubLooker{dep: dcr402.DepositStatus{Found: true, Confirmations: 2, AmountToAddressAtoms: 100000}},
		cfg:     Config{Onchain: OnchainConfig{Enabled: true, MinConfs: 3}}, // floor 3
	}
	if _, ve := svc.checkOnchain(context.Background(), req, dcr402.Mainnet); ve == nil || ve.Reason != dcr402.ReasonInsufficientConfs {
		t.Fatalf("want insufficient_confirmations (floor 3 > depth 2), got %v", ve)
	}
}
