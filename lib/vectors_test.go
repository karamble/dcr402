package dcr402

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrlnd/zpay32"
)

// TestInvoiceVectors decodes every invoice in the repository's test vectors
// through zpay32 and checks the expected values — the Go-side BOLT11
// verification promised by scheme/test-vectors/README.md.
func TestInvoiceVectors(t *testing.T) {
	raw, err := os.ReadFile("../scheme/test-vectors/invoices.json")
	if err != nil {
		t.Fatalf("reading invoice vectors: %v", err)
	}
	var file struct {
		Vectors []struct {
			Name          string `json:"name"`
			Network       string `json:"network"`
			HRP           string `json:"hrp"`
			Invoice       string `json:"invoice"`
			PaymentHash   string `json:"paymentHash"`
			Preimage      string `json:"preimage"`
			Destination   string `json:"destination"`
			MilliAtoms    string `json:"milliAtoms"`
			AmountAtoms   string `json:"amountAtoms"`
			Timestamp     int64  `json:"timestamp"`
			ExpirySeconds int64  `json:"expirySeconds"`
			MustReject    bool   `json:"mustReject"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}

	networks := map[string]Network{
		Mainnet.CAIP2:  Mainnet,
		Testnet3.CAIP2: Testnet3,
		Simnet.CAIP2:   Simnet,
	}

	for _, v := range file.Vectors {
		v := v
		t.Run(v.Name, func(t *testing.T) {
			net, ok := networks[v.Network]
			if !ok {
				t.Fatalf("unknown network %s", v.Network)
			}
			if net.HRP != v.HRP {
				t.Fatalf("network HRP %s != vector hrp %s", net.HRP, v.HRP)
			}
			if !strings.HasPrefix(v.Invoice, v.HRP) {
				t.Fatalf("invoice does not start with %s", v.HRP)
			}

			inv, err := zpay32.Decode(v.Invoice, net.Params)
			if err != nil {
				t.Fatalf("zpay32.Decode: %v", err)
			}
			if inv.PaymentHash == nil ||
				hex.EncodeToString(inv.PaymentHash[:]) != v.PaymentHash {
				t.Fatalf("payment hash mismatch")
			}
			if inv.Destination == nil ||
				hex.EncodeToString(inv.Destination.SerializeCompressed()) != v.Destination {
				t.Fatalf("destination mismatch")
			}
			if inv.Timestamp.Unix() != v.Timestamp {
				t.Fatalf("timestamp %d != %d", inv.Timestamp.Unix(), v.Timestamp)
			}

			if v.MustReject {
				// The amountless donation invoice: decodes fine, but the
				// scheme's rule 6 rejects it.
				if inv.MilliAt != nil {
					t.Fatalf("mustReject vector unexpectedly carries an amount")
				}
				return
			}
			if inv.MilliAt == nil {
				t.Fatalf("invoice carries no amount")
			}
			wantMAt, err := strconv.ParseInt(v.MilliAtoms, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			if int64(*inv.MilliAt) != wantMAt {
				t.Fatalf("m-atoms %d != %d", int64(*inv.MilliAt), wantMAt)
			}
			wantAtoms, err := strconv.ParseInt(v.AmountAtoms, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			if wantAtoms*1000 != wantMAt {
				t.Fatalf("amountAtoms×1000 != milliAtoms")
			}

			wantExpiry := 3600 * time.Second // BOLT11 default when x absent
			if v.ExpirySeconds != 0 {
				wantExpiry = time.Duration(v.ExpirySeconds) * time.Second
			}
			if inv.Expiry() != wantExpiry {
				t.Fatalf("expiry %v != %v", inv.Expiry(), wantExpiry)
			}

			if v.Preimage != "" {
				pre, err := hex.DecodeString(v.Preimage)
				if err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256(pre)
				if hex.EncodeToString(sum[:]) != v.PaymentHash {
					t.Fatalf("sha256(preimage) != paymentHash")
				}
			}
		})
	}
}
