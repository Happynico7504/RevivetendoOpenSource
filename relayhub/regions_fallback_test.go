package relayhub

import (
	"encoding/json"
	"testing"
)

func TestServingRelaysFallback(t *testing.T) {
	relays := []*Relay{
		{ID: "us-1", Region: "na", Enabled: true},
		{ID: "jp-1", Region: "jp", Enabled: true},
		{ID: "off", Region: "eu", Enabled: false},
	}
	s := ServingRelays(relays)
	if len(s["jp"]) != 1 || len(s["asia"]) != 1 || s["asia"][0].ID != "jp-1" || len(s["na"]) != 1 || len(s["eu"]) != 0 {
		t.Fatalf("%+v", s)
	}
	if got := RegionsServedBy(relays, "jp-1"); len(got) != 2 || got[0] != "asia" || got[1] != "jp" {
		t.Fatalf("jp-1 serves %v", got)
	}
	// A region with its own relay does not use the fallback.
	relays = append(relays, &Relay{ID: "sg-1", Region: "asia", Enabled: true})
	if s := ServingRelays(relays); s["asia"][0].ID != "sg-1" || len(s["asia"]) != 1 {
		t.Fatalf("%+v", s["asia"])
	}
}

func TestDNSConfigUsesFallback(t *testing.T) {
	out, err := BuildDNSConfig([]byte(`{"default":{}}`), []*Relay{{ID: "jp-1", Region: "jp", Host: "tokyo.example", HealthPort: 443, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Regions []struct {
			Name    string   `json:"name"`
			Targets []string `json:"targets"`
		} `json:"regions"`
	}
	json.Unmarshal(out, &cfg)
	if len(cfg.Regions) != 2 || cfg.Regions[0].Name != "jp" || cfg.Regions[1].Name != "asia" || cfg.Regions[1].Targets[0] != "tokyo.example" {
		t.Fatalf("%s", out)
	}
}
