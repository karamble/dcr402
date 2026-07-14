package l402

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// TestVerifyCaveatsFailClosed: an absent caveat class does not silently
// authorize — a token with no services caveat cannot bind a service, and a
// capability requirement needs a capabilities caveat.
func TestVerifyCaveatsFailClosed(t *testing.T) {
	var rootKey [32]byte
	pb, _ := hex.DecodeString(strings.Repeat("11", 32))
	id := Identifier{Version: IdentifierVersion, PaymentHash: sha256.Sum256(pb), TokenID: [32]byte{0x33}}
	preimage := strings.Repeat("11", 32)
	until := time.Now().Add(time.Hour)
	now := time.Now()

	noServices := Mint(rootKey, id, []string{ValidUntilCaveat("svc", until)})
	if err := VerifyToken(rootKey, noServices, preimage, VerifyOptions{Service: "svc", Now: now}); err == nil {
		t.Fatal("a token with no services caveat must not authorize a service")
	}

	withServices := Mint(rootKey, id, []string{ServicesCaveat("svc", 0), ValidUntilCaveat("svc", until)})
	if err := VerifyToken(rootKey, withServices, preimage, VerifyOptions{Service: "svc", Now: now}); err != nil {
		t.Fatalf("a token with the services caveat should verify: %v", err)
	}

	if err := VerifyToken(rootKey, withServices, preimage,
		VerifyOptions{Service: "svc", Capability: "admin", Now: now}); err == nil {
		t.Fatal("a capability with no capabilities caveat must not authorize")
	}
}
