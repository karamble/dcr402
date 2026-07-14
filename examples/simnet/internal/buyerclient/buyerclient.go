// Package buyerclient dials the buyer's dcrlnd and pays BOLT11 invoices —
// the client half shared by the simnet drivers.
package buyerclient

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/decred/dcrlnd/lnrpc"
	"github.com/decred/dcrlnd/lnrpc/routerrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type macaroonCred struct{ hexMac string }

func (c macaroonCred) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"macaroon": c.hexMac}, nil
}
func (c macaroonCred) RequireTransportSecurity() bool { return true }

// Dial connects to a dcrlnd node with TLS + macaroon credentials.
func Dial(rpcAddr, tlsCertPath, macaroonPath string) (*grpc.ClientConn, error) {
	certPEM, err := os.ReadFile(tlsCertPath)
	if err != nil {
		return nil, fmt.Errorf("tls cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("tls cert did not parse")
	}
	mac, err := os.ReadFile(macaroonPath)
	if err != nil {
		return nil, fmt.Errorf("macaroon: %w", err)
	}
	return grpc.NewClient(rpcAddr,
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(pool, "")),
		grpc.WithPerRPCCredentials(macaroonCred{hexMac: hex.EncodeToString(mac)}),
	)
}

// PayInvoice settles a BOLT11 invoice via routerrpc and returns the
// preimage hex.
func PayInvoice(ctx context.Context, conn *grpc.ClientConn, invoice string) (string, error) {
	stream, err := routerrpc.NewRouterClient(conn).SendPaymentV2(ctx, &routerrpc.SendPaymentRequest{
		PaymentRequest: invoice,
		TimeoutSeconds: 60,
		FeeLimitAtoms:  10000,
	})
	if err != nil {
		return "", fmt.Errorf("SendPaymentV2: %w", err)
	}
	for {
		update, err := stream.Recv()
		if err != nil {
			return "", fmt.Errorf("payment stream: %w", err)
		}
		switch update.Status {
		case lnrpc.Payment_SUCCEEDED:
			return update.PaymentPreimage, nil
		case lnrpc.Payment_FAILED:
			return "", fmt.Errorf("payment failed: %v", update.FailureReason)
		}
	}
}
