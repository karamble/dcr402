// Package dcrlnd implements dcr402.InvoiceBackend over a dcrlnd node's
// gRPC interface. It needs only the stock invoice.macaroon (invoices:read,
// invoices:write, address:read/write, onchain:read) — no offchain
// permissions, so the backend cannot spend funds.
package dcrlnd

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	chaincfg "github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/txscript/v4/stdscript"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/lnrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	dcr402 "github.com/karamble/dcr402/lib"
)

// Config locates and authenticates the node connection.
type Config struct {
	// Host is the gRPC address, e.g. "localhost:10009".
	Host string
	// TLSCertPath points at the node's tls.cert; TLSCert supplies the PEM
	// bytes directly and takes precedence.
	TLSCertPath string
	TLSCert     []byte
	// MacaroonPath points at invoice.macaroon; Macaroon supplies the raw
	// bytes directly and takes precedence.
	MacaroonPath string
	Macaroon     []byte
	// ChainParams identify the node's network; required only for the
	// OnchainBackend methods (per-address output extraction).
	ChainParams *chaincfg.Params
}

// Client is a dcrlnd-backed InvoiceBackend and OnchainBackend.
type Client struct {
	conn   *grpc.ClientConn
	ln     lnrpc.LightningClient
	params *chaincfg.Params
}

// macaroonCred attaches the macaroon to every RPC, hex-encoded in the
// "macaroon" metadata key, as lnd-family nodes expect.
type macaroonCred struct{ hexMac string }

func (m macaroonCred) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"macaroon": m.hexMac}, nil
}

func (m macaroonCred) RequireTransportSecurity() bool { return true }

// New connects to the node described by cfg.
func New(cfg Config) (*Client, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("dcrlnd: Config.Host is required")
	}
	certPEM := cfg.TLSCert
	if len(certPEM) == 0 {
		if cfg.TLSCertPath == "" {
			return nil, fmt.Errorf("dcrlnd: TLS certificate is required (TLSCert or TLSCertPath)")
		}
		var err error
		certPEM, err = os.ReadFile(cfg.TLSCertPath)
		if err != nil {
			return nil, fmt.Errorf("dcrlnd: reading tls cert: %w", err)
		}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("dcrlnd: tls cert PEM did not parse")
	}

	mac := cfg.Macaroon
	if len(mac) == 0 {
		if cfg.MacaroonPath == "" {
			return nil, fmt.Errorf("dcrlnd: macaroon is required (Macaroon or MacaroonPath)")
		}
		var err error
		mac, err = os.ReadFile(cfg.MacaroonPath)
		if err != nil {
			return nil, fmt.Errorf("dcrlnd: reading macaroon: %w", err)
		}
	}

	conn, err := grpc.NewClient(cfg.Host,
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(pool, "")),
		grpc.WithPerRPCCredentials(macaroonCred{hexMac: hex.EncodeToString(mac)}),
	)
	if err != nil {
		return nil, fmt.Errorf("dcrlnd: dial %s: %w", cfg.Host, err)
	}
	return &Client{conn: conn, ln: lnrpc.NewLightningClient(conn), params: cfg.ChainParams}, nil
}

// CreateInvoice implements dcr402.InvoiceBackend.
func (c *Client) CreateInvoice(ctx context.Context, amountAtoms int64, memo string, expiry time.Duration) (string, [32]byte, error) {
	var hash [32]byte
	if amountAtoms <= 0 {
		return "", hash, fmt.Errorf("dcrlnd: amount must be positive")
	}
	resp, err := c.ln.AddInvoice(ctx, &lnrpc.Invoice{
		Memo:        memo,
		ValueMAtoms: amountAtoms * 1000,
		Expiry:      int64(expiry.Seconds()),
	})
	if err != nil {
		return "", hash, fmt.Errorf("dcrlnd: AddInvoice: %w", err)
	}
	if len(resp.RHash) != 32 {
		return "", hash, fmt.Errorf("dcrlnd: AddInvoice returned %d-byte hash", len(resp.RHash))
	}
	copy(hash[:], resp.RHash)
	return resp.PaymentRequest, hash, nil
}

// LookupInvoice implements dcr402.InvoiceBackend.
func (c *Client) LookupInvoice(ctx context.Context, paymentHash [32]byte) (dcr402.InvoiceStatus, error) {
	inv, err := c.ln.LookupInvoice(ctx, &lnrpc.PaymentHash{RHash: paymentHash[:]})
	if err != nil {
		return dcr402.InvoiceStatus{}, fmt.Errorf("dcrlnd: LookupInvoice: %w", err)
	}
	return dcr402.InvoiceStatus{
		Settled:       inv.State == lnrpc.Invoice_SETTLED,
		AmtPaidMAtoms: inv.AmtPaidMAtoms,
	}, nil
}

// NewDepositAddress implements dcr402.OnchainBackend: a fresh P2PKH
// address from the node's wallet (address:write).
func (c *Client) NewDepositAddress(ctx context.Context) (string, error) {
	resp, err := c.ln.NewAddress(ctx, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_PUBKEY_HASH,
	})
	if err != nil {
		return "", fmt.Errorf("dcrlnd: NewAddress: %w", err)
	}
	return resp.Address, nil
}

// LookupDeposit implements dcr402.OnchainBackend: locates txid among the
// wallet's transactions (onchain:read) and sums the outputs paying address
// exactly, by decoding the raw transaction — verification rule 10 needs
// per-address precision, not the wallet-level aggregate amount.
func (c *Client) LookupDeposit(ctx context.Context, txid, address string) (dcr402.DepositStatus, error) {
	if c.params == nil {
		return dcr402.DepositStatus{}, fmt.Errorf("dcrlnd: Config.ChainParams is required for LookupDeposit")
	}
	txs, err := c.ln.GetTransactions(ctx, &lnrpc.GetTransactionsRequest{})
	if err != nil {
		return dcr402.DepositStatus{}, fmt.Errorf("dcrlnd: GetTransactions: %w", err)
	}
	for _, tx := range txs.Transactions {
		if tx.TxHash != txid {
			continue
		}
		raw, err := hex.DecodeString(tx.RawTxHex)
		if err != nil {
			return dcr402.DepositStatus{}, fmt.Errorf("dcrlnd: raw tx hex: %w", err)
		}
		var msg wire.MsgTx
		if err := msg.Deserialize(bytes.NewReader(raw)); err != nil {
			return dcr402.DepositStatus{}, fmt.Errorf("dcrlnd: decode tx: %w", err)
		}
		var sum int64
		for _, out := range msg.TxOut {
			_, addrs := stdscript.ExtractAddrs(out.Version, out.PkScript, c.params)
			for _, a := range addrs {
				if a.String() == address {
					sum += out.Value
				}
			}
		}
		return dcr402.DepositStatus{
			Found:                true,
			Confirmations:        tx.NumConfirmations,
			AmountToAddressAtoms: sum,
		}, nil
	}
	return dcr402.DepositStatus{}, nil
}

// Close tears down the gRPC connection.
func (c *Client) Close() error { return c.conn.Close() }

var (
	_ dcr402.InvoiceBackend = (*Client)(nil)
	_ dcr402.OnchainBackend = (*Client)(nil)
)
