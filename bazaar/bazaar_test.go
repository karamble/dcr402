package bazaar

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/x402"
)

func TestHarvestBazaar(t *testing.T) {
	svc := newTestService(t, Config{Networks: []string{"mainnet"}, Discovery: DiscoveryConfig{Enabled: true}})
	ctx := context.Background()
	ext := dcr402.BuildHTTPDiscovery("GET", nil, nil)
	pp := x402.PaymentPayload{
		Resource: &x402.ResourceInfo{
			URL:         "https://api.example.com/weather",
			ServiceName: "Weather",
			Tags:        []string{"weather"},
			IconURL:     "http://127.0.0.1/evil.png", // hostile -> soft-dropped
		},
		Extensions: map[string]x402.Extension{dcr402.ExtensionBazaar: ext},
	}
	status, reason := svc.harvestBazaar(ctx, pp, goldenRequirements())
	if status != "success" {
		t.Fatalf("status=%q reason=%q", status, reason)
	}
	all, _ := svc.store.Resources(ctx)
	if len(all) != 1 {
		t.Fatalf("cataloged %d resources", len(all))
	}
	r := all[0]
	if r.URL != "https://api.example.com/weather" || r.ServiceName != "Weather" ||
		r.IconURL != "" || r.Type != "http" || len(r.Tags) != 1 {
		t.Fatalf("cataloged resource wrong: %+v", r)
	}
}

func TestHarvestBazaarRejectsInvalidInfo(t *testing.T) {
	svc := newTestService(t, Config{Networks: []string{"mainnet"}, Discovery: DiscoveryConfig{Enabled: true}})
	ctx := context.Background()
	good := dcr402.BuildHTTPDiscovery("GET", nil, nil)
	// info claims a type the schema's const forbids.
	bad := x402.Extension{Info: json.RawMessage(`{"input":{"type":"ftp","method":"GET"}}`), Schema: good.Schema}
	pp := x402.PaymentPayload{
		Resource:   &x402.ResourceInfo{URL: "https://x/y"},
		Extensions: map[string]x402.Extension{dcr402.ExtensionBazaar: bad},
	}
	if status, _ := svc.harvestBazaar(ctx, pp, goldenRequirements()); status != "rejected" {
		t.Fatalf("expected rejected, got %q", status)
	}
	if all, _ := svc.store.Resources(ctx); len(all) != 0 {
		t.Fatalf("rejected info was cataloged: %d", len(all))
	}
}

func TestHarvestBazaarNoExtension(t *testing.T) {
	svc := newTestService(t, Config{Networks: []string{"mainnet"}, Discovery: DiscoveryConfig{Enabled: true}})
	status, _ := svc.harvestBazaar(context.Background(), x402.PaymentPayload{}, goldenRequirements())
	if status != "" {
		t.Fatalf("no extension should catalog nothing, got %q", status)
	}
}

func TestExtensionResponsesHeader(t *testing.T) {
	v, ok := extensionResponsesHeader("success", "")
	if !ok {
		t.Fatal("expected a header")
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"bazaar":{"status":"success"}}` {
		t.Fatalf("header body: %s", raw)
	}
	if _, ok := extensionResponsesHeader("", ""); ok {
		t.Fatal("empty status must produce no header")
	}
}

func TestSubmitSanitizesMetadata(t *testing.T) {
	srv := discoveryService(t, Config{
		Networks:  []string{"mainnet"},
		Discovery: DiscoveryConfig{Enabled: true, PublicSubmit: true},
	})
	req := sampleSubmit()
	req.Metadata.IconURL = "http://localhost/evil.png" // hostile -> dropped
	req.Metadata.ServiceName = "café"                  // non-ASCII -> dropped
	if err := Submit(context.Background(), srv.URL, "", req); err != nil {
		t.Fatal(err)
	}
	var list listResourcesResponse
	getJSON(t, srv.URL+"/discovery/resources", &list)
	if len(list.Items) != 1 {
		t.Fatalf("items: %d", len(list.Items))
	}
	if list.Items[0].IconURL != "" || list.Items[0].ServiceName != "" {
		t.Fatalf("metadata not sanitized: %+v", list.Items[0])
	}
}
