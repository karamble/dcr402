// Package l402 implements L402 credential minting, parsing, and
// verification for the dcr402 dual envelope (scheme/l402-dual-envelope.md).
//
// Two deliberate ground-truth choices, both documented in the annex:
//
//   - The HMAC chain is the L402 macaroon specification's direct
//     construction — sig_0 = HMAC-SHA256(root_key, identifier) — with no
//     key-derivation prefix. Signature computation is minter-internal, so
//     this does not affect client interoperability.
//   - The binary serialization is macaroon V2 exactly as the reference
//     implementations produce it (libmacaroons, gopkg.in/macaroon.v2),
//     which is what deployed L402 tooling parses.
package l402

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// IdentifierVersion is the current L402 macaroon identifier version.
const IdentifierVersion = 0

// identifierSize is version (2) + payment hash (32) + token id (32).
const identifierSize = 66

// Identifier is the public macaroon identifier (L402 macaroon spec).
type Identifier struct {
	Version     uint16
	PaymentHash [32]byte
	TokenID     [32]byte
}

// Encode serializes the identifier: big-endian version ‖ hash ‖ token id.
func (id Identifier) Encode() []byte {
	out := make([]byte, identifierSize)
	binary.BigEndian.PutUint16(out[0:2], id.Version)
	copy(out[2:34], id.PaymentHash[:])
	copy(out[34:66], id.TokenID[:])
	return out
}

// DecodeIdentifier parses a 66-byte version-0 identifier.
func DecodeIdentifier(raw []byte) (Identifier, error) {
	var id Identifier
	if len(raw) != identifierSize {
		return id, fmt.Errorf("l402: identifier is %d bytes, want %d", len(raw), identifierSize)
	}
	id.Version = binary.BigEndian.Uint16(raw[0:2])
	if id.Version != IdentifierVersion {
		return id, fmt.Errorf("l402: unsupported identifier version %d", id.Version)
	}
	copy(id.PaymentHash[:], raw[2:34])
	copy(id.TokenID[:], raw[34:66])
	return id, nil
}

// StorageKey is the root-key lookup key: SHA-256 of the encoded identifier.
func (id Identifier) StorageKey() [32]byte {
	return sha256.Sum256(id.Encode())
}

// Macaroon is an L402 macaroon: an identifier, an ordered list of
// first-party caveats, and the HMAC-chain signature. Third-party caveats
// are outside L402 and rejected at parse time.
type Macaroon struct {
	Location  string
	ID        []byte
	Caveats   []string
	Signature [32]byte
}

// Identifier decodes the macaroon's ID field.
func (m *Macaroon) Identifier() (Identifier, error) {
	return DecodeIdentifier(m.ID)
}

// chain computes the direct HMAC chain over id and caveats.
func chain(rootKey [32]byte, id []byte, caveats []string) [32]byte {
	mac := hmac.New(sha256.New, rootKey[:])
	mac.Write(id)
	sig := mac.Sum(nil)
	for _, cav := range caveats {
		mac = hmac.New(sha256.New, sig)
		mac.Write([]byte(cav))
		sig = mac.Sum(nil)
	}
	var out [32]byte
	copy(out[:], sig)
	return out
}

// Mint creates a macaroon for id with the given initial caveats.
func Mint(rootKey [32]byte, id Identifier, caveats []string) *Macaroon {
	m := &Macaroon{
		ID:      id.Encode(),
		Caveats: append([]string(nil), caveats...),
	}
	m.Signature = chain(rootKey, m.ID, m.Caveats)
	return m
}

// Attenuate appends a first-party caveat, extending the signature chain.
// Holders can do this without the root key.
func (m *Macaroon) Attenuate(caveat string) {
	mac := hmac.New(sha256.New, m.Signature[:])
	mac.Write([]byte(caveat))
	copy(m.Signature[:], mac.Sum(nil))
	m.Caveats = append(m.Caveats, caveat)
}

// VerifySignature recomputes the chain from rootKey and compares it to the
// macaroon's signature in constant time.
func (m *Macaroon) VerifySignature(rootKey [32]byte) bool {
	want := chain(rootKey, m.ID, m.Caveats)
	return hmac.Equal(want[:], m.Signature[:])
}

// --- macaroon V2 binary serialization (libmacaroons / macaroon.v2) --------

const (
	fieldEOS            = 0
	fieldLocation       = 1
	fieldIdentifier     = 2
	fieldVerificationID = 4
	fieldSignature      = 6
)

func appendVarint(buf []byte, n uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	w := binary.PutUvarint(tmp[:], n)
	return append(buf, tmp[:w]...)
}

func appendPacket(buf []byte, fieldType uint64, data []byte) []byte {
	buf = appendVarint(buf, fieldType)
	buf = appendVarint(buf, uint64(len(data)))
	return append(buf, data...)
}

// MarshalBinary serializes the macaroon in V2 wire format: version byte
// 0x02; [location] identifier EOS; per caveat: cid EOS (first-party caveats
// carry no verification-id field); EOS ending the caveat list; signature.
func (m *Macaroon) MarshalBinary() []byte {
	buf := []byte{2}
	if m.Location != "" {
		buf = appendPacket(buf, fieldLocation, []byte(m.Location))
	}
	buf = appendPacket(buf, fieldIdentifier, m.ID)
	buf = append(buf, fieldEOS)
	for _, cav := range m.Caveats {
		buf = appendPacket(buf, fieldIdentifier, []byte(cav))
		buf = append(buf, fieldEOS)
	}
	buf = append(buf, fieldEOS)
	buf = appendPacket(buf, fieldSignature, m.Signature[:])
	return buf
}

type packet struct {
	fieldType uint64
	data      []byte
}

func parsePacket(data []byte) ([]byte, packet, error) {
	ft, n := binary.Uvarint(data)
	if n <= 0 {
		return nil, packet{}, fmt.Errorf("l402: bad field type varint")
	}
	data = data[n:]
	p := packet{fieldType: ft}
	if ft == fieldEOS {
		return data, p, nil
	}
	length, n := binary.Uvarint(data)
	if n <= 0 {
		return nil, packet{}, fmt.Errorf("l402: bad length varint")
	}
	data = data[n:]
	if length > uint64(len(data)) {
		return nil, packet{}, fmt.Errorf("l402: field extends past end of buffer")
	}
	p.data = data[:length]
	return data[length:], p, nil
}

func parseSection(data []byte) ([]byte, []packet, error) {
	var packets []packet
	for {
		if len(data) == 0 {
			return nil, nil, fmt.Errorf("l402: section extends past end of buffer")
		}
		rest, p, err := parsePacket(data)
		if err != nil {
			return nil, nil, err
		}
		data = rest
		if p.fieldType == fieldEOS {
			return data, packets, nil
		}
		packets = append(packets, p)
	}
}

// UnmarshalBinary parses a V2 macaroon. Third-party caveats (non-empty
// verification id) are rejected: L402 uses first-party caveats only.
func UnmarshalBinary(raw []byte) (*Macaroon, error) {
	if len(raw) == 0 || raw[0] != 2 {
		return nil, fmt.Errorf("l402: not a V2 macaroon")
	}
	data := raw[1:]

	data, header, err := parseSection(data)
	if err != nil {
		return nil, err
	}
	m := &Macaroon{}
	if len(header) > 0 && header[0].fieldType == fieldLocation {
		m.Location = string(header[0].data)
		header = header[1:]
	}
	if len(header) != 1 || header[0].fieldType != fieldIdentifier {
		return nil, fmt.Errorf("l402: invalid macaroon header")
	}
	m.ID = append([]byte(nil), header[0].data...)

	for {
		rest, section, err := parseSection(data)
		if err != nil {
			return nil, err
		}
		data = rest
		if len(section) == 0 {
			break // end of caveat list
		}
		if section[0].fieldType == fieldLocation {
			return nil, fmt.Errorf("l402: third-party caveats not supported")
		}
		if section[0].fieldType != fieldIdentifier {
			return nil, fmt.Errorf("l402: caveat missing identifier")
		}
		if len(section) > 1 {
			return nil, fmt.Errorf("l402: third-party caveats not supported")
		}
		m.Caveats = append(m.Caveats, string(section[0].data))
	}

	data, sig, err := parsePacket(data)
	if err != nil {
		return nil, err
	}
	if sig.fieldType != fieldSignature || len(sig.data) != 32 {
		return nil, fmt.Errorf("l402: invalid signature field")
	}
	copy(m.Signature[:], sig.data)
	if len(data) != 0 {
		return nil, fmt.Errorf("l402: trailing bytes after signature")
	}
	return m, nil
}

// --- Caveats ---------------------------------------------------------------

// Caveat condition conventions (L402 macaroon spec + dcr402 annex §3/§7).
const (
	CaveatServices      = "services"
	CaveatValidUntilSfx = "_valid_until"
	CaveatCapsSfx       = "_capabilities"
	CaveatCreditAccount = "credit_account"
)

// ServicesCaveat renders "services=name:tier".
func ServicesCaveat(service string, tier int) string {
	return fmt.Sprintf("%s=%s:%d", CaveatServices, service, tier)
}

// ValidUntilCaveat renders "<service>_valid_until=<unix>".
func ValidUntilCaveat(service string, t time.Time) string {
	return fmt.Sprintf("%s%s=%d", service, CaveatValidUntilSfx, t.Unix())
}

// CapabilitiesCaveat renders "<service>_capabilities=a,b,c".
func CapabilitiesCaveat(service string, caps []string) string {
	return fmt.Sprintf("%s%s=%s", service, CaveatCapsSfx, strings.Join(caps, ","))
}

// VerifyOptions parameterize caveat evaluation.
type VerifyOptions struct {
	// Service is the service name the request targets (required).
	Service string
	// Capability is the operation being exercised; empty skips the
	// capabilities check beyond attenuation-chain validity.
	Capability string
	// Now is the evaluation time; zero means time.Now().
	Now time.Time
}

// VerifyToken performs the full L402 credential check from the annex §4:
// signature chain, preimage binding, and caveat satisfaction. preimageHex is
// the hex preimage from the Authorization header. Unknown caveat conditions
// are skipped, per the L402 specification.
func VerifyToken(rootKey [32]byte, m *Macaroon, preimageHex string, opts VerifyOptions) error {
	if !m.VerifySignature(rootKey) {
		return fmt.Errorf("l402: signature mismatch")
	}
	id, err := m.Identifier()
	if err != nil {
		return err
	}
	preimage, err := hex.DecodeString(preimageHex)
	if err != nil || len(preimage) != 32 {
		return fmt.Errorf("l402: preimage is not 32 hex-encoded bytes")
	}
	got := sha256.Sum256(preimage)
	if !hmac.Equal(got[:], id.PaymentHash[:]) {
		return fmt.Errorf("l402: preimage does not match payment hash")
	}
	return verifyCaveats(m.Caveats, opts)
}

func verifyCaveats(caveats []string, opts VerifyOptions) error {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	groups := map[string][]string{}
	var order []string
	for _, cav := range caveats {
		cond, val, ok := strings.Cut(cav, "=")
		if !ok {
			return fmt.Errorf("l402: malformed caveat %q", cav)
		}
		if _, seen := groups[cond]; !seen {
			order = append(order, cond)
		}
		groups[cond] = append(groups[cond], val)
	}
	sort.Strings(order) // deterministic evaluation order

	sawServices, sawCaps := false, false
	for _, cond := range order {
		vals := groups[cond]
		switch {
		case cond == CaveatServices:
			sawServices = true
			if err := verifyServicesChain(vals, opts.Service); err != nil {
				return err
			}
		case strings.HasSuffix(cond, CaveatValidUntilSfx):
			if err := verifyValidUntilChain(cond, vals, now); err != nil {
				return err
			}
		case strings.HasSuffix(cond, CaveatCapsSfx):
			svc := strings.TrimSuffix(cond, CaveatCapsSfx)
			if svc == opts.Service {
				sawCaps = true
			}
			if err := verifyCapsChain(vals, svc, opts); err != nil {
				return err
			}
		default:
			// Unknown caveat conditions MUST be skipped.
		}
	}

	// Fail closed: a required check with no matching caveat must NOT pass. A
	// token that never bound a service cannot authorize one, and a capability
	// requirement needs a capabilities caveat that actually granted it.
	if opts.Service != "" && !sawServices {
		return fmt.Errorf("l402: token carries no services caveat; refusing to authorize %q", opts.Service)
	}
	if opts.Capability != "" && !sawCaps {
		return fmt.Errorf("l402: token carries no %s%s caveat; refusing capability %q",
			opts.Service, CaveatCapsSfx, opts.Capability)
	}
	return nil
}

func parseList(val string) map[string]string {
	out := map[string]string{}
	for _, item := range strings.Split(val, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, tier, _ := strings.Cut(item, ":")
		out[name] = tier
	}
	return out
}

func verifyServicesChain(vals []string, service string) error {
	prev := parseList(vals[0])
	for _, v := range vals[1:] {
		cur := parseList(v)
		for name := range cur {
			if _, ok := prev[name]; !ok {
				return fmt.Errorf("l402: services caveat adds %q (must attenuate)", name)
			}
		}
		prev = cur
	}
	if _, ok := prev[service]; !ok {
		return fmt.Errorf("l402: token not valid for service %q", service)
	}
	return nil
}

func verifyCapsChain(vals []string, svc string, opts VerifyOptions) error {
	prev := parseList(vals[0])
	for _, v := range vals[1:] {
		cur := parseList(v)
		for name := range cur {
			if _, ok := prev[name]; !ok {
				return fmt.Errorf("l402: capabilities caveat adds %q (must attenuate)", name)
			}
		}
		prev = cur
	}
	if opts.Capability != "" && svc == opts.Service {
		if _, ok := prev[opts.Capability]; !ok {
			return fmt.Errorf("l402: capability %q not permitted", opts.Capability)
		}
	}
	return nil
}

func verifyValidUntilChain(cond string, vals []string, now time.Time) error {
	prev := int64(0)
	for i, v := range vals {
		ts, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return fmt.Errorf("l402: bad %s value %q", cond, v)
		}
		if i > 0 && ts > prev {
			return fmt.Errorf("l402: %s must be increasingly restrictive", cond)
		}
		prev = ts
	}
	if !now.Before(time.Unix(prev, 0)) {
		return fmt.Errorf("l402: token expired (%s)", cond)
	}
	return nil
}

// --- HTTP headers ------------------------------------------------------------

// SchemeL402 and SchemeLSAT are the Authorization/WWW-Authenticate scheme
// keywords; servers emit both challenges (LSAT first) and accept both.
const (
	SchemeL402 = "L402"
	SchemeLSAT = "LSAT"
)

// WriteChallenge adds the two WWW-Authenticate challenge headers (LSAT
// first, per the L402 compatibility rule) for the given macaroon and
// invoice.
func WriteChallenge(h http.Header, mac *Macaroon, invoice string) {
	b64 := base64.StdEncoding.EncodeToString(mac.MarshalBinary())
	h.Add("WWW-Authenticate", fmt.Sprintf(`%s macaroon="%s", invoice="%s"`, SchemeLSAT, b64, invoice))
	h.Add("WWW-Authenticate", fmt.Sprintf(`%s macaroon="%s", invoice="%s"`, SchemeL402, b64, invoice))
}

// AuthorizationValue renders a ready-to-send Authorization header value for
// the macaroon and hex preimage.
func AuthorizationValue(mac *Macaroon, preimageHex string) string {
	return fmt.Sprintf("%s %s:%s", SchemeL402,
		base64.StdEncoding.EncodeToString(mac.MarshalBinary()), preimageHex)
}

// Challenge is a parsed WWW-Authenticate L402/LSAT challenge — the
// client-side counterpart of WriteChallenge.
type Challenge struct {
	// Scheme is the keyword as presented: "L402" or "LSAT".
	Scheme string
	// MacaroonB64 is the challenge macaroon, standard base64 as received.
	MacaroonB64 string
	// Macaroon is the parsed macaroon.
	Macaroon *Macaroon
	// Invoice is the BOLT11 invoice string.
	Invoice string
}

// ParseChallenge parses one WWW-Authenticate header value of the form
// `L402 macaroon="<base64>", invoice="<bolt11>"` (or the legacy LSAT
// keyword; parameter order is accepted liberally). ok is false when the
// value is not an L402/LSAT challenge at all; err reports a malformed one.
func ParseChallenge(value string) (ch *Challenge, ok bool, err error) {
	value = strings.TrimSpace(value)
	scheme, params, found := strings.Cut(value, " ")
	if !found {
		return nil, false, nil
	}
	scheme = strings.ToUpper(scheme)
	if scheme != SchemeL402 && scheme != SchemeLSAT {
		return nil, false, nil
	}
	ch = &Challenge{Scheme: scheme}
	for _, part := range strings.Split(params, ",") {
		key, val, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			return nil, true, fmt.Errorf("l402: malformed challenge parameter %q", part)
		}
		val = strings.Trim(val, `"`)
		switch strings.ToLower(key) {
		case "macaroon":
			ch.MacaroonB64 = val
		case "invoice":
			ch.Invoice = val
		default:
			// Unknown parameters are ignored for forward compatibility.
		}
	}
	if ch.MacaroonB64 == "" || ch.Invoice == "" {
		return nil, true, fmt.Errorf("l402: challenge missing macaroon or invoice")
	}
	raw, err := base64.StdEncoding.DecodeString(ch.MacaroonB64)
	if err != nil {
		return nil, true, fmt.Errorf("l402: challenge macaroon is not standard base64: %w", err)
	}
	if ch.Macaroon, err = UnmarshalBinary(raw); err != nil {
		return nil, true, err
	}
	return ch, true, nil
}

// FindChallenge scans WWW-Authenticate values (as returned by
// http.Header.Values) and returns the first well-formed L402/LSAT
// challenge, or nil when none is present.
func FindChallenge(values []string) (*Challenge, error) {
	var firstErr error
	for _, v := range values {
		ch, ok, err := ParseChallenge(v)
		if !ok {
			continue
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return ch, nil
	}
	return nil, firstErr
}

// ParseAuthorization parses an Authorization header value of the form
// "L402 <b64mac>[,<b64mac>...]:<hexpreimage>", accepting both the L402 and
// legacy LSAT keywords case-insensitively. It returns the parsed macaroons
// (attenuation stacks give more than one; the first is the primary) and the
// preimage hex. ok is false when the header is not an L402 credential at
// all; err reports a malformed credential.
func ParseAuthorization(value string) (macs []*Macaroon, preimageHex string, ok bool, err error) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) != 2 {
		return nil, "", false, nil
	}
	scheme := strings.ToUpper(fields[0])
	if scheme != SchemeL402 && scheme != SchemeLSAT {
		return nil, "", false, nil
	}
	credential := fields[1]
	macPart, preimage, found := strings.Cut(credential, ":")
	if !found || macPart == "" || preimage == "" {
		return nil, "", true, fmt.Errorf("l402: credential is not <macaroon>:<preimage>")
	}
	if _, err := hex.DecodeString(preimage); err != nil {
		return nil, "", true, fmt.Errorf("l402: preimage is not hex")
	}
	for _, b64 := range strings.Split(macPart, ",") {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, "", true, fmt.Errorf("l402: macaroon is not standard base64: %w", err)
		}
		m, err := UnmarshalBinary(raw)
		if err != nil {
			return nil, "", true, err
		}
		macs = append(macs, m)
	}
	return macs, preimage, true, nil
}
