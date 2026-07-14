package bazaar

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	dcr402 "github.com/karamble/dcr402/lib"
)

// onchainLooker reads the Decred chain to report a deposit transaction's depth
// and the atoms it pays to an address. It is the only external dependency of
// the onchain verify path; dcrdataClient is the production implementation and
// tests inject a stub. It holds no keys and issues no addresses: the seller
// minted the payTo address and the payer self-broadcast the transaction, so
// dcrbazaar only reads whether that transaction landed (non-custodial).
type onchainLooker interface {
	lookupDeposit(ctx context.Context, txid, address string) (dcr402.DepositStatus, error)
}

// dcrdataClient confirms on-chain deposits through a dcrdata Insight API.
type dcrdataClient struct {
	baseURL string
	http    *http.Client
}

func newDcrdataClient(baseURL string) *dcrdataClient {
	return &dcrdataClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// insightTx is the subset of the dcrdata Insight /tx/{txid} response we read.
type insightTx struct {
	Confirmations int32 `json:"confirmations"`
	Vout          []struct {
		Value        json.Number `json:"value"` // DCR, fixed 8-decimal
		ScriptPubKey struct {
			Addresses []string `json:"addresses"`
		} `json:"scriptPubKey"`
	} `json:"vout"`
}

// lookupDeposit reports the chain's view of txid relative to address: whether
// it is visible yet, its confirmation depth, and the exact atoms its outputs
// pay to address. A transaction not yet known returns Found=false (the
// retryable pending condition), never an error.
func (c *dcrdataClient) lookupDeposit(ctx context.Context, txid, address string) (dcr402.DepositStatus, error) {
	var zero dcr402.DepositStatus
	if raw, err := hex.DecodeString(txid); err != nil || len(raw) != 32 {
		return zero, fmt.Errorf("dcrdata: txid is not 32 hex bytes")
	}
	url := c.baseURL + "/insight/api/tx/" + txid
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return zero, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return zero, fmt.Errorf("dcrdata: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusNotFound, http.StatusBadRequest:
		// Not yet mined / unknown to the indexer: pending, retryable.
		return dcr402.DepositStatus{Found: false}, nil
	default:
		return zero, fmt.Errorf("dcrdata: unexpected status %d", resp.StatusCode)
	}
	var tx insightTx
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&tx); err != nil {
		return zero, fmt.Errorf("dcrdata: decode: %w", err)
	}
	var atoms int64
	for _, v := range tx.Vout {
		if !containsAddr(v.ScriptPubKey.Addresses, address) {
			continue
		}
		a, err := dcrToAtoms(v.Value.String())
		if err != nil {
			return zero, fmt.Errorf("dcrdata: %w", err)
		}
		atoms += a
	}
	return dcr402.DepositStatus{
		Found:                true,
		Confirmations:        tx.Confirmations,
		AmountToAddressAtoms: atoms,
	}, nil
}

func containsAddr(addrs []string, want string) bool {
	for _, a := range addrs {
		if a == want {
			return true
		}
	}
	return false
}

// dcrToAtoms converts a fixed-point DCR decimal string (up to 8 fractional
// digits) to atoms exactly, avoiding float rounding on the amount that a
// settlement is checked against.
func dcrToAtoms(s string) (int64, error) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if len(fracPart) > 8 {
		return 0, fmt.Errorf("amount %q has more than 8 decimals", s)
	}
	for len(fracPart) < 8 {
		fracPart += "0"
	}
	whole, err := strconv.ParseInt(defZero(intPart), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q: %w", s, err)
	}
	frac, err := strconv.ParseInt(defZero(fracPart), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q: %w", s, err)
	}
	atoms := whole*1e8 + frac
	if neg {
		atoms = -atoms
	}
	return atoms, nil
}

func defZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}
