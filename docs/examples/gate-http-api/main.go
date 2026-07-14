// gate-http-api shows the smallest dcr402 seller integration: wrap one
// net/http handler so it charges DCR over Lightning per call. A buyer that
// requests /paid gets a 402 with the payment challenge; after paying the
// invoice it retries with the proof and receives the response plus a
// reusable credential. See the walkthrough in README.md.
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"

	dcr402 "github.com/karamble/dcr402/lib"
	dcr402lnd "github.com/karamble/dcr402/lib/dcrlnd"
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

	// The seller's dcrlnd, reached with the stock invoice.macaroon (no
	// spend permission). PayTo is the node identity pubkey (lncli getinfo).
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

	st, err := store.OpenSQLite(env("DB", "gate-http-api.db"))
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	gate, err := dcr402.New(dcr402.Config{
		Backend: backend,
		Store:   st,
		Network: net,
		PayTo:   os.Getenv("PAYTO"),
		Service: "gate-http-api",
	})
	if err != nil {
		log.Fatalf("gate: %v", err)
	}

	price, _ := strconv.ParseInt(env("PRICE_ATOMS", "250000"), 10, 64) // 0.0025 DCR

	paid := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this response cost you 0.0025 DCR\n"))
	})

	mux := http.NewServeMux()
	mux.Handle("/paid", gate.Require(price)(paid)) // one line to charge for it
	mux.HandleFunc("/free", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this endpoint is free\n"))
	})

	addr := env("LISTEN", "127.0.0.1:8080")
	log.Printf("listening on %s (paid /paid at %d atoms, free /free)", addr, price)
	log.Fatal(http.ListenAndServe(addr, mux))
}
