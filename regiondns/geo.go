package main

import (
	"net"

	"github.com/oschwald/maxminddb-golang"
)

// Geo maps an address to an ISO country code and a continent code. Empty
// strings mean "unknown".
type Geo interface {
	Lookup(ip net.IP) (country, continent string)
}

// mmdbGeo reads a MaxMind-format database (GeoLite2-Country, or DB-IP's free
// "IP to Country Lite", which uses the same layout).
type mmdbGeo struct{ r *maxminddb.Reader }

func openMMDB(path string) (*mmdbGeo, error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, err
	}
	return &mmdbGeo{r: r}, nil
}

func (g *mmdbGeo) Lookup(ip net.IP) (string, string) {
	var rec struct {
		Country struct {
			ISOCode string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
		Continent struct {
			Code string `maxminddb:"code"`
		} `maxminddb:"continent"`
	}
	if err := g.r.Lookup(ip, &rec); err != nil {
		return "", ""
	}
	return rec.Country.ISOCode, rec.Continent.Code
}

func (g *mmdbGeo) Close() { g.r.Close() }

// noGeo answers "unknown" for everything, so every client gets the default
// region (used when no database is configured).
type noGeo struct{}

func (noGeo) Lookup(net.IP) (string, string) { return "", "" }
