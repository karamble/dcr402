// Package bazaar implements dcrbazaar: the dcr402 discovery index and
// non-custodial payment-verification service for Decred. It is NOT a settlement
// facilitator - Decred payments settle peer-to-peer at the seller's own node
// (lightning) or by the payer's own on-chain broadcast, so dcrbazaar holds no
// keys and moves no funds. What it provides:
//
//   - a Bazaar-style discovery index of DCR-payable resources
//     (/discovery/resources, /search, /submit); and
//   - verify-as-a-service on the x402 v2 endpoints (/verify, /settle,
//     /supported): the lightning method reuses the same stateless
//     lib.VerifyLightningProof the embedded gate uses (the two never disagree),
//     and the onchain method reads the chain through dcrdata to confirm a
//     payer-broadcast deposit. /settle only notarizes the confirmed payment.
//
// This is the non-custodial verify-assisted topology (T2). Wire behavior is
// specified in ../scheme/.
package bazaar

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	dcr402 "github.com/karamble/dcr402/lib"
)

// Config is the dcrbazaar YAML configuration.
type Config struct {
	// Listen is the HTTP listen address, e.g. ":8444".
	Listen string `yaml:"listen"`
	// TLSCert/TLSKey enable TLS on the listener when both are set.
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
	// Networks are the Decred networks this facilitator serves: any of
	// mainnet, testnet3, simnet. /verify and /settle accept a payment only
	// for a configured network; /supported advertises them all.
	Networks []string `yaml:"networks"`
	// Store is the SQLite path for notarized settlements and the discovery
	// index; empty keeps everything in memory (lost on restart).
	Store string `yaml:"store"`
	// Discovery configures the resource index.
	Discovery DiscoveryConfig `yaml:"discovery"`
	// Onchain enables the on-chain Decred verify-as-a-service: /verify and
	// /settle confirm a payer-broadcast deposit by reading the chain through
	// dcrdata. Disabled by default (dcrbazaar then verifies only the
	// stateless lightning method).
	Onchain OnchainConfig `yaml:"onchain"`
	// APIKeys, when non-empty, gates /verify and /settle behind an
	// X-API-Key header. Empty (the default) leaves the facilitator open, as
	// a self-hosted instance normally is.
	APIKeys []string `yaml:"api_keys"`
}

// OnchainConfig configures the on-chain verify source. dcrbazaar reads the
// chain read-only through a dcrdata Insight API; it holds no keys and issues
// no addresses (the seller minted payTo, the payer self-broadcast the tx).
type OnchainConfig struct {
	// Enabled serves the onchain transfer method.
	Enabled bool `yaml:"enabled"`
	// DcrdataURL is the base URL of a dcrdata instance (e.g.
	// https://dcrdata.decred.org); its /insight/api/tx/{txid} endpoint is read.
	// It MUST match the served network (a mainnet dcrdata for mainnet).
	DcrdataURL string `yaml:"dcrdata_url"`
	// MinConfs is the confirmation floor dcrbazaar enforces regardless of a
	// challenge's requested depth (the effective requirement is the larger of
	// this and the challenge's extra.confirmations). Defaults to 1.
	MinConfs int32 `yaml:"min_confs"`
}

// DiscoveryConfig configures the Bazaar-style discovery index.
type DiscoveryConfig struct {
	// Enabled serves the /discovery endpoints.
	Enabled bool `yaml:"enabled"`
	// PublicSubmit lets anyone POST /discovery/submit. When false, submit
	// requires an API key (and APIKeys must be set for it to be reachable).
	PublicSubmit bool `yaml:"public_submit"`
}

// LoadConfig reads and validates a dcrbazaar YAML config.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dcrbazaar: read config: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("dcrbazaar: parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("dcrbazaar: listen is required")
	}
	if len(c.Networks) == 0 {
		return fmt.Errorf("dcrbazaar: at least one network is required")
	}
	if _, err := c.resolveNetworks(); err != nil {
		return err
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("dcrbazaar: tls_cert and tls_key must be set together")
	}
	// public_submit=false gates submit behind an API key. With no keys set the
	// gate would fall through to open, so reject the ambiguous config.
	if c.Discovery.Enabled && !c.Discovery.PublicSubmit && len(c.APIKeys) == 0 {
		return fmt.Errorf("dcrbazaar: discovery with public_submit=false requires api_keys " +
			"(otherwise submit would be open); set api_keys or public_submit=true")
	}
	if c.Onchain.Enabled {
		if c.Onchain.DcrdataURL == "" {
			return fmt.Errorf("dcrbazaar: onchain.enabled requires onchain.dcrdata_url")
		}
		if c.Onchain.MinConfs <= 0 {
			c.Onchain.MinConfs = 1
		}
	}
	return nil
}

// resolveNetworks maps the configured names to lib networks, preserving order
// and rejecting duplicates and unknown names.
func (c *Config) resolveNetworks() ([]dcr402.Network, error) {
	seen := make(map[string]bool, len(c.Networks))
	out := make([]dcr402.Network, 0, len(c.Networks))
	for _, name := range c.Networks {
		net, ok := networkByName(name)
		if !ok {
			return nil, fmt.Errorf("dcrbazaar: unknown network %q (use mainnet, testnet3, or simnet)", name)
		}
		if seen[net.CAIP2] {
			return nil, fmt.Errorf("dcrbazaar: duplicate network %q", name)
		}
		seen[net.CAIP2] = true
		out = append(out, net)
	}
	return out, nil
}

// networkByName resolves a config network name to a lib network.
func networkByName(name string) (dcr402.Network, bool) {
	switch name {
	case "mainnet":
		return dcr402.Mainnet, true
	case "testnet3":
		return dcr402.Testnet3, true
	case "simnet":
		return dcr402.Simnet, true
	}
	return dcr402.Network{}, false
}
