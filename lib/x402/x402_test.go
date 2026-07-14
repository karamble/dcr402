package x402

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const vectorDir = "../../scheme/test-vectors"

type wrappedVector struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	JSON     json.RawMessage `json:"json"`
	JSONText string          `json:"jsonText"`
	Base64   string          `json:"base64"`
}

type vectorFile struct {
	Vectors []wrappedVector `json:"vectors"`
}

func loadVectors(t *testing.T, name string) vectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(vectorDir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return vf
}

func semanticEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	return deepEqualJSON(av, bv)
}

// TestVectorRoundTrips decodes every wrapped envelope vector into its typed
// structure, re-encodes it, and requires semantic equality with the vector's
// json value: the Go types must carry every field the scheme defines.
func TestVectorRoundTrips(t *testing.T) {
	for _, fname := range []string{"x402-lightning.json", "x402-onchain.json"} {
		vf := loadVectors(t, fname)
		for _, v := range vf.Vectors {
			if v.Base64 == "" {
				continue // MCP vector: no header transport
			}
			v := v
			t.Run(fname+"/"+v.Name, func(t *testing.T) {
				// base64 <-> jsonText is byte-exact.
				raw, err := base64.StdEncoding.DecodeString(v.Base64)
				if err != nil {
					t.Fatalf("base64: %v", err)
				}
				if string(raw) != v.JSONText {
					t.Fatalf("base64 does not decode to jsonText")
				}

				var typed any
				switch v.Name {
				case "payment-required":
					typed = &PaymentRequired{}
				case "payment-payload":
					typed = &PaymentPayload{}
				case "settlement-success", "settlement-failure", "settlement-pending":
					typed = &SettlementResponse{}
				default:
					t.Fatalf("unknown vector %q", v.Name)
				}
				if err := DecodeHeader(v.Base64, typed); err != nil {
					t.Fatalf("DecodeHeader: %v", err)
				}
				reenc, err := json.Marshal(typed)
				if err != nil {
					t.Fatalf("re-marshal: %v", err)
				}
				if !semanticEqual(t, reenc, v.JSON) {
					t.Fatalf("re-encoded value differs from vector json:\n%s\nvs\n%s", reenc, v.JSON)
				}

				// EncodeHeader must round-trip through DecodeHeader.
				enc, err := EncodeHeader(typed)
				if err != nil {
					t.Fatalf("EncodeHeader: %v", err)
				}
				if _, err := base64.StdEncoding.DecodeString(enc); err != nil {
					t.Fatalf("EncodeHeader output not standard base64: %v", err)
				}
			})
		}
	}
}

// TestAcceptedEchoesOffered pins verification rule 1's equality helper to the
// vectors: the payload's accepted entry equals the challenge's accepts[0].
func TestAcceptedEchoesOffered(t *testing.T) {
	for _, fname := range []string{"x402-lightning.json", "x402-onchain.json"} {
		vf := loadVectors(t, fname)
		var req PaymentRequired
		var pay PaymentPayload
		for _, v := range vf.Vectors {
			switch v.Name {
			case "payment-required":
				if err := json.Unmarshal(v.JSON, &req); err != nil {
					t.Fatal(err)
				}
			case "payment-payload":
				if err := json.Unmarshal(v.JSON, &pay); err != nil {
					t.Fatal(err)
				}
			}
		}
		if !EqualSemantic(pay.Accepted, req.Accepts[0]) {
			t.Fatalf("%s: accepted != accepts[0]", fname)
		}
		// A different amount must not compare equal.
		mod := req.Accepts[0]
		mod.Amount = "1"
		if EqualSemantic(pay.Accepted, mod) {
			t.Fatalf("%s: EqualSemantic ignored amount change", fname)
		}
		// A change inside extra must not compare equal.
		mod = req.Accepts[0]
		var extra map[string]any
		if err := json.Unmarshal(mod.Extra, &extra); err != nil {
			t.Fatal(err)
		}
		extra["assetTransferMethod"] = "carrier-pigeon"
		mod.Extra = MustRaw(extra)
		if EqualSemantic(pay.Accepted, mod) {
			t.Fatalf("%s: EqualSemantic ignored extra change", fname)
		}
	}
}

// TestTransferMethod checks the extra.assetTransferMethod probe.
func TestTransferMethod(t *testing.T) {
	vf := loadVectors(t, "x402-lightning.json")
	var req PaymentRequired
	for _, v := range vf.Vectors {
		if v.Name == "payment-required" {
			if err := json.Unmarshal(v.JSON, &req); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := req.Accepts[0].TransferMethod(); got != MethodLightning {
		t.Fatalf("TransferMethod = %q, want lightning", got)
	}
	if got := (PaymentRequirements{}).TransferMethod(); got != "" {
		t.Fatalf("TransferMethod on empty extra = %q, want empty", got)
	}
}

// TestMCPVectorShape verifies the MCP tool-result vector against the typed
// PaymentRequired: structuredContent parses into it and content[0].text is
// its serialization.
func TestMCPVectorShape(t *testing.T) {
	vf := loadVectors(t, "x402-lightning.json")
	for _, v := range vf.Vectors {
		if v.Name != "mcp-payment-required" {
			continue
		}
		var result struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
			Content           []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(v.JSON, &result); err != nil {
			t.Fatal(err)
		}
		if !result.IsError {
			t.Fatal("isError must be true")
		}
		var pr PaymentRequired
		if err := json.Unmarshal(result.StructuredContent, &pr); err != nil {
			t.Fatalf("structuredContent: %v", err)
		}
		if pr.X402Version != Version || len(pr.Accepts) == 0 {
			t.Fatal("structuredContent is not a PaymentRequired")
		}
		if !semanticEqual(t, []byte(result.Content[0].Text), result.StructuredContent) {
			t.Fatal("content[0].text != structuredContent")
		}
		return
	}
	t.Fatal("mcp-payment-required vector not found")
}
