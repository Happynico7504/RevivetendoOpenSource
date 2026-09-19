package relayd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is relayd.json. Example:
//
//	{
//	  "bundle": "/etc/relayd/bundle.json",
//	  "data_dir": "/var/lib/relayd",
//	  "listeners": [
//	    {"listen": ":443",  "backend": "olv",     "mode": "sni"},
//	    {"listen": ":6666", "backend": "account", "mode": "single", "cert": "nicochristmann-nn-cert", "min_tls": "1.0", "stagger": "all"},
//	    {"listen": ":9013", "backend": "hpp",     "mode": "single", "cert": "wildcard-nicoch-net"}
//	  ]
//	}
type Config struct {
	Bundle          string     `json:"bundle"`
	DataDir         string     `json:"data_dir"`
	CertSyncSeconds int        `json:"cert_sync_seconds"` // default 300
	PollSeconds     int        `json:"poll_seconds"`      // invalidation poll, default 5
	Listeners       []Listener `json:"listeners"`
	StaggerHosts    []string   `json:"stagger_hosts"` // SNI names that get the per-IP handshake stagger (sni mode)
	StaggerEmptySNI *bool      `json:"stagger_empty_sni"`
}

// Listener is one console-facing TLS endpoint.
type Listener struct {
	Listen  string `json:"listen"`
	Backend string `json:"backend"` // name of a backend configured on the main
	Mode    string `json:"mode"`    // "sni": certificate chosen by the main's route table; "single": fixed Cert
	Cert    string `json:"cert"`    // single mode: certificate name from the manifest
	MinTLS  string `json:"min_tls"` // "1.0" | "1.2" (default 1.2 for sni, 1.0 for single)
	Stagger string `json:"stagger"` // "" | "sni" (hosts in stagger_hosts) | "all"
}

// DefaultStaggerHosts is account-proxy's is3DSSensitiveHost list (the 3DS/Wii U
// ssl module cannot survive concurrent handshakes to these hosts).
var DefaultStaggerHosts = []string{
	"olv3ds.nicochristmann.net", "ctr.olv.nicochristmann.net", "npdl.cdn.pretendo.cc", "nasc.nicochristmann.net",
	"olv.nicochristmann.net", "portal.olv.nicochristmann.net", "hpp-001a2c00-l1.n.app.nicoch.net",
}

func LoadConfig(path string) (*Config, error) {
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
	if c.Bundle == "" || c.DataDir == "" {
		return fmt.Errorf("bundle and data_dir are required")
	}
	if len(c.Listeners) == 0 {
		return fmt.Errorf("at least one listener is required")
	}
	for i, l := range c.Listeners {
		if l.Listen == "" || l.Backend == "" {
			return fmt.Errorf("listener %d: listen and backend are required", i)
		}
		switch l.Mode {
		case "sni":
		case "single":
			if l.Cert == "" {
				return fmt.Errorf("listener %d: single mode needs cert", i)
			}
		default:
			return fmt.Errorf("listener %d: mode must be sni or single", i)
		}
		switch l.MinTLS {
		case "", "1.0", "1.2":
		default:
			return fmt.Errorf("listener %d: min_tls must be 1.0 or 1.2", i)
		}
		switch l.Stagger {
		case "", "sni", "all":
		default:
			return fmt.Errorf("listener %d: stagger must be sni or all", i)
		}
	}
	if c.CertSyncSeconds == 0 {
		c.CertSyncSeconds = 300
	}
	if c.PollSeconds == 0 {
		c.PollSeconds = 5
	}
	if c.StaggerHosts == nil {
		c.StaggerHosts = DefaultStaggerHosts
	}
	return nil
}

func (c *Config) CertSyncEvery() time.Duration { return time.Duration(c.CertSyncSeconds) * time.Second }
