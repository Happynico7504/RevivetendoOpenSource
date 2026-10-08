package relayhub

import (
	"encoding/json"
	"fmt"
	"sort"
)

// BuildDNSConfig returns a regiondns config: the base config with its
// "regions" replaced by one region per catalog entry that has at least one
// enabled relay serving it (its own, or its RegionFallback's). The default region (base config) stays the main.
func BuildDNSConfig(base []byte, relays []*Relay) ([]byte, error) {
	var cfg map[string]any
	if err := json.Unmarshal(base, &cfg); err != nil {
		return nil, fmt.Errorf("base config: %w", err)
	}
	byRegion := ServingRelays(relays)
	regions := []map[string]any{}
	for _, name := range regionOrder {
		rs := append([]*Relay(nil), byRegion[name]...)
		if len(rs) == 0 {
			continue
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
		spec := RegionCatalog[name]
		var targets []string
		health := ""
		for _, r := range rs {
			targets = append(targets, r.Host)
			if health == "" && r.HealthPort > 0 {
				health = fmt.Sprintf("tcp:%d", r.HealthPort)
			}
		}
		reg := map[string]any{"name": name, "targets": targets}
		if len(spec.Countries) > 0 {
			reg["countries"] = spec.Countries
		}
		if len(spec.Continents) > 0 {
			reg["continents"] = spec.Continents
		}
		if health != "" {
			reg["health"] = health
		}
		regions = append(regions, reg)
	}
	cfg["regions"] = regions
	return json.MarshalIndent(cfg, "", "  ")
}
