package relayhub

import "sort"

// RegionSpec says which clients a region's relays serve. regiondns picks the
// first region whose country list matches, then the first whose continent list
// matches, otherwise the default (the main), so country-specific regions
// (e.g. jp) take precedence over the broader continent ones.
type RegionSpec struct {
	Countries  []string
	Continents []string
}

var RegionCatalog = map[string]RegionSpec{
	"na":   {Countries: []string{"US", "CA", "MX"}, Continents: []string{"NA", "SA"}},
	"jp":   {Countries: []string{"JP", "KR", "TW"}},
	"asia": {Continents: []string{"AS", "OC"}},
	"eu":   {Continents: []string{"EU", "AF"}},
}

// regionOrder puts country-specific regions ahead of continent-only ones.
var regionOrder = []string{"jp", "na", "asia", "eu"}

func RegionNames() []string {
	var n []string
	for k := range RegionCatalog {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// RegionFallback names the region whose relays serve a region that has none of its own: a Tokyo
// relay is far closer to Australia or Singapore than the main is, and the other way round.
var RegionFallback = map[string]string{"asia": "jp", "jp": "asia"}

// ServingRelays maps each catalog region to the enabled relays that serve it: its own, or if it
// has none, those of its fallback region.
func ServingRelays(relays []*Relay) map[string][]*Relay {
	own := map[string][]*Relay{}
	for _, r := range relays {
		if r.Enabled {
			own[r.Region] = append(own[r.Region], r)
		}
	}
	out := map[string][]*Relay{}
	for name := range RegionCatalog {
		rs := own[name]
		if len(rs) == 0 {
			rs = own[RegionFallback[name]]
		}
		if len(rs) > 0 {
			out[name] = rs
		}
	}
	return out
}

// RegionsServedBy lists the catalog regions a relay serves (see ServingRelays), sorted.
func RegionsServedBy(relays []*Relay, id string) []string {
	var out []string
	for name, rs := range ServingRelays(relays) {
		for _, r := range rs {
			if r.ID == id {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}
