package dcr402

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/karamble/dcr402/lib/x402"
)

func TestBuildHTTPDiscovery(t *testing.T) {
	ext := BuildHTTPDiscovery("GET", map[string]string{"city": "SF"}, &DiscoveryOutput{Type: "json"})
	var info struct {
		Input struct {
			Type        string            `json:"type"`
			Method      string            `json:"method"`
			QueryParams map[string]string `json:"queryParams"`
		} `json:"input"`
		Output *struct {
			Type string `json:"type"`
		} `json:"output"`
	}
	if err := json.Unmarshal(ext.Info, &info); err != nil {
		t.Fatal(err)
	}
	if info.Input.Type != "http" || info.Input.Method != "GET" || info.Input.QueryParams["city"] != "SF" {
		t.Fatalf("http info wrong: %+v", info.Input)
	}
	if info.Output == nil || info.Output.Type != "json" {
		t.Fatal("output missing")
	}
	var sch map[string]any
	if err := json.Unmarshal(ext.Schema, &sch); err != nil || sch["$schema"] != draft2020 {
		t.Fatalf("schema wrong: %v", sch)
	}
}

func TestBuildMCPDiscovery(t *testing.T) {
	ext := BuildMCPDiscovery("analyze",
		json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		"a tool", "streamable-http", nil)
	var info struct {
		Input struct {
			Type        string          `json:"type"`
			ToolName    string          `json:"toolName"`
			InputSchema json.RawMessage `json:"inputSchema"`
			Transport   string          `json:"transport"`
		} `json:"input"`
	}
	if err := json.Unmarshal(ext.Info, &info); err != nil {
		t.Fatal(err)
	}
	if info.Input.Type != "mcp" || info.Input.ToolName != "analyze" ||
		info.Input.Transport != "streamable-http" || len(info.Input.InputSchema) == 0 {
		t.Fatalf("mcp info wrong: %+v", info.Input)
	}
}

func TestSanitizeServiceMetadata(t *testing.T) {
	cases := []struct {
		name string
		in   x402.ResourceInfo
		want func(x402.ResourceInfo) bool
	}{
		{"valid kept",
			x402.ResourceInfo{ServiceName: "Weather API", Tags: []string{"weather", "api"}, IconURL: "https://example.com/i.png"},
			func(r x402.ResourceInfo) bool {
				return r.ServiceName == "Weather API" && len(r.Tags) == 2 && r.IconURL != ""
			}},
		{"long serviceName dropped",
			x402.ResourceInfo{ServiceName: strings.Repeat("x", 33)},
			func(r x402.ResourceInfo) bool { return r.ServiceName == "" }},
		{"control-char serviceName dropped",
			x402.ResourceInfo{ServiceName: "bad\x00name"},
			func(r x402.ResourceInfo) bool { return r.ServiceName == "" }},
		{"non-ascii serviceName dropped",
			x402.ResourceInfo{ServiceName: "café"},
			func(r x402.ResourceInfo) bool { return r.ServiceName == "" }},
		{"tags dedup + cap to 5",
			x402.ResourceInfo{Tags: []string{"A", "a", "b", "c", "d", "e", "f"}},
			func(r x402.ResourceInfo) bool { return len(r.Tags) == 5 && r.Tags[0] == "A" }},
		{"data: icon dropped",
			x402.ResourceInfo{IconURL: "data:image/png;base64,xxx"},
			func(r x402.ResourceInfo) bool { return r.IconURL == "" }},
		{"ip-literal icon dropped",
			x402.ResourceInfo{IconURL: "http://127.0.0.1/i.png"},
			func(r x402.ResourceInfo) bool { return r.IconURL == "" }},
		{"localhost icon dropped",
			x402.ResourceInfo{IconURL: "http://localhost/i.png"},
			func(r x402.ResourceInfo) bool { return r.IconURL == "" }},
		{"userinfo icon dropped",
			x402.ResourceInfo{IconURL: "http://user@example.com/i.png"},
			func(r x402.ResourceInfo) bool { return r.IconURL == "" }},
		{"all-digit host dropped",
			x402.ResourceInfo{IconURL: "http://2130706433/i.png"},
			func(r x402.ResourceInfo) bool { return r.IconURL == "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.in
			SanitizeServiceMetadata(&r)
			if !c.want(r) {
				t.Fatalf("got %+v", r)
			}
		})
	}
}
