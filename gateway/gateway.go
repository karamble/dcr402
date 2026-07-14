package gateway

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	dcr402 "github.com/karamble/dcr402/lib"
	dcr402lnd "github.com/karamble/dcr402/lib/dcrlnd"
	"github.com/karamble/dcr402/lib/ledger"
	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

// TopupPath is where the gateway serves credit top-ups.
const TopupPath = "/_dcr402/topup"

// Deps are the gateway's injectable backends (tests supply fakes;
// NewFromConfig wires the real ones).
type Deps struct {
	Backend dcr402.InvoiceBackend
	Onchain dcr402.OnchainBackend // nil: no on-chain top-ups
	Store   store.Store
	Ledger  ledger.Ledger // nil: no credit accounts
}

// Gateway is a built dcr402d instance.
type Gateway struct {
	cfg     *Config
	gate    *dcr402.Gate
	ups     []*Upstream
	mux     *http.ServeMux
	closers []io.Closer
}

// Build assembles a Gateway from a validated config and dependencies.
func Build(cfg *Config, deps Deps) (*Gateway, error) {
	network, err := cfg.NetworkFor()
	if err != nil {
		return nil, err
	}
	if cfg.Credits.Enabled && deps.Ledger == nil {
		return nil, fmt.Errorf("gateway: credits enabled but no ledger provided")
	}
	ups, err := cfg.ResolveUpstreams()
	if err != nil {
		return nil, err
	}
	upFor := func(path string) *Upstream {
		var best *Upstream
		for _, u := range ups {
			if strings.HasPrefix(path, u.Route) && (best == nil || len(u.Route) > len(best.Route)) {
				best = u
			}
		}
		return best
	}
	gate, err := dcr402.New(dcr402.Config{
		Backend:   deps.Backend,
		Onchain:   deps.Onchain,
		Ledger:    deps.Ledger,
		Store:     deps.Store,
		Network:   network,
		PayTo:     cfg.PayTo,
		Service:   cfg.Service,
		TopupPath: TopupPath,
		// Attach per-upstream service metadata and a bazaar discovery
		// extension to priced 402s so a facilitator can catalog the resource.
		Resource: func(r *http.Request) x402.ResourceInfo {
			ri := x402.ResourceInfo{URL: gatewayURL(r)}
			if u := upFor(r.URL.Path); u != nil {
				ri.ServiceName = u.Discovery.ServiceName
				ri.Description = u.Discovery.Description
				ri.Tags = u.Discovery.Tags
				ri.IconURL = u.Discovery.IconURL
			}
			return ri
		},
		DiscoveryExtension: func(r *http.Request, method string) *x402.Extension {
			if u := upFor(r.URL.Path); u == nil || u.MCP {
				return nil
			}
			ext := dcr402.BuildHTTPDiscovery(method, nil, nil)
			return &ext
		},
		MCPDiscovery: func(tool string) *x402.Extension {
			ext := dcr402.BuildMCPDiscovery(tool, nil, "", "", nil)
			return &ext
		},
	})
	if err != nil {
		return nil, err
	}

	gw := &Gateway{cfg: cfg, gate: gate, ups: ups, mux: http.NewServeMux()}
	for _, u := range ups {
		gw.mux.Handle(u.Route, gw.upstreamHandler(u))
	}
	if cfg.Credits.Enabled {
		opts := dcr402.TopupOptions{Confirmations: cfg.Credits.Confirmations}
		if cfg.Topup.Min != "" {
			if opts.MinAtoms, err = ParseDCR(cfg.Topup.Min); err != nil {
				return nil, fmt.Errorf("config: topup.min: %w", err)
			}
		}
		if cfg.Topup.Max != "" {
			if opts.MaxAtoms, err = ParseDCR(cfg.Topup.Max); err != nil {
				return nil, fmt.Errorf("config: topup.max: %w", err)
			}
		}
		if cfg.Topup.Default != "" {
			if opts.DefaultAtoms, err = ParseDCR(cfg.Topup.Default); err != nil {
				return nil, fmt.Errorf("config: topup.default: %w", err)
			}
		}
		gw.mux.Handle(TopupPath, gate.Topup(opts))
		gw.mux.HandleFunc("/_dcr402/balance", gw.balanceHandler)
	}
	if cfg.Web.StatusPage {
		gw.mux.HandleFunc("/_dcr402/", gw.statusHandler)
		gw.mux.HandleFunc("/_dcr402/info", gw.infoHandler)
	}
	return gw, nil
}

func gatewayURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.Path
}

// NewFromConfig builds a Gateway with real dcrlnd and SQLite backends.
func NewFromConfig(cfg *Config) (*Gateway, error) {
	network, err := cfg.NetworkFor()
	if err != nil {
		return nil, err
	}
	client, err := dcr402lnd.New(dcr402lnd.Config{
		Host:         cfg.LN.Host,
		TLSCertPath:  cfg.LN.TLSCert,
		MacaroonPath: cfg.LN.Macaroon,
		ChainParams:  network.Params,
	})
	if err != nil {
		return nil, err
	}
	st, err := store.OpenSQLite(cfg.Store)
	if err != nil {
		client.Close()
		return nil, err
	}
	deps := Deps{Backend: client, Store: st}
	closers := []io.Closer{client, st}
	if cfg.Credits.Enabled {
		led, err := ledger.OpenSQLite(cfg.Store + ".ledger")
		if err != nil {
			client.Close()
			st.Close()
			return nil, err
		}
		deps.Ledger = led
		closers = append(closers, led)
		if cfg.Credits.Onchain {
			deps.Onchain = client
		}
	}
	gw, err := Build(cfg, deps)
	if err != nil {
		for _, c := range closers {
			c.Close()
		}
		return nil, err
	}
	gw.closers = closers
	return gw, nil
}

// Handler is the gateway's root handler.
func (gw *Gateway) Handler() http.Handler { return gw.mux }

// Close releases backends opened by NewFromConfig.
func (gw *Gateway) Close() error {
	var firstErr error
	for _, c := range gw.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (gw *Gateway) balanceHandler(w http.ResponseWriter, r *http.Request) {
	account, balance, err := gw.gate.CredentialBalance(r.Context(), r.Header.Get("Authorization"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{
			"account":      account,
			"balanceAtoms": balance,
		})
	case errors.Is(err, dcr402.ErrNoCredential):
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"error": "credential required", "topup": TopupPath,
		})
	case errors.Is(err, dcr402.ErrInvalidCredential):
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "invalid or expired credential", "topup": TopupPath,
		})
	default:
		http.Error(w, "balance lookup failed", http.StatusInternalServerError)
	}
}

func (gw *Gateway) infoHandler(w http.ResponseWriter, r *http.Request) {
	network, _ := gw.cfg.NetworkFor()
	type upstreamInfo struct {
		Route            string           `json:"route"`
		Mode             string           `json:"mode"`
		MCP              bool             `json:"mcp,omitempty"`
		Prices           map[string]int64 `json:"pricesAtoms,omitempty"`
		PriceDefault     int64            `json:"priceDefaultAtoms,omitempty"`
		ToolPrices       map[string]int64 `json:"toolPricesAtoms,omitempty"`
		ToolPriceDefault int64            `json:"toolPriceDefaultAtoms,omitempty"`
	}
	infos := make([]upstreamInfo, 0, len(gw.ups))
	for _, u := range gw.ups {
		infos = append(infos, upstreamInfo{
			Route: u.Route, Mode: u.Mode, MCP: u.MCP,
			Prices: u.Prices, PriceDefault: u.PriceDefault,
			ToolPrices: u.ToolPrices, ToolPriceDefault: u.ToolPriceDefault,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":        gw.cfg.Service,
		"network":        network.Name,
		"caip2":          network.CAIP2,
		"upstreams":      infos,
		"creditsEnabled": gw.cfg.Credits.Enabled,
		"topup":          TopupPath,
	})
}

func (gw *Gateway) statusHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/_dcr402/" && r.URL.Path != "/_dcr402" {
		http.NotFound(w, r)
		return
	}
	network, _ := gw.cfg.NetworkFor()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s — dcr402d</title>
<style>body{font:15px/1.5 monospace;max-width:52rem;margin:2rem auto;padding:0 1rem}
table{border-collapse:collapse}td,th{border:1px solid #8884;padding:.25rem .6rem;text-align:left}</style>
<h1>%s</h1>
<p>dcr402d payment gateway · network <b>%s</b> (<code>%s</code>)</p>`,
		gw.cfg.Service, gw.cfg.Service, network.Name, network.CAIP2)

	fmt.Fprint(w, "<h2>Priced routes</h2><table><tr><th>route</th><th>mode</th><th>entry</th><th>atoms</th></tr>")
	for _, u := range gw.ups {
		for k, v := range u.Prices {
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>",
				u.Route, u.Mode, k, strconv.FormatInt(v, 10))
		}
		for k, v := range u.ToolPrices {
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>tool %s</td><td>%s</td></tr>",
				u.Route, u.Mode, k, strconv.FormatInt(v, 10))
		}
		if u.PriceDefault > 0 {
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>(default)</td><td>%d</td></tr>", u.Route, u.Mode, u.PriceDefault)
		}
		if u.ToolPriceDefault > 0 {
			fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>(tool default)</td><td>%d</td></tr>", u.Route, u.Mode, u.ToolPriceDefault)
		}
	}
	fmt.Fprint(w, "</table>")
	if gw.cfg.Credits.Enabled {
		fmt.Fprintf(w, `<h2>Credits</h2>
<p>Top up: <code>GET %s?atoms=N</code> → pay any offered method → settlement returns your credential.<br>
Balance: <code>GET /_dcr402/balance</code> with your <code>Authorization</code> credential.</p>`, TopupPath)
	}
	fmt.Fprint(w, `<p>Machine-readable: <a href="/_dcr402/info">/_dcr402/info</a></p>`)
}
