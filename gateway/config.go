// Package gateway implements dcr402d: a standalone reverse-proxy payment
// gateway that fronts plain HTTP APIs and Streamable-HTTP MCP servers with
// dcr402 payment gating — per-call Lightning payments or prepaid credit
// accounts — configured from a single YAML file.
package gateway

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	dcr402 "github.com/karamble/dcr402/lib"
)

// Modes for an upstream's pricing.
const (
	ModePerCall = "percall"
	ModeCredits = "credits"
)

// Config is the dcr402d YAML configuration.
type Config struct {
	// Listen is the gateway's listen address, e.g. ":8443".
	Listen string `yaml:"listen"`
	// TLSCert/TLSKey enable TLS on the listener when both are set.
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
	// Network is mainnet, testnet3, or simnet.
	Network string `yaml:"network"`
	// Service names this deployment in L402 caveats and challenges.
	Service string `yaml:"service"`
	// PayTo is the seller dcrlnd's identity pubkey (compressed hex).
	PayTo string `yaml:"payto"`
	// Store is the SQLite path for challenges/settlements/tokens; the
	// ledger lives beside it (<store>.ledger).
	Store string `yaml:"store"`

	LN        LNConfig         `yaml:"ln"`
	Credits   CreditsConfig    `yaml:"credits"`
	Topup     TopupConfig      `yaml:"topup"`
	Upstreams []UpstreamConfig `yaml:"upstreams"`
	Web       WebConfig        `yaml:"web"`
}

// LNConfig locates the seller dcrlnd node.
type LNConfig struct {
	Host     string `yaml:"host"`
	TLSCert  string `yaml:"tls_cert"`
	Macaroon string `yaml:"macaroon"` // invoice.macaroon
}

// CreditsConfig enables credit accounts.
type CreditsConfig struct {
	Enabled bool `yaml:"enabled"`
	// Onchain additionally offers on-chain top-ups (needs the node's
	// wallet for deposit addresses).
	Onchain       bool  `yaml:"onchain"`
	Confirmations int32 `yaml:"confirmations"`
}

// TopupConfig bounds top-up amounts (decimal DCR strings).
type TopupConfig struct {
	Min     string `yaml:"min"`
	Max     string `yaml:"max"`
	Default string `yaml:"default"`
}

// WebConfig controls the built-in /_dcr402/ endpoints.
type WebConfig struct {
	StatusPage bool `yaml:"status_page"`
}

// UpstreamConfig is one proxied route.
type UpstreamConfig struct {
	// Route is the path prefix this upstream owns, e.g. "/api/".
	Route string `yaml:"route"`
	// Target is the upstream base URL; the incoming path is joined onto
	// it.
	Target string `yaml:"target"`
	// MCP marks a Streamable-HTTP MCP upstream: JSON-RPC-aware gating
	// (only tools/call is priced, by tool name).
	MCP bool `yaml:"mcp"`
	// Mode is percall (default) or credits.
	Mode string `yaml:"mode"`

	// HTTP pricing: exact "METHOD /path" entries in decimal DCR, plus an
	// optional default for everything else under the route. Paths with no
	// price are forwarded free.
	Prices       map[string]string `yaml:"prices"`
	PriceDefault string            `yaml:"price_default"`

	// MCP pricing: per-tool entries, an optional default for unlisted
	// tools, and tools that stay free.
	ToolPrices       map[string]string `yaml:"tool_prices"`
	ToolPriceDefault string            `yaml:"tool_price_default"`
	FreeTools        []string          `yaml:"free_tools"`

	// Discovery is optional service metadata advertised in the bazaar
	// extension so a facilitator can catalog this upstream's resources.
	Discovery DiscoveryMeta `yaml:"discovery"`
}

// DiscoveryMeta is optional bazaar service metadata for an upstream.
type DiscoveryMeta struct {
	ServiceName string   `yaml:"service_name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	IconURL     string   `yaml:"icon_url"`
}

// atomsPerDCR is 1e8.
const atomsPerDCR = 100_000_000

// ParseDCR converts a decimal DCR string ("0.002", "1", "10.5") to atoms
// exactly — no floats, at most 8 fractional digits.
func ParseDCR(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 {
		return 0, fmt.Errorf("invalid DCR amount %q", s)
	}
	var f int64
	if hasFrac {
		if frac == "" || len(frac) > 8 {
			return 0, fmt.Errorf("invalid DCR amount %q (max 8 decimal places)", s)
		}
		f, err = strconv.ParseInt(frac+strings.Repeat("0", 8-len(frac)), 10, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("invalid DCR amount %q", s)
		}
	}
	if w > (1<<63-1)/atomsPerDCR-1 {
		return 0, fmt.Errorf("DCR amount %q out of range", s)
	}
	atoms := w*atomsPerDCR + f
	if atoms <= 0 {
		return 0, fmt.Errorf("DCR amount %q must be positive", s)
	}
	return atoms, nil
}

// Upstream is a validated, atoms-resolved UpstreamConfig.
type Upstream struct {
	Route            string
	Target           *url.URL
	MCP              bool
	Mode             string
	Prices           map[string]int64 // "METHOD /path" -> atoms
	PriceDefault     int64            // 0 = unpriced (free)
	ToolPrices       map[string]int64
	ToolPriceDefault int64
	FreeTools        map[string]bool
	Discovery        DiscoveryMeta
}

// LoadConfig reads and validates a YAML config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseConfig(raw)
}

func parseConfig(raw []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// NetworkFor maps the config's network name.
func (c *Config) NetworkFor() (dcr402.Network, error) {
	switch c.Network {
	case "mainnet":
		return dcr402.Mainnet, nil
	case "testnet3":
		return dcr402.Testnet3, nil
	case "simnet":
		return dcr402.Simnet, nil
	}
	return dcr402.Network{}, fmt.Errorf("config: unknown network %q", c.Network)
}

func (c *Config) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("config: listen is required")
	}
	if _, err := c.NetworkFor(); err != nil {
		return err
	}
	if c.Service == "" {
		return fmt.Errorf("config: service is required")
	}
	if len(c.PayTo) != 66 {
		return fmt.Errorf("config: payto must be a 33-byte compressed pubkey in hex")
	}
	if c.Store == "" {
		return fmt.Errorf("config: store is required")
	}
	if c.LN.Host == "" || c.LN.TLSCert == "" || c.LN.Macaroon == "" {
		return fmt.Errorf("config: ln.host, ln.tls_cert, and ln.macaroon are required")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("config: tls_cert and tls_key must be set together")
	}
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("config: at least one upstream is required")
	}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if u.Mode == "" {
			u.Mode = ModePerCall
		}
		if u.Mode != ModePerCall && u.Mode != ModeCredits {
			return fmt.Errorf("config: upstream %q: unknown mode %q", u.Route, u.Mode)
		}
		if u.Mode == ModeCredits && !c.Credits.Enabled {
			return fmt.Errorf("config: upstream %q uses credits mode but credits.enabled is false", u.Route)
		}
		if !strings.HasPrefix(u.Route, "/") {
			return fmt.Errorf("config: upstream route %q must start with /", u.Route)
		}
		if t, err := url.Parse(u.Target); err != nil ||
			(t.Scheme != "http" && t.Scheme != "https") || t.Host == "" {
			return fmt.Errorf("config: upstream %q: target must be an absolute http(s) URL, got %q",
				u.Route, u.Target)
		}
		if strings.HasPrefix(u.Route, "/_dcr402") {
			return fmt.Errorf("config: route %q collides with the gateway's own endpoints", u.Route)
		}
	}
	if c.Credits.Confirmations <= 0 {
		c.Credits.Confirmations = 2
	}
	return nil
}

// ResolveUpstreams converts configured upstreams to atoms-priced, sorted
// longest-route-first for dispatch.
func (c *Config) ResolveUpstreams() ([]*Upstream, error) {
	out := make([]*Upstream, 0, len(c.Upstreams))
	for _, uc := range c.Upstreams {
		target, err := url.Parse(uc.Target)
		if err != nil {
			return nil, err
		}
		u := &Upstream{
			Route:      uc.Route,
			Target:     target,
			MCP:        uc.MCP,
			Mode:       uc.Mode,
			Prices:     map[string]int64{},
			ToolPrices: map[string]int64{},
			FreeTools:  map[string]bool{},
			Discovery:  uc.Discovery,
		}
		for k, v := range uc.Prices {
			atoms, err := ParseDCR(v)
			if err != nil {
				return nil, fmt.Errorf("config: upstream %q price %q: %w", uc.Route, k, err)
			}
			u.Prices[normalizePriceKey(k)] = atoms
		}
		if uc.PriceDefault != "" {
			if u.PriceDefault, err = ParseDCR(uc.PriceDefault); err != nil {
				return nil, fmt.Errorf("config: upstream %q price_default: %w", uc.Route, err)
			}
		}
		for k, v := range uc.ToolPrices {
			atoms, err := ParseDCR(v)
			if err != nil {
				return nil, fmt.Errorf("config: upstream %q tool price %q: %w", uc.Route, k, err)
			}
			u.ToolPrices[k] = atoms
		}
		if uc.ToolPriceDefault != "" {
			if u.ToolPriceDefault, err = ParseDCR(uc.ToolPriceDefault); err != nil {
				return nil, fmt.Errorf("config: upstream %q tool_price_default: %w", uc.Route, err)
			}
		}
		for _, name := range uc.FreeTools {
			u.FreeTools[name] = true
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].Route) > len(out[j].Route) })
	return out, nil
}

func normalizePriceKey(k string) string {
	parts := strings.Fields(k)
	if len(parts) == 2 {
		// Clean the path so config keys match the canonicalized request path
		// used at lookup (cleanPath), e.g. a trailing slash cannot desync them.
		return strings.ToUpper(parts[0]) + " " + cleanPath(parts[1])
	}
	return k
}

// PriceFor resolves an HTTP request's price in atoms; 0 means free.
func (u *Upstream) PriceFor(method, path string) int64 {
	if atoms, ok := u.Prices[strings.ToUpper(method)+" "+path]; ok {
		return atoms
	}
	return u.PriceDefault
}

// ToolPriceFor resolves an MCP tool's price in atoms; 0 means free.
func (u *Upstream) ToolPriceFor(tool string) int64 {
	if u.FreeTools[tool] {
		return 0
	}
	if atoms, ok := u.ToolPrices[tool]; ok {
		return atoms
	}
	return u.ToolPriceDefault
}
