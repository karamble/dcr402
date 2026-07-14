// Package agent is dcr402-agent: a wallet-as-MCP-server signing broker
// that lets any MCP-capable agent spend Decred within hard, owner-set
// policy. The agent holds no keys: Lightning payments go through dcrlnd
// behind a payment-scoped macaroon, on-chain sends through a dedicated
// dcrwallet account over mutual-TLS gRPC, and every attempt — allowed or
// blocked — is written to the audit store with its full policy trace
// before anything is signed.
//
// The package (and its subpackages) are designed for embedding: the
// cmd/dcr402-agent binary is one thin consumer, the MCP surface, web
// dashboard, and Bison Relay approver are optional plug-ins, and the
// Approver seam accepts any implementation.
package agent

import (
	"fmt"
	"os"
	"strings"

	dcr402 "github.com/karamble/dcr402/lib"
)

// MCP transport modes.
const (
	MCPStdio = "stdio"
	MCPHTTP  = "http"
	MCPBoth  = "both"
)

// LNConfig locates the payer dcrlnd node. The macaroon is the
// payment-scoped one (offchain:read+write, invoices:read+write,
// info:read) — never admin.
type LNConfig struct {
	RPC      string
	TLSCert  string
	Macaroon string
}

// WalletConfig locates the dcrwallet gRPC endpoint for the on-chain rail
// (mutual TLS). Empty RPC disables the rail.
type WalletConfig struct {
	RPC            string
	TLSCert        string // wallet's rpc.cert
	ClientCert     string // client keypair; its CA must be in the wallet's clients.pem
	ClientKey      string
	Account        string // dedicated account, e.g. "agent"
	PassphraseFile string // private passphrase; read at start, held in memory
}

// BRConfig locates a Bison Relay clientrpc endpoint (any brclient with
// [clientrpc] enabled) for approval escalation. Empty RPC disables the
// channel.
type BRConfig struct {
	RPC        string // wss://127.0.0.1:7676/ws
	ServerCert string
	ClientCert string
	ClientKey  string
	User       string
	Pass       string
	Owner      string // owner's 64-hex user id; a nick is rejected
}

// Config configures the daemon. All fields have DCR402_AGENT_* (or rail-
// specific) environment counterparts; see FromEnv.
type Config struct {
	Network   string // mainnet | testnet3 | simnet
	MCP       string // stdio | http | both
	HTTPAddr  string // serves MCP streamable HTTP at /mcp, web UI at /, API at /api/
	HTTPToken string // bearer token for MCP + the whole web API
	// FetchAllow lists "host:port" targets fetch_paid may reach even though
	// they resolve to loopback/private addresses (e.g. a local gateway in
	// development). Everything internal is refused by default; the agent's own
	// address is refused regardless.
	FetchAllow []string
	DB         string
	Policy     string

	LN     LNConfig
	Wallet WalletConfig
	BR     BRConfig
}

// splitList parses a comma-separated environment value into a trimmed,
// empty-free list.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// FromEnv builds a Config from the environment (spec §8).
func FromEnv() Config {
	get := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return def
	}
	return Config{
		Network:    get("DCR402_AGENT_NETWORK", "mainnet"),
		MCP:        get("DCR402_AGENT_MCP", MCPHTTP),
		HTTPAddr:   get("DCR402_AGENT_HTTP_ADDR", "127.0.0.1:9377"),
		HTTPToken:  os.Getenv("DCR402_AGENT_HTTP_TOKEN"),
		FetchAllow: splitList(os.Getenv("DCR402_AGENT_FETCH_ALLOW")),
		DB:         get("DCR402_AGENT_DB", "dcr402-agent.db"),
		Policy:     get("DCR402_AGENT_POLICY", "policy.yaml"),
		LN: LNConfig{
			RPC:      os.Getenv("DCRLND_RPC"),
			TLSCert:  os.Getenv("DCRLND_TLS_CERT"),
			Macaroon: os.Getenv("DCRLND_MACAROON"),
		},
		Wallet: WalletConfig{
			RPC:            os.Getenv("DCRWALLET_RPC"),
			TLSCert:        os.Getenv("DCRWALLET_TLS_CERT"),
			ClientCert:     os.Getenv("DCRWALLET_CLIENT_CERT"),
			ClientKey:      os.Getenv("DCRWALLET_CLIENT_KEY"),
			Account:        get("DCRWALLET_ACCOUNT", "agent"),
			PassphraseFile: os.Getenv("DCRWALLET_PASSPHRASE_FILE"),
		},
		BR: BRConfig{
			RPC:        os.Getenv("BR_CLIENTRPC"),
			ServerCert: os.Getenv("BR_SERVER_CERT"),
			ClientCert: os.Getenv("BR_CLIENT_CERT"),
			ClientKey:  os.Getenv("BR_CLIENT_KEY"),
			User:       os.Getenv("BR_RPCUSER"),
			Pass:       os.Getenv("BR_RPCPASS"),
			Owner:      os.Getenv("BR_OWNER"),
		},
	}
}

// NetworkFor maps Config.Network.
func (c *Config) NetworkFor() (dcr402.Network, error) {
	switch c.Network {
	case "mainnet":
		return dcr402.Mainnet, nil
	case "testnet3":
		return dcr402.Testnet3, nil
	case "simnet":
		return dcr402.Simnet, nil
	}
	return dcr402.Network{}, fmt.Errorf("agent: unknown network %q", c.Network)
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	if _, err := c.NetworkFor(); err != nil {
		return err
	}
	switch c.MCP {
	case MCPStdio, MCPHTTP, MCPBoth:
	default:
		return fmt.Errorf("agent: MCP mode must be stdio, http, or both (got %q)", c.MCP)
	}
	if c.MCP != MCPStdio && c.HTTPAddr == "" {
		return fmt.Errorf("agent: HTTPAddr is required for the http MCP mode")
	}
	if c.LN.RPC == "" || c.LN.TLSCert == "" || c.LN.Macaroon == "" {
		return fmt.Errorf("agent: DCRLND_RPC, DCRLND_TLS_CERT, and DCRLND_MACAROON are required")
	}
	if c.Wallet.RPC != "" {
		if c.Wallet.TLSCert == "" || c.Wallet.ClientCert == "" || c.Wallet.ClientKey == "" {
			return fmt.Errorf("agent: the on-chain rail needs DCRWALLET_TLS_CERT, _CLIENT_CERT, and _CLIENT_KEY")
		}
		if c.Wallet.Account == "" {
			return fmt.Errorf("agent: DCRWALLET_ACCOUNT is required for the on-chain rail")
		}
	}
	if c.BR.RPC != "" {
		if c.BR.ServerCert == "" || c.BR.ClientCert == "" || c.BR.ClientKey == "" {
			return fmt.Errorf("agent: the Bison Relay channel needs BR_SERVER_CERT, BR_CLIENT_CERT, and BR_CLIENT_KEY")
		}
		if c.BR.Owner == "" {
			return fmt.Errorf("agent: BR_OWNER is required for the Bison Relay channel")
		}
	}
	if c.DB == "" || c.Policy == "" {
		return fmt.Errorf("agent: DB and Policy paths are required")
	}
	return nil
}
