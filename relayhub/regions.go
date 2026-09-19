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
