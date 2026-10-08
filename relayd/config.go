package relayd

import (
	"encoding/json"
	"fmt"
	"github.com/Happynico7504/relaylink"
	"os"
	"regexp"
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
	Bundle          string            `json:"bundle"`
	DataDir         string            `json:"data_dir"`
	CertSyncSeconds int               `json:"cert_sync_seconds"` // default 300
	PollSeconds     int               `json:"poll_seconds"`      // invalidation poll, default 5
	Listeners       []Listener        `json:"listeners"`
	Update          UpdateConfig      `json:"update"`
	Components      []ComponentConfig `json:"components"`    // separately shipped binaries this relay runs and keeps current
	NexAuth         []string          `json:"nex_auth"`      // NEX auth servers to host: "wsc", "mk8", "badge-arcade" (needs the stream)
	ContentCache    *ContentConfig    `json:"content_cache"` // console-content cache for the OLV hosts (off unless enabled)
	StreamAddr      string            `json:"stream_addr"`   // default: the bundle host, port 7778
	StreamDisabled  bool              `json:"stream_disabled"`
	StaggerHosts    []string          `json:"stagger_hosts"` // SNI names that get the per-IP handshake stagger (sni mode)
	StaggerEmptySNI *bool             `json:"stagger_empty_sni"`
	P2P             *P2PConfig        `json:"p2p"` // UDP tunnels between consoles (off unless enabled; needs the stream)
}

// P2PConfig turns on the P2P tunnel host: the main may then put consoles that cannot reach each
// other directly on this relay (see relaylink.P2PTunnels).
type P2PConfig struct {
	Enabled bool `json:"enabled"`
	PortMin int  `json:"port_min"`      // default 61000
	PortMax int  `json:"port_max"`      // default 61999
	Trace   int  `json:"trace_packets"` // log the first N packets of each session in hex (0 = off)
}

// ComponentConfig is one separately shipped binary (for example the WSC edge). relayd
// downloads it from the main like its own updates, runs it as a child process and restarts
// just that child when a new version arrives. Nothing runs until a release is published.
type ComponentConfig struct {
	Name     string   `json:"name"`     // component name, e.g. "wscedge"
	Args     []string `json:"args"`     // command-line arguments
	Env      []string `json:"env"`      // extra environment, "KEY=value"
	Fallback string   `json:"fallback"` // binary to run until the first release arrives; "" = wait
	Disabled bool     `json:"disabled"`
}

// UpdateConfig controls over-the-air updates. They only happen if the relay's
// bundle carries a release public key (relays registered before one existed
// never auto-update).
type UpdateConfig struct {
	Disabled          bool   `json:"disabled"`
	CheckMinutes      int    `json:"check_minutes"`       // default 30
	FirstCheckSeconds int    `json:"first_check_seconds"` // default 120
	Window            string `json:"window"`              // "HH:MM-HH:MM" local time; "" = any time
}

// Listener is one console-facing TLS endpoint.
type Listener struct {
	Listen  string `json:"listen"`
	Backend string `json:"backend"` // name of a backend configured on the main
	Mode    string `json:"mode"`    // "sni": certificate chosen by the main's route table; "single": fixed Cert; "plain": no TLS
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

var componentNameRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)

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
		case "sni", "plain": // plain = no TLS at all (port 80: the consoles' connection test)
		case "single":
			if l.Cert == "" {
				return fmt.Errorf("listener %d: single mode needs cert", i)
			}
		default:
			return fmt.Errorf("listener %d: mode must be sni, single or plain", i)
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
	known := relaylink.NexGameDefaults()
	for _, g := range c.NexAuth {
		if _, ok := known[g]; !ok {
			return fmt.Errorf("nex_auth: unknown game %q", g)
		}
	}
	if len(c.NexAuth) > 0 && c.StreamDisabled {
		return fmt.Errorf("nex_auth needs the real-time stream (stream_disabled must be false)")
	}
	seen := map[string]bool{}
	for i, comp := range c.Components {
		if !componentNameRe.MatchString(comp.Name) || comp.Name == relaylink.ComponentRelayd {
			return fmt.Errorf("components[%d]: name must be lowercase letters and digits, and not %q", i, relaylink.ComponentRelayd)
		}
		if seen[comp.Name] {
			return fmt.Errorf("components[%d]: %q is listed twice", i, comp.Name)
		}
		seen[comp.Name] = true
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
