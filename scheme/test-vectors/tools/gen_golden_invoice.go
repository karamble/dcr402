// gen_golden_invoice generates the deterministic "golden" Decred BOLT11
// invoice used by the dcr402 scheme test vectors: a mainnet invoice whose
// preimage is the fixed constant 0x11 x 32, signed by the standard BOLT11
// test key (published in the BOLT11 spec and dcrlnd's zpay32 test suite —
// never fund it). ECDSA signing is RFC 6979 deterministic, so the output is
// byte-identical on every run.
//
// This generator is not part of any module. To run, create a scratch module
// anywhere outside the repo:
//
//	mkdir /tmp/golden && cd /tmp/golden
//	cp <repo>/scheme/test-vectors/tools/gen_golden_invoice.go main.go
//	go mod init golden
//	go mod edit -require=github.com/decred/dcrlnd@v0.0.0 \
//	  -replace=github.com/decred/dcrlnd=<path to dcrlnd checkout>
//	go mod tidy && go run .
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/decred/dcrd/chaincfg/chainhash"
	chaincfg "github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/decred/dcrlnd/lnwire"
	"github.com/decred/dcrlnd/zpay32"
)

const (
	// The standard BOLT11 test private key. Public knowledge; never fund.
	testPrivKeyHex = "e126f68f7eafcc8b74f54d269fe206be715000f94dac067d1c04a8ca3b2db734"

	// Fixed vector constants (see test-vectors/README.md).
	preimageByte = 0x11
	timestamp    = 1496314658 // matches the zpay32 test-suite timestamp
	amountMAt    = 250000000  // 250,000 atoms = 0.0025 DCR
	expirySec    = 3600
	description  = "dcr402 golden vector"
)

func main() {
	var preimage [32]byte
	for i := range preimage {
		preimage[i] = preimageByte
	}
	paymentHash := sha256.Sum256(preimage[:])

	privKeyBytes, _ := hex.DecodeString(testPrivKeyHex)
	privKey := secp256k1.PrivKeyFromBytes(privKeyBytes)

	inv, err := zpay32.NewInvoice(
		chaincfg.MainNetParams(),
		paymentHash,
		time.Unix(timestamp, 0),
		zpay32.Amount(lnwire.MilliAtom(amountMAt)),
		zpay32.Description(description),
		zpay32.Expiry(expirySec*time.Second),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "NewInvoice:", err)
		os.Exit(1)
	}

	encoded, err := inv.Encode(zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			hash := chainhash.HashB(msg)
			return ecdsa.SignCompact(privKey, hash, true), nil
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Encode:", err)
		os.Exit(1)
	}

	out, _ := json.MarshalIndent(map[string]any{
		"invoice":       encoded,
		"preimage":      hex.EncodeToString(preimage[:]),
		"paymentHash":   hex.EncodeToString(paymentHash[:]),
		"destination":   hex.EncodeToString(privKey.PubKey().SerializeCompressed()),
		"milliAtoms":    fmt.Sprintf("%d", amountMAt),
		"amountAtoms":   fmt.Sprintf("%d", amountMAt/1000),
		"timestamp":     timestamp,
		"expirySeconds": expirySec,
		"description":   description,
	}, "", "  ")
	fmt.Println(string(out))
}
