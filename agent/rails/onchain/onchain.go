// Package onchain is dcr402-agent's on-chain rail: a dcrwallet gRPC client
// (mutual TLS) that spends exclusively from one dedicated account — the
// account balance is the on-chain exposure ceiling by construction. The
// private passphrase is supplied per signing call and never persisted.
package onchain

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	pb "decred.org/dcrwallet/v5/rpc/walletrpc"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Config locates and authenticates the wallet connection.
type Config struct {
	RPC        string
	TLSCert    string // wallet's rpc.cert
	ClientCert string // client keypair; CA must be in the wallet's clients.pem
	ClientKey  string
	// Account is the dedicated spending account name (e.g. "agent").
	Account string
	// Passphrase is the wallet private passphrase, held in memory only.
	Passphrase []byte
}

// Client is the on-chain rail.
type Client struct {
	conn       *grpc.ClientConn
	w          pb.WalletServiceClient
	account    uint32
	passphrase []byte
}

// New connects and resolves the configured account (which must already
// exist; see CreateAccount for setup tooling).
func New(ctx context.Context, cfg Config) (*Client, error) {
	conn, err := dial(cfg)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, w: pb.NewWalletServiceClient(conn), passphrase: cfg.Passphrase}
	acct, err := c.w.AccountNumber(ctx, &pb.AccountNumberRequest{AccountName: cfg.Account})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("onchain: account %q not found (create it first, e.g. with the harness or CreateAccount): %w", cfg.Account, err)
	}
	c.account = acct.AccountNumber
	return c, nil
}

func dial(cfg Config) (*grpc.ClientConn, error) {
	serverCAs := x509.NewCertPool()
	pem, err := os.ReadFile(cfg.TLSCert)
	if err != nil {
		return nil, fmt.Errorf("onchain: wallet tls cert: %w", err)
	}
	if !serverCAs.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("onchain: wallet tls cert did not parse")
	}
	keypair, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("onchain: client keypair: %w", err)
	}
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{keypair},
		RootCAs:      serverCAs,
	})
	conn, err := grpc.NewClient(cfg.RPC, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("onchain: dial %s: %w", cfg.RPC, err)
	}
	return conn, nil
}

// CreateAccount creates the dedicated account on a wallet — setup tooling
// for operators and the simnet harness, not part of the payment path.
func CreateAccount(ctx context.Context, cfg Config) (uint32, error) {
	conn, err := dial(cfg)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	w := pb.NewWalletServiceClient(conn)
	if acct, err := w.AccountNumber(ctx, &pb.AccountNumberRequest{AccountName: cfg.Account}); err == nil {
		return acct.AccountNumber, nil // already exists
	}
	resp, err := w.NextAccount(ctx, &pb.NextAccountRequest{
		Passphrase:  cfg.Passphrase,
		AccountName: cfg.Account,
	})
	if err != nil {
		return 0, fmt.Errorf("onchain: NextAccount: %w", err)
	}
	return resp.AccountNumber, nil
}

// BalanceAtoms reports the dedicated account's balance.
func (c *Client) BalanceAtoms(ctx context.Context, requiredConfs int32) (spendable, total int64, err error) {
	resp, err := c.w.Balance(ctx, &pb.BalanceRequest{
		AccountNumber:         c.account,
		RequiredConfirmations: requiredConfs,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("onchain: Balance: %w", err)
	}
	return resp.Spendable, resp.Total, nil
}

// NewAddress returns a fresh external receive address for the account.
func (c *Client) NewAddress(ctx context.Context) (string, error) {
	resp, err := c.w.NextAddress(ctx, &pb.NextAddressRequest{
		Account:   c.account,
		Kind:      pb.NextAddressRequest_BIP0044_EXTERNAL,
		GapPolicy: pb.NextAddressRequest_GAP_POLICY_IGNORE,
	})
	if err != nil {
		return "", fmt.Errorf("onchain: NextAddress: %w", err)
	}
	return resp.Address, nil
}

// Send pays amountAtoms to address from the dedicated account —
// construct (source_account pinned) → sign (per-call passphrase) →
// publish — and returns the DISPLAY-order txid.
func (c *Client) Send(ctx context.Context, address string, amountAtoms int64) (string, error) {
	if amountAtoms <= 0 {
		return "", fmt.Errorf("onchain: amount must be positive")
	}
	constructed, err := c.w.ConstructTransaction(ctx, &pb.ConstructTransactionRequest{
		SourceAccount:         c.account,
		RequiredConfirmations: 1,
		NonChangeOutputs: []*pb.ConstructTransactionRequest_Output{{
			Destination: &pb.ConstructTransactionRequest_OutputDestination{
				Address: address,
			},
			Amount: amountAtoms,
		}},
	})
	if err != nil {
		return "", fmt.Errorf("onchain: ConstructTransaction: %w", err)
	}
	signed, err := c.w.SignTransactions(ctx, &pb.SignTransactionsRequest{
		Passphrase: c.passphrase,
		Transactions: []*pb.SignTransactionsRequest_UnsignedTransaction{{
			SerializedTransaction: constructed.UnsignedTransaction,
		}},
	})
	if err != nil {
		return "", fmt.Errorf("onchain: SignTransactions: %w", err)
	}
	if len(signed.Transactions) != 1 {
		return "", fmt.Errorf("onchain: expected 1 signed transaction, got %d", len(signed.Transactions))
	}
	published, err := c.w.PublishTransaction(ctx, &pb.PublishTransactionRequest{
		SignedTransaction: signed.Transactions[0].Transaction,
	})
	if err != nil {
		return "", fmt.Errorf("onchain: PublishTransaction: %w", err)
	}
	// The wire hash is internal byte order; chainhash.String() renders the
	// display order every other component (and the chain) uses.
	var hash chainhash.Hash
	if err := hash.SetBytes(published.TransactionHash); err != nil {
		return "", fmt.Errorf("onchain: transaction hash: %w", err)
	}
	return hash.String(), nil
}

// Close tears down the connection.
func (c *Client) Close() error { return c.conn.Close() }
