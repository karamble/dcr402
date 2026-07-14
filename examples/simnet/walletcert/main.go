// Command walletcert bootstraps the mutual-TLS client credentials the agent
// needs to reach dcrwallet's gRPC interface: an ed25519 CA, a client
// keypair signed by it, and the CA written as the wallet's clients.pem
// (dcrwallet refuses to serve gRPC without a trusted client CA). Simnet
// setup tooling for the harness — not part of any payment path.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

func main() {
	outDir := flag.String("out", ".", "output directory for client.pem / client-key.pem")
	clientsPEM := flag.String("clients", "", "path to write the wallet's clients.pem (the CA cert)")
	flag.Parse()

	// CA.
	caPub, caPriv, err := ed25519.GenerateKey(rand.Reader)
	must(err)
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{Organization: []string{"dcr402-agent simnet CA"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, caPub, caPriv)
	must(err)

	// Client keypair signed by the CA.
	cliPub, cliPriv, err := ed25519.GenerateKey(rand.Reader)
	must(err)
	cliTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{Organization: []string{"dcr402-agent client"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	caCert, err := x509.ParseCertificate(caDER)
	must(err)
	cliDER, err := x509.CreateCertificate(rand.Reader, cliTmpl, caCert, cliPub, caPriv)
	must(err)
	cliKeyDER, err := x509.MarshalPKCS8PrivateKey(cliPriv)
	must(err)

	writePEM(filepath.Join(*outDir, "client.pem"), "CERTIFICATE", cliDER)
	writePEM(filepath.Join(*outDir, "client-key.pem"), "PRIVATE KEY", cliKeyDER)
	if *clientsPEM != "" {
		writePEM(*clientsPEM, "CERTIFICATE", caDER)
	}
	fmt.Println("wrote client.pem, client-key.pem" + func() string {
		if *clientsPEM != "" {
			return ", " + *clientsPEM
		}
		return ""
	}())
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	must(err)
	return n
}

func writePEM(path, typ string, der []byte) {
	f, err := os.Create(path)
	must(err)
	defer f.Close()
	must(pem.Encode(f, &pem.Block{Type: typ, Bytes: der}))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "walletcert:", err)
		os.Exit(1)
	}
}
