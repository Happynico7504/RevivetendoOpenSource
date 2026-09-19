package main

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// targetSet is the live set of addresses behind a region: static IPs plus
// hostnames this server resolves itself, filtered by health.
type targetSet struct {
	static []net.IP
	hosts  []string
	probe  string // "" or "tcp:<port>"

	mu       sync.RWMutex
	resolved []net.IP       // last successful resolution (kept on failure)
	failures map[string]int // consecutive probe failures per IP
}

const unhealthyAfter = 2 // consecutive failed probes

func newTargetSet(targets []string, probe string) *targetSet {
	t := &targetSet{probe: probe, failures: map[string]int{}}
	for _, s := range targets {
		if ip := net.ParseIP(s); ip != nil {
			t.static = append(t.static, ip)
		} else {
			t.hosts = append(t.hosts, s)
		}
	}
	return t
}

func (t *targetSet) all() []net.IP {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := append([]net.IP(nil), t.static...)
	return append(out, t.resolved...)
}

// ips returns the healthy addresses of the requested family.
func (t *targetSet) ips(v6 bool) []net.IP {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []net.IP
	for _, ip := range append(append([]net.IP(nil), t.static...), t.resolved...) {
		is4 := ip.To4() != nil
		if is4 == v6 {
			continue
		}
		if t.failures[ip.String()] >= unhealthyAfter {
			continue
		}
		out = append(out, ip)
	}
	return out
}

// usable reports whether any healthy address (either family) exists.
func (t *targetSet) usable() bool {
	return len(t.ips(false))+len(t.ips(true)) > 0
}

// refresh re-resolves hostnames and re-probes health. Failures never drop the
// last good resolution (an outage of the upstream resolver must not blank the
// answers).
func (t *targetSet) refresh(ctx context.Context, res *net.Resolver, logf func(string, ...any)) {
	if len(t.hosts) > 0 {
		var got []net.IP
		ok := true
		for _, h := range t.hosts {
			ips, err := res.LookupIP(ctx, "ip", strings.TrimSuffix(h, "."))
			if err != nil {
				logf("resolve %s: %v (keeping previous)", h, err)
				ok = false
				continue
			}
			got = append(got, ips...)
		}
		if ok || len(got) > 0 {
			t.mu.Lock()
			t.resolved = got
			t.mu.Unlock()
		}
	}
	if t.probe == "" {
		return
	}
	port := strings.TrimPrefix(t.probe, "tcp:")
	for _, ip := range t.all() {
		d := net.Dialer{Timeout: 2 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		t.mu.Lock()
		if err != nil {
			t.failures[ip.String()]++
			if t.failures[ip.String()] == unhealthyAfter {
				logf("target %s is now UNHEALTHY (%v)", ip, err)
			}
		} else {
			conn.Close()
			if t.failures[ip.String()] >= unhealthyAfter {
				logf("target %s is healthy again", ip)
			}
			t.failures[ip.String()] = 0
		}
		t.mu.Unlock()
	}
}

// setFailures forces a target's failure count (used by tests and tooling).
func (t *targetSet) setFailures(ip string, n int) {
	t.mu.Lock()
	t.failures[ip] = n
	t.mu.Unlock()
}
