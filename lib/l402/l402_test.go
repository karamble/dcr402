package l402

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	macaroonv2 "gopkg.in/macaroon.v2"
)

const vectorPath = "../../scheme/test-vectors/l402.json"

type l402Vectors struct {
	Constants struct {
		Preimage          string   `json:"preimage"`
		RootKey           string   `json:"rootKey"`
		TokenID           string   `json:"tokenId"`
		IdentifierVersion int      `json:"identifierVersion"`
		Caveats           []string `json:"caveats"`
		Invoice           string   `json:"invoice"`
	} `json:"constants"`
	Derived struct {
		PaymentHash    string   `json:"paymentHash"`
		Identifier     string   `json:"identifier"`
		HMACChain      []string `json:"hmacChain"`
		Signature      string   `json:"signature"`
		MacaroonHex    string   `json:"macaroonHex"`
		MacaroonBase64 string   `json:"macaroonBase64"`
	} `json:"derived"`
	Headers struct {
		Challenge     []string `json:"challenge"`
		Authorization string   `json:"authorization"`
	} `json:"headers"`
}

func loadVectors(t *testing.T) l402Vectors {
	t.Helper()
	raw, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatalf("reading vectors: %v", err)
	}
	var v l402Vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parsing vectors: %v", err)
	}
	return v
}

func vectorInputs(t *testing.T, v l402Vectors) (rootKey [32]byte, id Identifier, preimage string) {
	t.Helper()
	pre, _ := hex.DecodeString(v.Constants.Preimage)
	rk, _ := hex.DecodeString(v.Constants.RootKey)
	tid, _ := hex.DecodeString(v.Constants.TokenID)
	copy(rootKey[:], rk)
	id.Version = uint16(v.Constants.IdentifierVersion)
	id.PaymentHash = sha256.Sum256(pre)
	copy(id.TokenID[:], tid)
	return rootKey, id, v.Constants.Preimage
}

// TestMintMatchesVectors re-derives every value in l402.json.
func TestMintMatchesVectors(t *testing.T) {
	v := loadVectors(t)
	rootKey, id, _ := vectorInputs(t, v)

	if hex.EncodeToString(id.PaymentHash[:]) != v.Derived.PaymentHash {
		t.Fatalf("payment hash mismatch")
	}
	if hex.EncodeToString(id.Encode()) != v.Derived.Identifier {
		t.Fatalf("identifier encoding mismatch")
	}

	// Chain intermediates via Mint(no caveats) + Attenuate steps.
	step := Mint(rootKey, id, nil)
	if hex.EncodeToString(step.Signature[:]) != v.Derived.HMACChain[0] {
		t.Fatalf("sig_0 mismatch")
	}
	for i, cav := range v.Constants.Caveats {
		step.Attenuate(cav)
		if hex.EncodeToString(step.Signature[:]) != v.Derived.HMACChain[i+1] {
			t.Fatalf("sig_%d mismatch", i+1)
		}
	}

	m := Mint(rootKey, id, v.Constants.Caveats)
	if hex.EncodeToString(m.Signature[:]) != v.Derived.Signature {
		t.Fatalf("final signature mismatch")
	}
	bin := m.MarshalBinary()
	if hex.EncodeToString(bin) != v.Derived.MacaroonHex {
		t.Fatalf("V2 serialization mismatch")
	}
	if base64.StdEncoding.EncodeToString(bin) != v.Derived.MacaroonBase64 {
		t.Fatalf("base64 mismatch")
	}
	if !m.VerifySignature(rootKey) {
		t.Fatalf("VerifySignature rejected freshly minted macaroon")
	}
}

// TestUnmarshalRoundTrip parses the vector macaroon and re-serializes it
// byte-identically; a tampered caveat must fail signature verification.
func TestUnmarshalRoundTrip(t *testing.T) {
	v := loadVectors(t)
	rootKey, _, _ := vectorInputs(t, v)

	raw, err := hex.DecodeString(v.Derived.MacaroonHex)
	if err != nil {
		t.Fatal(err)
	}
	m, err := UnmarshalBinary(raw)
	if err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	if hex.EncodeToString(m.ID) != v.Derived.Identifier {
		t.Fatalf("parsed identifier mismatch")
	}
	if len(m.Caveats) != len(v.Constants.Caveats) {
		t.Fatalf("parsed %d caveats, want %d", len(m.Caveats), len(v.Constants.Caveats))
	}
	for i := range m.Caveats {
		if m.Caveats[i] != v.Constants.Caveats[i] {
			t.Fatalf("caveat %d mismatch: %q", i, m.Caveats[i])
		}
	}
	if hex.EncodeToString(m.MarshalBinary()) != v.Derived.MacaroonHex {
		t.Fatalf("re-serialization differs")
	}
	if !m.VerifySignature(rootKey) {
		t.Fatalf("parsed macaroon fails signature verification")
	}

	tampered := *m
	tampered.Caveats = append([]string(nil), m.Caveats...)
	tampered.Caveats[0] = "services=evil:9"
	if tampered.VerifySignature(rootKey) {
		t.Fatalf("tampered caveat passed signature verification")
	}
}

// TestMacaroonV2Parity checks wire compatibility: gopkg.in/macaroon.v2
// parses our bytes and re-serializes them byte-identically.
func TestMacaroonV2Parity(t *testing.T) {
	v := loadVectors(t)
	raw, _ := hex.DecodeString(v.Derived.MacaroonHex)

	var ref macaroonv2.Macaroon
	if err := ref.UnmarshalBinary(raw); err != nil {
		t.Fatalf("macaroon.v2 rejects our serialization: %v", err)
	}
	if hex.EncodeToString(ref.Id()) != v.Derived.Identifier {
		t.Fatalf("macaroon.v2 parsed a different identifier")
	}
	if got := len(ref.Caveats()); got != len(v.Constants.Caveats) {
		t.Fatalf("macaroon.v2 parsed %d caveats", got)
	}
	out, err := ref.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(out) != v.Derived.MacaroonHex {
		t.Fatalf("macaroon.v2 re-serialization differs")
	}
}

// TestChallengeHeaders reproduces the vector WWW-Authenticate lines (LSAT
// first) and the Authorization value.
func TestChallengeHeaders(t *testing.T) {
	v := loadVectors(t)
	rootKey, id, preimage := vectorInputs(t, v)
	m := Mint(rootKey, id, v.Constants.Caveats)

	h := http.Header{}
	WriteChallenge(h, m, v.Constants.Invoice)
	got := h.Values("WWW-Authenticate")
	if len(got) != 2 {
		t.Fatalf("expected 2 WWW-Authenticate values, got %d", len(got))
	}
	for i, want := range v.Headers.Challenge {
		want = strings.TrimPrefix(want, "WWW-Authenticate: ")
		if got[i] != want {
			t.Fatalf("challenge[%d]:\n got %s\nwant %s", i, got[i], want)
		}
	}

	auth := AuthorizationValue(m, preimage)
	want := strings.TrimPrefix(v.Headers.Authorization, "Authorization: ")
	if auth != want {
		t.Fatalf("authorization:\n got %s\nwant %s", auth, want)
	}
}

// TestParseAndVerify exercises ParseAuthorization + VerifyToken end to end
// against the vector credential, including expiry, service, capability, and
// keyword handling.
func TestParseAndVerify(t *testing.T) {
	v := loadVectors(t)
	rootKey, _, _ := vectorInputs(t, v)
	value := strings.TrimPrefix(v.Headers.Authorization, "Authorization: ")

	macs, preimage, ok, err := ParseAuthorization(value)
	if !ok || err != nil {
		t.Fatalf("ParseAuthorization: ok=%v err=%v", ok, err)
	}
	if len(macs) != 1 || preimage != v.Constants.Preimage {
		t.Fatalf("unexpected parse result")
	}

	before := time.Unix(1893455999, 0) // just inside _valid_until
	after := time.Unix(1893456000, 0)

	base := VerifyOptions{Service: "example", Now: before}
	if err := VerifyToken(rootKey, macs[0], preimage, base); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	for name, opts := range map[string]VerifyOptions{
		"expired":          {Service: "example", Now: after},
		"wrong service":    {Service: "other", Now: before},
		"wrong capability": {Service: "example", Capability: "admin", Now: before},
	} {
		if err := VerifyToken(rootKey, macs[0], preimage, opts); err == nil {
			t.Fatalf("%s: token accepted", name)
		}
	}
	if err := VerifyToken(rootKey, macs[0], preimage,
		VerifyOptions{Service: "example", Capability: "read", Now: before}); err != nil {
		t.Fatalf("permitted capability rejected: %v", err)
	}

	// Wrong preimage and wrong root key must fail.
	if err := VerifyToken(rootKey, macs[0], strings.Repeat("22", 32), base); err == nil {
		t.Fatal("wrong preimage accepted")
	}
	var otherKey [32]byte
	if err := VerifyToken(otherKey, macs[0], preimage, base); err == nil {
		t.Fatal("wrong root key accepted")
	}

	// LSAT keyword and lowercase scheme are accepted.
	for _, alt := range []string{
		"LSAT " + strings.TrimPrefix(value, "L402 "),
		"l402 " + strings.TrimPrefix(value, "L402 "),
	} {
		if _, _, ok, err := ParseAuthorization(alt); !ok || err != nil {
			t.Fatalf("scheme variant %q rejected: ok=%v err=%v", alt[:5], ok, err)
		}
	}

	// Non-L402 schemes are not-ours (ok=false, no error); malformed L402
	// credentials are errors (ok=true).
	if _, _, ok, _ := ParseAuthorization("Bearer abc"); ok {
		t.Fatal("Bearer parsed as L402")
	}
	if _, _, ok, err := ParseAuthorization("L402 not-base64!!:11"); !ok || err == nil {
		t.Fatal("malformed credential not reported as error")
	}
}

// TestAttenuation checks holder-side attenuation semantics: tightening is
// verifiable, loosening fails the chain rule.
func TestAttenuation(t *testing.T) {
	v := loadVectors(t)
	rootKey, id, preimage := vectorInputs(t, v)
	m := Mint(rootKey, id, v.Constants.Caveats)
	before := time.Unix(1700000000, 0)

	// Tighten expiry: still verifies against an earlier Now.
	m.Attenuate("example_valid_until=1800000000")
	if !m.VerifySignature(rootKey) {
		t.Fatal("attenuated macaroon fails signature")
	}
	if err := VerifyToken(rootKey, m, preimage, VerifyOptions{Service: "example", Now: before}); err != nil {
		t.Fatalf("tightened token rejected: %v", err)
	}
	// Now past the tightened expiry but inside the original: must fail.
	if err := VerifyToken(rootKey, m, preimage,
		VerifyOptions{Service: "example", Now: time.Unix(1800000001, 0)}); err == nil {
		t.Fatal("tightened expiry not enforced")
	}

	// Loosening (later valid_until than the previous) violates the chain.
	m2 := Mint(rootKey, id, v.Constants.Caveats)
	m2.Attenuate("example_valid_until=1999999999")
	if err := VerifyToken(rootKey, m2, preimage,
		VerifyOptions{Service: "example", Now: before}); err == nil {
		t.Fatal("loosened valid_until accepted")
	}

	// Expanding the services list violates the subset rule.
	m3 := Mint(rootKey, id, v.Constants.Caveats)
	m3.Attenuate("services=example:0,extra:0")
	if err := VerifyToken(rootKey, m3, preimage,
		VerifyOptions{Service: "example", Now: before}); err == nil {
		t.Fatal("expanded services accepted")
	}
}

// TestParseChallenge pins the client-side challenge parser to the vector
// header lines and covers rejection modes.
func TestParseChallenge(t *testing.T) {
	v := loadVectors(t)
	rootKey, _, _ := vectorInputs(t, v)

	for i, line := range v.Headers.Challenge {
		value := strings.TrimPrefix(line, "WWW-Authenticate: ")
		ch, ok, err := ParseChallenge(value)
		if !ok || err != nil {
			t.Fatalf("challenge[%d]: ok=%v err=%v", i, ok, err)
		}
		wantScheme := SchemeLSAT
		if i == 1 {
			wantScheme = SchemeL402
		}
		if ch.Scheme != wantScheme {
			t.Fatalf("challenge[%d]: scheme %q", i, ch.Scheme)
		}
		if ch.Invoice != v.Constants.Invoice || ch.MacaroonB64 != v.Derived.MacaroonBase64 {
			t.Fatalf("challenge[%d]: values differ", i)
		}
		if !ch.Macaroon.VerifySignature(rootKey) {
			t.Fatalf("challenge[%d]: parsed macaroon fails signature", i)
		}
	}

	// Liberal parameter order.
	swapped := `L402 invoice="` + v.Constants.Invoice + `", macaroon="` + v.Derived.MacaroonBase64 + `"`
	if ch, ok, err := ParseChallenge(swapped); !ok || err != nil || ch.Invoice != v.Constants.Invoice {
		t.Fatalf("swapped params: ok=%v err=%v", ok, err)
	}

	// Not-ours vs malformed.
	if _, ok, _ := ParseChallenge(`Bearer realm="x"`); ok {
		t.Fatal("Bearer parsed as L402 challenge")
	}
	if _, ok, err := ParseChallenge(`L402 macaroon="!!notb64!!", invoice="lndcr1..."`); !ok || err == nil {
		t.Fatal("bad macaroon not reported as error")
	}
	if _, ok, err := ParseChallenge(`L402 macaroon="` + v.Derived.MacaroonBase64 + `"`); !ok || err == nil {
		t.Fatal("missing invoice not reported as error")
	}

	// FindChallenge picks the first well-formed one and skips foreign schemes.
	ch, err := FindChallenge([]string{`Basic realm="x"`,
		strings.TrimPrefix(v.Headers.Challenge[0], "WWW-Authenticate: ")})
	if err != nil || ch == nil || ch.Scheme != SchemeLSAT {
		t.Fatalf("FindChallenge: %+v err=%v", ch, err)
	}
	if ch, err := FindChallenge([]string{`Bearer x`}); ch != nil || err != nil {
		t.Fatalf("FindChallenge on foreign schemes: %+v err=%v", ch, err)
	}
}

// TestIdentifierErrors covers decode failure modes.
func TestIdentifierErrors(t *testing.T) {
	if _, err := DecodeIdentifier(make([]byte, 65)); err == nil {
		t.Fatal("short identifier accepted")
	}
	bad := make([]byte, 66)
	bad[1] = 1 // version 1
	if _, err := DecodeIdentifier(bad); err == nil {
		t.Fatal("future version accepted")
	}
}
