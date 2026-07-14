package dcr402

import (
	chaincfg "github.com/decred/dcrd/chaincfg/v3"
)

// Network binds a Decred network's chain parameters to its CAIP-2
// identifier and BOLT11 invoice prefix (scheme_exact_dcr.md, "Network
// Identifier" and "Lightning Invoice Prefix Binding").
type Network struct {
	Name   string
	CAIP2  string
	HRP    string // BOLT11 human-readable prefix ("ln" + currency)
	Params *chaincfg.Params
}

var (
	// Mainnet is Decred mainnet.
	Mainnet = Network{
		Name:   "mainnet",
		CAIP2:  "bip122:298e5cc3d985bfe7f81dc135f360abe0",
		HRP:    "lndcr",
		Params: chaincfg.MainNetParams(),
	}
	// Testnet3 is the current Decred test network.
	Testnet3 = Network{
		Name:   "testnet3",
		CAIP2:  "bip122:a649dce53918caf422e9c711c858837e",
		HRP:    "lntdcr",
		Params: chaincfg.TestNet3Params(),
	}
	// Simnet is the local simulation network (development only; MUST NOT
	// be advertised by publicly reachable services).
	Simnet = Network{
		Name:   "simnet",
		CAIP2:  "bip122:6bef82c645999585f7255cb02672921a",
		HRP:    "lnsdcr",
		Params: chaincfg.SimNetParams(),
	}
)
