package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/miekg/dns"
)

// Config is the JSON file the server loads (and reloads when it changes).
//
//	{
//	  "listen": ["45.157.178.35:53"],
//	  "zone": "regionselect.nicochristmann.net",
//	  "ns": ["netcup-server.nicochristmann.net"],
//	  "hostmaster": "hostmaster.nicochristmann.net",
//	  "ttl": 60,
//	  "geoip_db": "/etc/regiondns/dbip-country-lite.mmdb",
//	  "default": {"targets": ["netcup-server.nicochristmann.net"]},
//	  "regions": [
//	    {"name": "us", "countries": ["US","CA"], "continents": ["NA","SA"],
//	     "targets": ["relay-us.nicochristmann.net"], "health": "tcp:443"}
//	  ]
//	}
//
// A target is an IP address or a hostname; hostnames are resolved by this
// server itself (so answers are always plain A/AAAA, which also keeps the zone
// apex valid) and re-resolved periodically.
type Config struct {
	Listen     []string `json:"listen"`
	Zone       string   `json:"zone"`
	NS         []string `json:"ns"`
	Hostmaster string   `json:"hostmaster"`
	TTL        uint32   `json:"ttl"`
	GeoIPDB    string   `json:"geoip_db"`
	Default    Region   `json:"default"`
	Regions    []Region `json:"regions"`
	RateLimit  struct {
		QPS   float64 `json:"qps"`   // sustained UDP queries/sec per source prefix
		Burst float64 `json:"burst"` // bucket size
	} `json:"rate_limit"`
	// ResolverAddr is the DNS server used to resolve hostname targets
	// ("host:port"); empty = the system resolver.
	ResolverAddr string `json:"resolver"`
}

// Region maps countries/continents to a set of targets.
type Region struct {
	Name       string   `json:"name"`
	Countries  []string `json:"countries"`  // ISO 3166-1 alpha-2, matched first
	Continents []string `json:"continents"` // AF AN AS EU NA OC SA, matched second
	Targets    []string `json:"targets"`
	Health     string   `json:"health"` // "" (always healthy) or "tcp:<port>"
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	if len(c.Listen) == 0 {
		return fmt.Errorf("listen: at least one address required")
	}
	if _, ok := dns.IsDomainName(c.Zone); !ok || c.Zone == "" {
		return fmt.Errorf("zone %q is not a valid domain name", c.Zone)
	}
	if len(c.NS) == 0 {
		return fmt.Errorf("ns: at least one nameserver hostname required")
	}
	for _, n := range c.NS {
		if net.ParseIP(n) != nil {
			return fmt.Errorf("ns %q: must be a hostname, not an IP", n)
		}
		if _, ok := dns.IsDomainName(n); !ok {
			return fmt.Errorf("ns %q is not a valid hostname", n)
		}
	}
	if c.Hostmaster == "" {
		return fmt.Errorf("hostmaster required")
	}
	if c.TTL == 0 {
		c.TTL = 60
	}
	if c.RateLimit.QPS == 0 {
		c.RateLimit.QPS = 20
	}
	if c.RateLimit.Burst == 0 {
		c.RateLimit.Burst = 40
	}
	if len(c.Default.Targets) == 0 {
		return fmt.Errorf("default.targets: at least one target required")
	}
	seen := map[string]bool{}
	check := func(r Region, isDefault bool) error {
		for _, t := range r.Targets {
			if net.ParseIP(t) == nil {
				if _, ok := dns.IsDomainName(t); !ok {
					return fmt.Errorf("region %q: target %q is neither an IP nor a hostname", r.Name, t)
				}
			}
		}
		if r.Health != "" {
			if !strings.HasPrefix(r.Health, "tcp:") {
				return fmt.Errorf("region %q: health must be \"tcp:<port>\"", r.Name)
			}
			var p int
			if _, err := fmt.Sscanf(strings.TrimPrefix(r.Health, "tcp:"), "%d", &p); err != nil || p < 1 || p > 65535 {
				return fmt.Errorf("region %q: bad health port", r.Name)
			}
		}
		if !isDefault {
			if r.Name == "" || seen[r.Name] {
				return fmt.Errorf("region name %q missing or duplicated", r.Name)
			}
			seen[r.Name] = true
			if len(r.Targets) == 0 {
				return fmt.Errorf("region %q: no targets", r.Name)
			}
			if len(r.Countries) == 0 && len(r.Continents) == 0 {
				return fmt.Errorf("region %q: needs countries or continents", r.Name)
			}
		}
		return nil
	}
	if err := check(c.Default, true); err != nil {
		return err
	}
	for _, r := range c.Regions {
		if err := check(r, false); err != nil {
			return err
		}
	}
	return nil
}
