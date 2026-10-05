package main

import "testing"

func TestBadgeArcadeRegionForBossAppID(t *testing.T) {
	cases := []struct {
		path   string
		prefix string
		ok     bool
	}{
		{"/p01/nsa/J6la9Kj8iqTvAPOq/data/data_v131.dat", "GB_en", true},
		{"/p01/nsa/OvbmGLZ9senvgV3K/FGONLYT/playinfo_v131.dat", "US_en", true},
		{"/p01/nsa/j0ITmVqVgfUxe0O9/data/allbadge_v131.dat", "JP_ja", true},
		{"/p01/nsa/unknownAppId0000/data/data_v131.dat", "", false},
		{"/p01/policylist/3/NL", "", false},
	}
	for _, c := range cases {
		prefix, ok := badgeArcadeRegionForBossAppID(c.path)
		if prefix != c.prefix || ok != c.ok {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.path, prefix, ok, c.prefix, c.ok)
		}
	}
}
