// Package ln is dcr402-agent's Lightning rail: a dcrlnd client scoped by
// the payment macaroon (offchain:read+write, invoices:read+write,
// info:read — never admin, never on-chain). Invoice decoding happens
// locally via zpay32, so policy can inspect amount and destination without
// trusting any remote party.
package ln

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	chaincfg "github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrlnd/lnrpc"
	"github.com/decred/dcrlnd/lnrpc/routerrpc"
	"github.com/decred/dcrlnd/zpay32"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Config locates the payer dcrlnd node.
type Config struct {
	RPC          string
	TLSCertPath  string
	MacaroonPath string
	// Params identify the network (invoice decode + validation).
	Params *chaincfg.Params
}

// Client is the Lightning rail.
type Client struct {
	conn   *grpc.ClientConn
	ln     lnrpc.LightningClient
	router routerrpc.RouterClient
	params *chaincfg.Params
}

type macaroonCred struct{ hexMac string }

func (m macaroonCred) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"macaroon": m.hexMac}, nil
}
func (m macaroonCred) RequireTransportSecurity() bool { return true }

// New connects to dcrlnd.
func New(cfg Config) (*Client, error) {
	if cfg.Params == nil {
		return nil, fmt.Errorf("ln: Config.Params is required")
	}
	certPEM, err := os.ReadFile(cfg.TLSCertPath)
	if err != nil {
		return nil, fmt.Errorf("ln: tls cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("ln: tls cert did not parse")
	}
	mac, err := os.ReadFile(cfg.MacaroonPath)
	if err != nil {
		return nil, fmt.Errorf("ln: macaroon: %w", err)
	}
	conn, err := grpc.NewClient(cfg.RPC,
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(pool, "")),
		grpc.WithPerRPCCredentials(macaroonCred{hexMac: hex.EncodeToString(mac)}),
	)
	if err != nil {
		return nil, fmt.Errorf("ln: dial %s: %w", cfg.RPC, err)
	}
	return &Client{
		conn:   conn,
		ln:     lnrpc.NewLightningClient(conn),
		router: routerrpc.NewRouterClient(conn),
		params: cfg.Params,
	}, nil
}

// Invoice is a locally decoded BOLT11 invoice.
type Invoice struct {
	// AmountAtoms is 0 for amountless invoices (which policy rejects for
	// payment — the amount is the contract).
	AmountAtoms  int64
	AmountMAtoms int64
	// Destination is the payee node's compressed pubkey, lowercase hex.
	Destination string
	PaymentHash string
	Description string
	Timestamp   time.Time
	Expiry      time.Duration
}

// DecodeInvoice decodes locally via zpay32 — no RPC, no trust.
func (c *Client) DecodeInvoice(invoice string) (Invoice, error) {
	return DecodeInvoice(invoice, c.params)
}

// DecodeInvoice is the standalone decoder (usable without a node
// connection, e.g. by estimate).
func DecodeInvoice(invoice string, params *chaincfg.Params) (Invoice, error) {
	inv, err := zpay32.Decode(invoice, params)
	if err != nil {
		return Invoice{}, fmt.Errorf("ln: decode invoice: %w", err)
	}
	out := Invoice{
		Timestamp: inv.Timestamp,
		Expiry:    inv.Expiry(),
	}
	if inv.MilliAt != nil {
		out.AmountMAtoms = int64(*inv.MilliAt)
		out.AmountAtoms = out.AmountMAtoms / 1000
	}
	if inv.Destination != nil {
		out.Destination = hex.EncodeToString(inv.Destination.SerializeCompressed())
	}
	if inv.PaymentHash != nil {
		out.PaymentHash = hex.EncodeToString(inv.PaymentHash[:])
	}
	if inv.Description != nil {
		out.Description = *inv.Description
	}
	return out, nil
}

// Pay settles the invoice and returns the preimage — the cryptographic
// receipt — plus the routing fee actually paid.
func (c *Client) Pay(ctx context.Context, invoice string, feeLimitAtoms int64, timeout time.Duration) (preimageHex string, feeAtoms int64, err error) {
	if feeLimitAtoms <= 0 {
		feeLimitAtoms = 10_000
	}
	secs := int32(timeout / time.Second)
	if secs <= 0 {
		secs = 60
	}
	stream, err := c.router.SendPaymentV2(ctx, &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice,
		TimeoutSeconds: secs,
		FeeLimitAtoms:  feeLimitAtoms,
	})
	if err != nil {
		return "", 0, fmt.Errorf("ln: SendPaymentV2: %w", err)
	}
	for {
		update, err := stream.Recv()
		if err != nil {
			return "", 0, fmt.Errorf("ln: payment stream: %w", err)
		}
		switch update.Status {
		case lnrpc.Payment_SUCCEEDED:
			return update.PaymentPreimage, update.FeeAtoms, nil
		case lnrpc.Payment_FAILED:
			return "", 0, fmt.Errorf("ln: payment failed: %v", update.FailureReason)
		}
	}
}

// SpendableAtoms is the local channel balance — the agent's hot budget by
// construction.
func (c *Client) SpendableAtoms(ctx context.Context) (int64, error) {
	resp, err := c.ln.ChannelBalance(ctx, &lnrpc.ChannelBalanceRequest{})
	if err != nil {
		return 0, fmt.Errorf("ln: ChannelBalance: %w", err)
	}
	if resp.LocalBalance != nil {
		return int64(resp.LocalBalance.Atoms), nil
	}
	return 0, nil
}

// Info reports the node identity and sync state (info:read).
func (c *Client) Info(ctx context.Context) (pubkey string, synced bool, err error) {
	info, err := c.ln.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return "", false, fmt.Errorf("ln: GetInfo: %w", err)
	}
	return info.IdentityPubkey, info.SyncedToChain, nil
}

// CreateInvoice mints a receive invoice (inbound funding; invoices:write).
func (c *Client) CreateInvoice(ctx context.Context, amountAtoms int64, memo string, expiry time.Duration) (invoice string, paymentHash string, err error) {
	resp, err := c.ln.AddInvoice(ctx, &lnrpc.Invoice{
		Memo:        memo,
		ValueMAtoms: amountAtoms * 1000,
		Expiry:      int64(expiry.Seconds()),
	})
	if err != nil {
		return "", "", fmt.Errorf("ln: AddInvoice: %w", err)
	}
	return resp.PaymentRequest, hex.EncodeToString(resp.RHash), nil
}

// Close tears down the connection.
func (c *Client) Close() error { return c.conn.Close() }
