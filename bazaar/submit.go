package bazaar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/x402"
)

// SubmitRequest is the POST /discovery/submit body: a resource a seller
// registers in the facilitator's discovery index. It is the fallback for the
// Bazaar EXTENSION-RESPONSES ack that hosted facilitators do not emit, so a
// dcr402 or dcr402d deployment can list itself explicitly.
type SubmitRequest struct {
	// Resource is the resource URL: for http the paid endpoint, for mcp the
	// server's public streamable-HTTP endpoint (with the tool identified by
	// the bazaar extension) or the host-less mcp://tool/<name> fallback.
	Resource string `json:"resource"`
	// Type is "http" (default) or "mcp".
	Type string `json:"type,omitempty"`
	// Accepts is the resource's accepts[] entries (at least one).
	Accepts []x402.PaymentRequirements `json:"accepts"`
	// Metadata carries the human-facing fields (serviceName, tags, iconUrl,
	// description, mimeType). Its URL is ignored in favor of Resource.
	Metadata x402.ResourceInfo `json:"metadata"`
	// Extensions carries the item's extension payloads (the foundation
	// discovery-item shape). Only the "bazaar" extension is validated and
	// stored; an mcp resource listed under its server endpoint URL MUST
	// carry its per-tool identity in the extension's info.input.toolName -
	// the catalog is keyed by the (resource, toolName) tuple.
	Extensions map[string]x402.Extension `json:"extensions,omitempty"`
}

// maxExtensionBytes caps a stored extension payload (info + schema) so a
// submission cannot bloat the index.
const maxExtensionBytes = 64 << 10

// toResource validates a SubmitRequest and turns it into a stored Resource.
func (req SubmitRequest) toResource(now time.Time) (Resource, error) {
	if strings.TrimSpace(req.Resource) == "" {
		return Resource{}, fmt.Errorf("resource is required")
	}
	if len(req.Accepts) == 0 {
		return Resource{}, fmt.Errorf("at least one accepts entry is required")
	}
	typ := req.Type
	if typ == "" {
		typ = "http"
	}
	if typ != "http" && typ != "mcp" {
		return Resource{}, fmt.Errorf("type must be \"http\" or \"mcp\"")
	}
	if !validResourceURL(req.Resource) {
		return Resource{}, fmt.Errorf("resource must be an http(s) or mcp URL")
	}
	if len(req.Metadata.Description) > 2048 {
		return Resource{}, fmt.Errorf("description exceeds its length limit")
	}
	// Soft-drop sanitize the client-supplied service metadata: an offending
	// serviceName, tag, or iconUrl is dropped, the rest preserved.
	meta := req.Metadata
	dcr402.SanitizeServiceMetadata(&meta)
	seen := make(map[string]bool)
	var networks []string
	for _, a := range req.Accepts {
		if a.Network != "" && !seen[a.Network] {
			seen[a.Network] = true
			networks = append(networks, a.Network)
		}
	}
	res := Resource{
		URL:         req.Resource,
		Type:        typ,
		Accepts:     req.Accepts,
		Description: meta.Description,
		MimeType:    meta.MimeType,
		ServiceName: meta.ServiceName,
		Tags:        meta.Tags,
		IconURL:     meta.IconURL,
		Networks:    networks,
		LastUpdated: now,
	}
	if ext, ok := req.Extensions[dcr402.ExtensionBazaar]; ok {
		if len(ext.Info)+len(ext.Schema) > maxExtensionBytes {
			return Resource{}, fmt.Errorf("bazaar extension exceeds its size limit")
		}
		if err := validateDiscoveryInfo(ext); err != nil {
			return Resource{}, fmt.Errorf("bazaar extension info failed schema validation")
		}
		if it := bazaarInputType(ext.Info); it != typ {
			return Resource{}, fmt.Errorf("bazaar extension input type %q does not match resource type %q", it, typ)
		}
		res.ToolName = bazaarInputToolName(ext.Info)
		res.Extensions = map[string]x402.Extension{dcr402.ExtensionBazaar: ext}
	}
	// An mcp resource listed under its server endpoint URL is unaddressable
	// without the per-tool identity, and every tool of the server would
	// collide on the same key. The host-less mcp://tool/<name> form encodes
	// the tool in the URL itself, so it stands alone.
	if typ == "mcp" && res.ToolName == "" && !strings.HasPrefix(strings.TrimSpace(req.Resource), "mcp:") {
		return Resource{}, fmt.Errorf("an mcp resource listed under a server URL requires the bazaar extension's input.toolName")
	}
	return res, nil
}

// validResourceURL accepts only http(s) URLs with a host, or an mcp:// URL -
// so a submission cannot inject a file:, javascript:, or data: link that a
// downstream UI might follow.
func validResourceURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https":
		return u.Host != ""
	case "mcp":
		return true
	}
	return false
}

// Submit registers a resource with a facilitator's discovery index by POSTing
// to facURL + "/discovery/submit". apiKey may be empty for an open (self-
// hosted) facilitator. This is the seller-side network-effect helper: a
// dcr402 or dcr402d deployment calls it once per resource it offers.
func Submit(ctx context.Context, facURL, apiKey string, req SubmitRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("dcrbazaar submit: marshal: %w", err)
	}
	url := strings.TrimRight(facURL, "/") + "/discovery/submit"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("dcrbazaar submit: request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("X-API-Key", apiKey)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("dcrbazaar submit: post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("dcrbazaar submit: %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}
