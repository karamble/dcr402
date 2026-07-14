// pay-client-go consumes a paid resource from Go. It embeds the dcr402-agent
// pay-client (the dual-envelope engine) with a dcrlnd-backed Lightning rail,
// an in-memory credential cache, and an allow-everything policy hook. It
// fetches a 402-protected URL twice: the first call pays, the second rides
// the cached credential for free. See README.md.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	dcr402 "github.com/karamble/dcr402/lib"

	"github.com/karamble/dcr402/agent/payclient"
	"github.com/karamble/dcr402/agent/policy"
	"github.com/karamble/dcr402/agent/rails/ln"
	"github.com/karamble/dcr402/agent/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func network(name string) dcr402.Network {
	switch name {
	case "mainnet":
		return dcr402.Mainnet
	case "testnet3":
		return dcr402.Testnet3
	default:
		return dcr402.Simnet
	}
}

func main() {
	net := network(env("NETWORK", "simnet"))
	url := env("URL", "http://127.0.0.1:8080/paid")

	// The buyer's dcrlnd, reached with a payment-scoped macaroon.
	rail, err := ln.New(ln.Config{
		RPC:          env("DCRLND_RPC", "127.0.0.1:10010"),
		TLSCertPath:  os.Getenv("DCRLND_TLS_CERT"),
		MacaroonPath: os.Getenv("DCRLND_MACAROON"),
		Params:       net.Params,
	})
	if err != nil {
		log.Fatalf("dcrlnd: %v", err)
	}

	client, err := payclient.New(payclient.Config{
		LN:      rail,
		Network: net,
		Tokens:  store.NewMemory(), // per-host credential cache
		// A standalone client has no policy engine, so allow every charge.
		// Enforce your own caps or allowlists here before returning nil.
		Gate: func(ctx context.Context, a policy.Attempt) (int64, error) {
			log.Printf("about to pay %d atoms to %s", a.AmountAtoms, a.Dest)
			return 1, nil
		},
	})
	if err != nil {
		log.Fatalf("payclient: %v", err)
	}

	ctx := context.Background()

	res, err := client.FetchPaid(ctx, url, payclient.FetchOptions{Memo: "buy the resource"})
	if err != nil {
		log.Fatalf("first fetch: %v", err)
	}
	fmt.Printf("first  call: rail=%s paid=%d atoms\n  body: %s\n", res.Rail, res.PaidAtoms, res.Body)

	res, err = client.FetchPaid(ctx, url, payclient.FetchOptions{Memo: "again"})
	if err != nil {
		log.Fatalf("second fetch: %v", err)
	}
	fmt.Printf("second call: rail=%s paid=%d atoms (cached credential)\n  body: %s\n", res.Rail, res.PaidAtoms, res.Body)
}
