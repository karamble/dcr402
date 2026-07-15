package dcr402

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/karamble/dcr402/lib/l402"
	"github.com/karamble/dcr402/lib/store"
	"github.com/karamble/dcr402/lib/x402"
)

// ExtensionL402 is the extensions key for credential advertisement and
// delivery (annex §6).
const ExtensionL402 = "l402"

var (
	l402AdvertSchema = json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"tokenTtlSeconds":{"type":"integer","minimum":0}},"required":["tokenTtlSeconds"]}`)

	l402DeliverSchema = json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"authorization":{"type":"string"},"validUntil":{"type":"integer"}},"required":["authorization"]}`)
)

// challengeArtifacts is everything one 402 challenge produces: the wire
// object plus the pieces the transports need (macaroon for WWW-Authenticate,
// invoice string).
type challengeArtifacts struct {
	PaymentRequired x402.PaymentRequired
	Macaroon        *l402.Macaroon
	Invoice         string
}

// mintCredential mints one challenge credential (annex §3): a fresh random
// root key stored under sha256(identifier), the 66-byte identifier over
// paymentHash and tokenID, service caveats, and the TokenTTL expiry.
func (g *Gate) mintCredential(ctx context.Context, paymentHash, tokenID [32]byte, now time.Time) (*l402.Macaroon, string, error) {
	var rootKey [32]byte
	if _, err := rand.Read(rootKey[:]); err != nil {
		return nil, "", err
	}
	id := l402.Identifier{
		Version:     l402.IdentifierVersion,
		PaymentHash: paymentHash,
		TokenID:     tokenID,
	}
	caveats := []string{l402.ServicesCaveat(g.cfg.Service, g.cfg.Tier)}
	if len(g.cfg.Capabilities) > 0 {
		caveats = append(caveats, l402.CapabilitiesCaveat(g.cfg.Service, g.cfg.Capabilities))
	}
	caveats = append(caveats, l402.ValidUntilCaveat(g.cfg.Service, now.Add(g.cfg.TokenTTL)))
	mac := l402.Mint(rootKey, id, caveats)
	if err := g.cfg.Store.PutRootKey(ctx, id.StorageKey(), rootKey); err != nil {
		return nil, "", fmt.Errorf("dcr402: store root key: %w", err)
	}
	return mac, base64.StdEncoding.EncodeToString(mac.MarshalBinary()), nil
}

// buildChallenge creates the invoice, mints the credential (annex §3:
// mint-at-challenge), persists the challenge and root key, and assembles
// the PaymentRequired object. bind, when non-empty, is stored as the
// challenge's Resource in place of resource.URL, decoupling the settlement
// binding key from the wire resource object; empty keeps them fused.
func (g *Gate) buildChallenge(ctx context.Context, resource x402.ResourceInfo, bind string, amountAtoms int64, extra map[string]x402.Extension) (*challengeArtifacts, error) {
	if amountAtoms <= 0 {
		return nil, fmt.Errorf("dcr402: amount must be positive, got %d", amountAtoms)
	}
	if bind == "" {
		bind = resource.URL
	}
	memo := g.cfg.Service + " " + bind
	invoice, paymentHash, err := g.cfg.Backend.CreateInvoice(ctx, amountAtoms, memo, g.cfg.ChallengeTTL)
	if err != nil {
		return nil, fmt.Errorf("dcr402: create invoice: %w", err)
	}

	var tokenID [32]byte
	if _, err := rand.Read(tokenID[:]); err != nil {
		return nil, err
	}
	now := g.now()
	mac, macB64, err := g.mintCredential(ctx, paymentHash, tokenID, now)
	if err != nil {
		return nil, err
	}

	entry := x402.PaymentRequirements{
		Scheme:            x402.SchemeExact,
		Network:           g.cfg.Network.CAIP2,
		Amount:            strconv.FormatInt(amountAtoms, 10),
		Asset:             x402.AssetDCR,
		PayTo:             g.cfg.PayTo,
		MaxTimeoutSeconds: int(g.cfg.ChallengeTTL.Seconds()),
		Extra: x402.MustRaw(x402.LightningExtra{
			AssetTransferMethod: x402.MethodLightning,
			Invoice:             invoice,
			PaymentHash:         hex.EncodeToString(paymentHash[:]),
		}),
	}
	reqJSON, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	if err := g.cfg.Store.PutChallenge(ctx, store.Challenge{
		PaymentHash:  paymentHash,
		Invoice:      invoice,
		MacaroonB64:  macB64,
		RootKeyID:    sha256.Sum256(mac.ID),
		Requirements: reqJSON,
		Resource:     bind,
		AmountAtoms:  amountAtoms,
		CreatedAt:    now,
		ExpiresAt:    now.Add(g.cfg.ChallengeTTL),
	}); err != nil {
		return nil, fmt.Errorf("dcr402: store challenge: %w", err)
	}

	exts := map[string]x402.Extension{
		ExtensionL402: {
			Info: x402.MustRaw(map[string]int64{
				"tokenTtlSeconds": int64(g.cfg.TokenTTL.Seconds()),
			}),
			Schema: l402AdvertSchema,
		},
	}
	for k, v := range extra {
		exts[k] = v
	}
	if g.cfg.PaymentID.Enabled {
		exts[ExtensionPaymentID] = buildPaymentIDAdvert(g.cfg.PaymentID.Required)
	}
	if ext, ok := g.offerExtension(resource.URL, []x402.PaymentRequirements{entry}, now.Add(g.cfg.ChallengeTTL).Unix()); ok {
		exts[ExtensionOfferReceipt] = ext
	}
	pr := x402.PaymentRequired{
		X402Version: x402.Version,
		Error:       "Payment required",
		Resource:    resource,
		Accepts:     []x402.PaymentRequirements{entry},
		Extensions:  exts,
	}
	return &challengeArtifacts{PaymentRequired: pr, Macaroon: mac, Invoice: invoice}, nil
}

// Challenge builds a payment challenge for an arbitrary resource: a fresh
// invoice, a minted credential of record, and the x402 PaymentRequired
// object carrying the lightning method plus any extra extensions. Callers
// embedding the gate in their own transport serve the returned object
// however their envelope requires and settle the paid retry with Settle;
// Require and MCPChallenge are the HTTP and MCP conveniences over the same
// mint.
func (g *Gate) Challenge(ctx context.Context, resource x402.ResourceInfo, amountAtoms int64, extra map[string]x402.Extension) (x402.PaymentRequired, error) {
	return g.ChallengeBound(ctx, resource, "", amountAtoms, extra)
}

// ChallengeBound is Challenge with the settlement binding key decoupled from
// the wire resource: the challenge is stored (and its invoice memo written)
// under bind while the 402 carries resource untouched. Sellers advertising a
// shared endpoint URL (an MCP server multiplexing tools) keep per-tool
// binding by passing the per-tool key here. Empty bind falls back to
// resource.URL — plain Challenge.
func (g *Gate) ChallengeBound(ctx context.Context, resource x402.ResourceInfo, bind string, amountAtoms int64, extra map[string]x402.Extension) (x402.PaymentRequired, error) {
	art, err := g.buildChallenge(ctx, resource, bind, amountAtoms, extra)
	if err != nil {
		return x402.PaymentRequired{}, err
	}
	return art.PaymentRequired, nil
}
