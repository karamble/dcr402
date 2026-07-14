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
	// Resource is the resource URL (or mcp://tool/name), the index key.
	Resource string `json:"resource"`
	// Type is "http" (default) or "mcp".
	Type string `json:"type,omitempty"`
	// Accepts is the resource's accepts[] entries (at least one).
	Accepts []x402.PaymentRequirements `json:"accepts"`
	// Metadata carries the human-facing fields (serviceName, tags, iconUrl,
	// description, mimeType). Its URL is ignored in favor of Resource.
	Metadata x402.ResourceInfo `json:"metadata"`
}

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
	return Resource{
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
	}, nil
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
