package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	chaincfg "github.com/decred/dcrd/chaincfg/v3"

	"github.com/karamble/dcr402/agent/approve"
	brapprove "github.com/karamble/dcr402/agent/approve/bisonrelay"
	"github.com/karamble/dcr402/agent/payclient"
	"github.com/karamble/dcr402/agent/policy"
	"github.com/karamble/dcr402/agent/rails/ln"
	"github.com/karamble/dcr402/agent/rails/onchain"
	"github.com/karamble/dcr402/agent/store"
)

// closer is the subset of io.Closer the daemon tracks.
type closer interface{ Close() error }

// Daemon is the assembled agent: policy engine, rails, service, approvals.
// Transports (MCP, web) are mounted by the binary on top of Service and
// Registry — the daemon itself is transport-agnostic.
type Daemon struct {
	Config   Config
	Params   *chaincfg.Params
	Policy   *policy.Engine
	Store    store.Store
	Service  *Service
	Registry *approve.Registry
	BR       *brapprove.Approver // nil when the channel is not configured

	closers []closer
}

// New assembles the daemon from a validated config: opens the store, loads
// policy, dials the rails, and wires the payment service and approvals.
// Call Run to start the background loops.
func New(ctx context.Context, cfg Config) (*Daemon, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	network, err := cfg.NetworkFor()
	if err != nil {
		return nil, err
	}

	pol, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		return nil, err
	}
	st, err := store.OpenSQLite(cfg.DB)
	if err != nil {
		return nil, err
	}
	d := &Daemon{Config: cfg, Params: network.Params, Policy: pol, Store: st}
	d.closers = append(d.closers, st)

	// Lightning rail (required).
	lnClient, err := ln.New(ln.Config{
		RPC:          cfg.LN.RPC,
		TLSCertPath:  cfg.LN.TLSCert,
		MacaroonPath: cfg.LN.Macaroon,
		Params:       network.Params,
	})
	if err != nil {
		d.Close()
		return nil, err
	}
	d.closers = append(d.closers, lnClient)

	// On-chain rail (optional).
	var onchainRail OnchainRail
	if cfg.Wallet.RPC != "" {
		pass, err := os.ReadFile(cfg.Wallet.PassphraseFile)
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("agent: reading wallet passphrase: %w", err)
		}
		wc, err := onchain.New(ctx, onchain.Config{
			RPC:        cfg.Wallet.RPC,
			TLSCert:    cfg.Wallet.TLSCert,
			ClientCert: cfg.Wallet.ClientCert,
			ClientKey:  cfg.Wallet.ClientKey,
			Account:    cfg.Wallet.Account,
			Passphrase: pass,
		})
		if err != nil {
			d.Close()
			return nil, err
		}
		d.closers = append(d.closers, wc)
		onchainRail = wc
	}

	// Approvals: web always available; Bison Relay when configured.
	approvers := []approve.Approver{approve.WebApprover{}}
	d.Registry = approve.NewRegistry(st, time.Now, approvers...)
	// (BR approver is appended after the registry exists, since it needs it.)

	// Pay client (fetch_paid). Its HTTP transport refuses the agent's own
	// control API and other internal services, so paid fetches reach only
	// external endpoints.
	pay, err := payclient.New(payclient.Config{
		LN:      lnClient,
		Network: network,
		Tokens:  st,
		Gate:    nil, // set below once the Service exists
		HTTP:    payclient.GuardedClient(cfg.HTTPAddr, cfg.FetchAllow, 60*time.Second),
	})
	if err != nil {
		d.Close()
		return nil, err
	}

	d.Service = NewService(ServiceDeps{
		Policy:   pol,
		Store:    st,
		LN:       lnAdapter{lnClient},
		Onchain:  onchainRail,
		Pay:      pay,
		Approval: d.Registry,
		Now:      time.Now,
	})
	pay.SetGate(d.Service.Gatekeeper())

	if cfg.BR.RPC != "" {
		br, err := brapprove.New(brapprove.Config{
			RPC:        cfg.BR.RPC,
			ServerCert: cfg.BR.ServerCert,
			ClientCert: cfg.BR.ClientCert,
			ClientKey:  cfg.BR.ClientKey,
			User:       cfg.BR.User,
			Pass:       cfg.BR.Pass,
			Owner:      cfg.BR.Owner,
			Registry:   d.Registry,
			Store:      st,
			Freezer:    d.Service,
		})
		if err != nil {
			d.Close()
			return nil, err
		}
		d.Registry.AddApprover(br)
		d.BR = br
	}
	return d, nil
}

// Run starts the background loops (approval expiry, Bison Relay consumer)
// and blocks until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	go d.Registry.RunExpiry(ctx, 30*time.Second)
	if d.BR != nil {
		go func() { _ = d.BR.Run(ctx) }()
	}
	<-ctx.Done()
	return ctx.Err()
}

// Reload re-reads the policy file (SIGHUP).
func (d *Daemon) Reload() error { return d.Policy.Reload() }

// Close releases every backend opened during assembly.
func (d *Daemon) Close() error {
	var firstErr error
	for i := len(d.closers) - 1; i >= 0; i-- {
		if err := d.closers[i].Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// lnAdapter bridges *ln.Client to the Service's LNRail (mapping the
// ln.Invoice type to the service's LNInvoice).
type lnAdapter struct{ c *ln.Client }

func (a lnAdapter) DecodeInvoice(invoice string) (LNInvoice, error) {
	inv, err := a.c.DecodeInvoice(invoice)
	if err != nil {
		return LNInvoice{}, err
	}
	return LNInvoice{
		AmountAtoms: inv.AmountAtoms,
		Destination: inv.Destination,
		PaymentHash: inv.PaymentHash,
		Description: inv.Description,
		Timestamp:   inv.Timestamp,
		Expiry:      inv.Expiry,
	}, nil
}

func (a lnAdapter) Pay(ctx context.Context, invoice string, feeLimitAtoms int64, timeout time.Duration) (string, int64, error) {
	return a.c.Pay(ctx, invoice, feeLimitAtoms, timeout)
}
func (a lnAdapter) SpendableAtoms(ctx context.Context) (int64, error) {
	return a.c.SpendableAtoms(ctx)
}
func (a lnAdapter) Info(ctx context.Context) (string, bool, error) { return a.c.Info(ctx) }
func (a lnAdapter) CreateInvoice(ctx context.Context, amountAtoms int64, memo string, expiry time.Duration) (string, string, error) {
	return a.c.CreateInvoice(ctx, amountAtoms, memo, expiry)
}
