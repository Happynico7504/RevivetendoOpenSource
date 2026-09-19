// regiondns is a small authoritative DNS server that answers one delegated
// zone with region-dependent A/AAAA records. It is not a recursive resolver:
// it refuses everything outside its zone.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/miekg/dns"
)

func buildState(cfg *Config) (*state, error) {
	var geo Geo = noGeo{}
	if cfg.GeoIPDB != "" {
		g, err := openMMDB(cfg.GeoIPDB)
		if err != nil {
			return nil, err
		}
		geo = g
	} else {
		log.Printf("warning: no geoip_db configured, every client gets the default region")
	}
	return compile(cfg, geo), nil
}

func resolverFor(cfg *Config) *net.Resolver {
	if cfg.ResolverAddr == "" {
		return net.DefaultResolver
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "udp", cfg.ResolverAddr)
		},
	}
}

func refreshAll(st *state) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res := resolverFor(st.cfg)
	for _, r := range append([]*compiledRegion{st.def}, st.regions...) {
		r.targets.refresh(ctx, res, func(f string, a ...any) { log.Printf("["+r.name+"] "+f, a...) })
	}
}

func main() {
	cfgPath := flag.String("config", "regiondns.json", "path to the JSON config")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	st, err := buildState(cfg)
	if err != nil {
		log.Fatalf("geoip: %v", err)
	}
	srv := &Server{}
	srv.set(st)

	var servers []*dns.Server
	for _, addr := range cfg.Listen {
		for _, network := range []string{"udp", "tcp"} {
			s := &dns.Server{Addr: addr, Net: network, Handler: srv, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
			servers = append(servers, s)
			go func() {
				if err := s.ListenAndServe(); err != nil {
					log.Fatalf("listen %s/%s: %v", s.Net, s.Addr, err)
				}
			}()
		}
	}
	log.Printf("regiondns serving %s on %v", st.zone, cfg.Listen)
	// First hostname resolution and health probes run in the background so
	// unreachable relays (2s probe timeout each) never delay the server coming
	// up. Until they finish, regions have no resolved/probed targets and
	// clients simply get the default region.
	go refreshAll(st)

	// Periodic refresh (hostname re-resolution + health probes) and config
	// reload on change or SIGHUP. An invalid new config is ignored.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGINT, syscall.SIGTERM)
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	lastMod := modTime(*cfgPath)
	reload := func() {
		nc, err := loadConfig(*cfgPath)
		if err != nil {
			log.Printf("reload rejected, keeping previous config: %v", err)
			return
		}
		ns, err := buildState(nc)
		if err != nil {
			log.Printf("reload rejected, keeping previous config: %v", err)
			return
		}
		refreshAll(ns)
		srv.set(ns)
		st = ns
		log.Printf("config reloaded")
	}
	for {
		select {
		case <-tick.C:
			if m := modTime(*cfgPath); !m.Equal(lastMod) {
				lastMod = m
				reload()
				continue
			}
			refreshAll(st)
		case <-hup:
			reload()
		case <-term:
			for _, s := range servers {
				s.Shutdown()
			}
			return
		}
	}
}

func modTime(p string) time.Time {
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
