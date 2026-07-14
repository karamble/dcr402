// credit-accounts shows the credit flow: a buyer tops up an account
// (over Lightning or on-chain), then spends from the balance with no payment
// in the hot path. One dcr402.Gate serves both the /topup endpoint and the
// credit-gated tool routes. See README.md.
package main

import (
	"log"
	"net/http"
	"os"

	dcr402 "github.com/karamble/dcr402/lib"
	dcr402lnd "github.com/karamble/dcr402/lib/dcrlnd"
	"github.com/karamble/dcr402/lib/ledger"
	"github.com/karamble/dcr402/lib/store"
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

	// One dcrlnd Client satisfies both the invoice and the on-chain deposit
	// backends, so top-ups can be paid over either rail.
	backend, err := dcr402lnd.New(dcr402lnd.Config{
		Host:         env("DCRLND_RPC", "127.0.0.1:10009"),
		TLSCertPath:  os.Getenv("DCRLND_TLS_CERT"),
		MacaroonPath: os.Getenv("DCRLND_MACAROON"),
		ChainParams:  net.Params,
	})
	if err != nil {
		log.Fatalf("dcrlnd: %v", err)
	}
	defer backend.Close()

	st, err := store.OpenSQLite(env("DB", "credit-accounts.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	led, err := ledger.OpenSQLite(env("DB", "credit-accounts.db") + ".ledger")
	if err != nil {
		log.Fatalf("ledger: %v", err)
	}
	defer led.Close()

	gate, err := dcr402.New(dcr402.Config{
		Backend:   backend,
		Onchain:   backend, // enables the on-chain top-up method
		Ledger:    led,     // enables credit accounts
		Store:     st,
		Network:   net,
		PayTo:     os.Getenv("PAYTO"),
		Service:   "credit-accounts",
		TopupPath: "/topup",
	})
	if err != nil {
		log.Fatalf("gate: %v", err)
	}

	search := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("search results (100000 atoms burned)\n"))
	})

	mux := http.NewServeMux()
	// Top up: a 402 offering both Lightning and on-chain; settlement grants
	// credits and returns the account credential.
	mux.Handle("/topup", gate.Topup(dcr402.TopupOptions{Confirmations: 1}))
	// A credit-gated tool: each call charges the account named by the
	// caller's credential.
	mux.Handle("/search", gate.RequireCredits("search", 100000)(search))

	addr := env("LISTEN", "127.0.0.1:8082")
	log.Printf("listening on %s (top up at /topup, spend at /search)", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
