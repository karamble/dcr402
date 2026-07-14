package dcr402

import (
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/net/idna"

	"github.com/karamble/dcr402/lib/x402"
)

// ExtensionBazaar is the extensions key for the x402 Bazaar discovery
// extension: a seller declares a resource's discovery info in the 402, the
// client echoes it, and a facilitator catalogs it (specs/extensions/bazaar.md).
const ExtensionBazaar = "bazaar"

const draft2020 = "https://json-schema.org/draft/2020-12/schema"

// DiscoveryOutput describes a resource's response shape for the bazaar info.
type DiscoveryOutput struct {
	Type    string `json:"type"`
	Format  string `json:"format,omitempty"`
	Example any    `json:"example,omitempty"`
}

func isBodyMethod(m string) bool {
	switch strings.ToUpper(m) {
	case "POST", "PUT", "PATCH":
		return true
	}
	return false
}

// BuildHTTPDiscovery builds a bazaar extension for an HTTP resource: the
// discovery info plus a Draft 2020-12 schema that validates it. queryParams is
// used for the query methods (GET/HEAD/DELETE); body methods (POST/PUT/PATCH)
// carry a json body shape. output is optional.
func BuildHTTPDiscovery(method string, queryParams map[string]string, output *DiscoveryOutput) x402.Extension {
	method = strings.ToUpper(method)
	input := map[string]any{"type": "http", "method": method}
	inputProps := map[string]any{
		"type":   map[string]any{"type": "string", "const": "http"},
		"method": map[string]any{"type": "string", "enum": []string{method}},
	}
	required := []string{"type", "method"}

	if len(queryParams) > 0 {
		input["queryParams"] = queryParams
		props := map[string]any{}
		keys := make([]string, 0, len(queryParams))
		for k := range queryParams {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			props[k] = map[string]any{"type": "string"}
		}
		inputProps["queryParams"] = map[string]any{
			"type": "object", "properties": props, "required": keys,
		}
	}
	if isBodyMethod(method) {
		input["bodyType"] = "json"
		input["body"] = map[string]any{}
		inputProps["bodyType"] = map[string]any{"type": "string", "enum": []string{"json", "form-data", "text"}}
		inputProps["body"] = map[string]any{"type": "object"}
		required = append(required, "bodyType", "body")
	}

	info := map[string]any{"input": input}
	schemaProps := map[string]any{
		"input": map[string]any{
			"type": "object", "properties": inputProps,
			"required": required, "additionalProperties": false,
		},
	}
	if output != nil {
		info["output"] = output
		schemaProps["output"] = map[string]any{
			"type":       "object",
			"properties": map[string]any{"type": map[string]any{"type": "string"}},
			"required":   []string{"type"},
		}
	}
	return x402.Extension{
		Info: x402.MustRaw(info),
		Schema: x402.MustRaw(map[string]any{
			"$schema": draft2020, "type": "object",
			"properties": schemaProps, "required": []string{"input"},
		}),
	}
}

// BuildMCPDiscovery builds a bazaar extension for an MCP tool. inputSchema is
// the tool's JSON-Schema input (as advertised by tools/list); description and
// transport are optional (transport defaults to streamable-http).
func BuildMCPDiscovery(toolName string, inputSchema json.RawMessage, description, transport string, output *DiscoveryOutput) x402.Extension {
	input := map[string]any{"type": "mcp", "toolName": toolName}
	if len(inputSchema) > 0 {
		input["inputSchema"] = inputSchema
	} else {
		input["inputSchema"] = map[string]any{"type": "object"}
	}
	inputProps := map[string]any{
		"type":        map[string]any{"type": "string", "const": "mcp"},
		"toolName":    map[string]any{"type": "string"},
		"inputSchema": map[string]any{"type": "object"},
	}
	required := []string{"type", "toolName", "inputSchema"}
	if description != "" {
		input["description"] = description
		inputProps["description"] = map[string]any{"type": "string"}
	}
	if transport != "" {
		input["transport"] = transport
		inputProps["transport"] = map[string]any{"type": "string", "enum": []string{"streamable-http", "sse"}}
	}
	info := map[string]any{"input": input}
	schemaProps := map[string]any{
		"input": map[string]any{
			"type": "object", "properties": inputProps,
			"required": required, "additionalProperties": false,
		},
	}
	if output != nil {
		info["output"] = output
		schemaProps["output"] = map[string]any{
			"type":       "object",
			"properties": map[string]any{"type": map[string]any{"type": "string"}},
			"required":   []string{"type"},
		}
	}
	return x402.Extension{
		Info: x402.MustRaw(info),
		Schema: x402.MustRaw(map[string]any{
			"$schema": draft2020, "type": "object",
			"properties": schemaProps, "required": []string{"input"},
		}),
	}
}

// --- Service metadata sanitization (specs/extensions/bazaar.md, soft-drop) ---

const (
	maxServiceNameLen = 32
	maxTagLen         = 32
	maxTags           = 5
	maxIconURLLen     = 2048
)

var (
	printableASCII = regexp.MustCompile(`^[\x20-\x7e]+$`)
	ipv4Host       = regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$`)
	allDigitsHost  = regexp.MustCompile(`^\d+$`)
	hexLiteralHost = regexp.MustCompile(`(?i)^0x[0-9a-f]+$`)
	loopbackHosts  = map[string]bool{
		"localhost": true, "localhost.localdomain": true,
		"ip6-localhost": true, "ip6-loopback": true,
	}
)

func hasControlChar(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func isValidServiceName(s string) bool {
	return s != "" && len(s) <= maxServiceNameLen && !hasControlChar(s) && printableASCII.MatchString(s)
}

func isValidTag(s string) bool {
	return s != "" && len(s) <= maxTagLen && !hasControlChar(s) && printableASCII.MatchString(s)
}

func sanitizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, maxTags)
	for _, t := range tags {
		if !isValidTag(t) {
			continue
		}
		key := strings.ToLower(t)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
		if len(out) == maxTags {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isValidIconURL enforces the bazaar iconUrl rules: an absolute http(s) URL, no
// userinfo, and a host that is not an IP literal, a loopback name, or a numeric
// (decimal/hex) IP encoding, after percent-decode and IDNA normalization.
func isValidIconURL(s string) bool {
	if s == "" || len(s) > maxIconURLLen || hasControlChar(s) {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.User != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	decoded, err := url.PathUnescape(host)
	if err != nil {
		return false
	}
	ascii, err := idna.Lookup.ToASCII(decoded)
	if err != nil {
		ascii = decoded
	}
	ascii = strings.ToLower(ascii)
	if loopbackHosts[ascii] || strings.Contains(ascii, ":") {
		return false
	}
	if ipv4Host.MatchString(ascii) || net.ParseIP(ascii) != nil {
		return false
	}
	if allDigitsHost.MatchString(ascii) || hexLiteralHost.MatchString(ascii) {
		return false
	}
	return true
}

// SanitizeServiceMetadata applies the bazaar soft-drop rules in place: an
// offending serviceName, tag, or iconUrl is dropped, the rest is preserved. The
// facilitator is the catalog trust boundary, so it sanitizes client-echoed and
// submitted metadata before storing it.
func SanitizeServiceMetadata(r *x402.ResourceInfo) {
	if r == nil {
		return
	}
	if !isValidServiceName(r.ServiceName) {
		r.ServiceName = ""
	}
	r.Tags = sanitizeTags(r.Tags)
	if !isValidIconURL(r.IconURL) {
		r.IconURL = ""
	}
}
