package bazaar

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	dcr402 "github.com/karamble/dcr402/lib"
	"github.com/karamble/dcr402/lib/x402"
)

func discoveryService(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	svc := newTestService(t, cfg)
	srv := httptest.NewServer(svc.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func sampleSubmit() SubmitRequest {
	return SubmitRequest{
		Resource: "https://api.example.com/weather",
		Type:     "http",
		Accepts:  []x402.PaymentRequirements{goldenRequirements()},
		Metadata: x402.ResourceInfo{
			ServiceName: "Example Weather",
			Description: "Weather forecast API",
			Tags:        []string{"weather", "forecast"},
			IconURL:     "https://api.example.com/icon.png",
		},
	}
}

func TestDiscoveryDisabled(t *testing.T) {
	srv := discoveryService(t, Config{Networks: []string{"mainnet"}})
	resp, err := http.Get(srv.URL + "/discovery/resources")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled discovery: status %d, want 404", resp.StatusCode)
	}
}

func TestDiscoverySubmitListSearch(t *testing.T) {
	srv := discoveryService(t, Config{
		Networks:  []string{"mainnet"},
		Discovery: DiscoveryConfig{Enabled: true, PublicSubmit: true},
	})

	if err := Submit(context.Background(), srv.URL, "", sampleSubmit()); err != nil {
		t.Fatalf("submit: %v", err)
	}

	var list listResourcesResponse
	getJSON(t, srv.URL+"/discovery/resources", &list)
	if len(list.Items) != 1 || list.Pagination.Total != 1 {
		t.Fatalf("list: %+v", list)
	}
	item := list.Items[0]
	if item.Resource != "https://api.example.com/weather" ||
		item.ServiceName != "Example Weather" || item.X402Version != 2 ||
		len(item.Accepts) != 1 || item.Type != "http" || item.LastUpdated == "" {
		t.Fatalf("item: %+v", item)
	}

	var hit searchResourcesResponse
	getJSON(t, srv.URL+"/discovery/search?query=weather", &hit)
	if len(hit.Resources) != 1 {
		t.Fatalf("search hit: %+v", hit)
	}
	var miss searchResourcesResponse
	getJSON(t, srv.URL+"/discovery/search?query=nonexistent", &miss)
	if len(miss.Resources) != 0 {
		t.Fatalf("search miss: %+v", miss)
	}

	var byNet listResourcesResponse
	getJSON(t, srv.URL+"/discovery/resources?network="+dcr402.Mainnet.CAIP2, &byNet)
	if len(byNet.Items) != 1 {
		t.Fatalf("network match: %+v", byNet)
	}
	var wrongNet listResourcesResponse
	getJSON(t, srv.URL+"/discovery/resources?network="+dcr402.Testnet3.CAIP2, &wrongNet)
	if len(wrongNet.Items) != 0 {
		t.Fatalf("network miss: %+v", wrongNet)
	}
}

func TestSupportedAdvertisesBazaar(t *testing.T) {
	svc := newTestService(t, Config{
		Networks:  []string{"mainnet"},
		Discovery: DiscoveryConfig{Enabled: true},
	})
	found := false
	for _, e := range svc.supported().Extensions {
		if e == extensionBazaar {
			found = true
		}
	}
	if !found {
		t.Fatalf("supported extensions lack bazaar: %+v", svc.supported().Extensions)
	}
}

func TestSubmitValidation(t *testing.T) {
	srv := discoveryService(t, Config{
		Networks:  []string{"mainnet"},
		Discovery: DiscoveryConfig{Enabled: true, PublicSubmit: true},
	})
	body, _ := json.Marshal(SubmitRequest{Resource: "https://x"}) // no accepts
	resp, err := http.Post(srv.URL+"/discovery/submit", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing accepts: status %d, want 400", resp.StatusCode)
	}
}

func TestSubmitGated(t *testing.T) {
	srv := discoveryService(t, Config{
		Networks:  []string{"mainnet"},
		Discovery: DiscoveryConfig{Enabled: true, PublicSubmit: false},
		APIKeys:   []string{"k"},
	})
	if err := Submit(context.Background(), srv.URL, "", sampleSubmit()); err == nil {
		t.Fatal("gated submit without key should fail")
	}
	if err := Submit(context.Background(), srv.URL, "k", sampleSubmit()); err != nil {
		t.Fatalf("submit with key: %v", err)
	}
}

// TestConfigFailsClosed checks the ambiguous submit config is rejected.
func TestConfigFailsClosed(t *testing.T) {
	cfg := Config{
		Listen:    ":1",
		Networks:  []string{"mainnet"},
		Discovery: DiscoveryConfig{Enabled: true, PublicSubmit: false},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("discovery with public_submit=false and no api_keys must be rejected")
	}
}

// TestSubmitFailsClosed checks the runtime gate refuses submit when it is
// neither public nor key-protected.
func TestSubmitFailsClosed(t *testing.T) {
	srv := discoveryService(t, Config{
		Networks:  []string{"mainnet"},
		Discovery: DiscoveryConfig{Enabled: true, PublicSubmit: false},
	})
	body, _ := json.Marshal(sampleSubmit())
	resp, err := http.Post(srv.URL+"/discovery/submit", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("submit with no keys and public_submit=false must be 403, got %d", resp.StatusCode)
	}
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}
