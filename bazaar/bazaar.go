package bazaar

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/x402"
)

// validateDiscoveryInfo validates a bazaar extension's info against its own
// schema (Draft 2020-12). A facilitator MUST do this before cataloging
// (specs/extensions/bazaar.md).
func validateDiscoveryInfo(ext x402.Extension) error {
	if len(ext.Schema) == 0 || len(ext.Info) == 0 {
		return fmt.Errorf("bazaar: missing info or schema")
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(ext.Schema, &schema); err != nil {
		return fmt.Errorf("bazaar: schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("bazaar: resolve schema: %w", err)
	}
	var instance any
	if err := json.Unmarshal(ext.Info, &instance); err != nil {
		return fmt.Errorf("bazaar: info: %w", err)
	}
	return resolved.Validate(instance)
}

// bazaarInputType reads the discovery input's type ("http" | "mcp").
func bazaarInputType(info json.RawMessage) string {
	var probe struct {
		Input struct {
			Type string `json:"type"`
		} `json:"input"`
	}
	_ = json.Unmarshal(info, &probe)
	return probe.Input.Type
}

// bazaarInputToolName reads the discovery input's toolName (mcp inputs).
func bazaarInputToolName(info json.RawMessage) string {
	var probe struct {
		Input struct {
			ToolName string `json:"toolName"`
		} `json:"input"`
	}
	_ = json.Unmarshal(info, &probe)
	return probe.Input.ToolName
}

// harvestBazaar validates and catalogs a resource from a settled payment's
// echoed bazaar extension, returning the EXTENSION-RESPONSES bazaar.status. An
// absent extension returns ("", "") and catalogs nothing. Metadata is
// soft-drop sanitized before storage (the facilitator is the catalog trust
// boundary).
func (s *Service) harvestBazaar(ctx context.Context, pp x402.PaymentPayload, offered x402.PaymentRequirements) (status, reason string) {
	ext, ok := pp.Extensions[dcr402.ExtensionBazaar]
	if !ok {
		return "", ""
	}
	if pp.Resource == nil || pp.Resource.URL == "" {
		return "rejected", "missing resource url"
	}
	if err := validateDiscoveryInfo(ext); err != nil {
		return "rejected", "info failed schema validation"
	}
	typ := bazaarInputType(ext.Info)
	if typ != "http" && typ != "mcp" {
		return "rejected", "unknown discovery type"
	}
	// The catalog key is the (resource url, toolName) tuple
	// (specs/extensions/bazaar.md): an MCP server multiplexes many tools
	// over one endpoint URL, so the URL alone is not unique.
	toolName := ""
	if typ == "mcp" {
		toolName = bazaarInputToolName(ext.Info)
		if toolName == "" {
			return "rejected", "missing input.toolName"
		}
	}

	meta := *pp.Resource
	dcr402.SanitizeServiceMetadata(&meta)
	res := Resource{
		URL:         meta.URL,
		ToolName:    toolName,
		Type:        typ,
		Accepts:     []x402.PaymentRequirements{offered},
		Description: meta.Description,
		MimeType:    meta.MimeType,
		ServiceName: meta.ServiceName,
		Tags:        meta.Tags,
		IconURL:     meta.IconURL,
		LastUpdated: s.now(),
	}
	if len(ext.Info)+len(ext.Schema) <= maxExtensionBytes {
		res.Extensions = map[string]x402.Extension{dcr402.ExtensionBazaar: ext}
	}
	if offered.Network != "" {
		res.Networks = []string{offered.Network}
	}
	if err := s.store.PutResource(ctx, res); err != nil {
		return "rejected", "storage error"
	}
	return "success", ""
}

// extensionResponsesHeader base64-encodes the EXTENSION-RESPONSES value for a
// bazaar status. ok is false when there is nothing to report.
func extensionResponsesHeader(status, reason string) (value string, ok bool) {
	if status == "" {
		return "", false
	}
	bz := map[string]string{"status": status}
	if reason != "" {
		bz["rejectedReason"] = reason
	}
	raw, err := json.Marshal(map[string]any{"bazaar": bz})
	if err != nil {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(raw), true
}
