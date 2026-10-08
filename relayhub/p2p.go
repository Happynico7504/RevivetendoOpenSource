package relayhub

// P2P tunnel routing: a game server (wsc-secure) asks whether a gathering's consoles should
// reach each other through a UDP tunnel instead of directly, and the hub decides where that
// tunnel runs: on the main itself or on a relay (relaylink.P2PTunnels does the forwarding).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Happynico7504/relaylink"
)

// ErrNoTunnel means: the consoles connect directly, as without a tunnel.
var ErrNoTunnel = errors.New("relayhub: no tunnel for these consoles")

// MainInstance is the instance name of the tunnel host on the main.
const MainInstance = "main"

// P2PPolicy says which gatherings are tunneled.
type P2PPolicy struct {
	All           bool            // every gathering
	International bool            // gatherings whose consoles are in different regions
	NAT           bool            // gatherings with a console the game server flags as hard to reach (CGNAT, symmetric NAT, failure history)
	PIDs          map[uint32]bool // any gathering one of these consoles is in
}

func (p P2PPolicy) Off() bool { return !p.All && !p.International && !p.NAT && len(p.PIDs) == 0 }

// ParseP2PPolicy reads a policy file: "*" (everyone), "international", "nat" and/or PIDs, separated by
// whitespace or commas, "#" comments. An empty or missing file means no tunnels.
func ParseP2PPolicy(text string) P2PPolicy {
	p := P2PPolicy{PIDs: map[uint32]bool{}}
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		for _, w := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' }) {
			switch strings.ToLower(w) {
			case "*", "all":
				p.All = true
			case "international":
				p.International = true
			case "nat":
				p.NAT = true
			default:
				if n, err := strconv.ParseUint(w, 10, 32); err == nil && n != 0 {
					p.PIDs[uint32(n)] = true
				}
			}
		}
	}
	return p
}

// P2PRequest asks for a tunnel. The first station is the gathering's host.
type P2PRequest struct {
	Key      string                 `json:"key"` // e.g. "wsc:<gid>"; reopening the same key adds stations
	Stations []relaylink.P2PStation `json:"stations"`
}

type P2PResult struct {
	Instance string         `json:"instance"` // "main" or a relay id
	IP       string         `json:"ip"`       // where the consoles send their packets
	Ports    map[uint32]int `json:"ports"`    // each console's alias port
	Reason   string         `json:"reason,omitempty"`
}

// P2PInstance is a relay that can host tunnels.
type P2PInstance struct {
	ID     string
	Region string
	IP     string
	RTT    time.Duration
}

type P2PRouter struct {
	Local       *relaylink.P2PTunnels // the main's own tunnel host (nil = the main hosts none)
	LocalIP     string                // the main's public IPv4
	LocalRegion string                // the main's region in RegionCatalog terms (default "eu")
	Geo         Geo
	Policy      func() P2PPolicy
	// Relays lists the connected, enabled relays (relays that turn out not to host tunnels are
	// skipped for a while automatically).
	Relays    func(ctx context.Context) ([]P2PInstance, error)
	CallRelay func(ctx context.Context, relayID, method string, body []byte) ([]byte, error)
	Logf      func(string, ...any)
	Now       func() time.Time

	mu     sync.Mutex
	placed map[string]p2pPlacement
	broken map[string]time.Time // relay id -> skip until
}

type p2pPlacement struct {
	instance string
	ip       string
	expires  time.Time
}

// p2pPlacementTTL bounds how long the hub remembers where a session runs (the tunnel host closes
// idle sessions itself; reopening an expired one just creates it again).
const p2pPlacementTTL = 4 * time.Hour

func (r *P2PRouter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *P2PRouter) logf(f string, a ...any) {
	if r.Logf != nil {
		r.Logf(f, a...)
	}
}

// RegionOf is the RegionCatalog region of a console's IP ("" if unknown).
func (r *P2PRouter) RegionOf(ip string) string {
	if r.Geo == nil {
		return ""
	}
	p := net.ParseIP(ip)
	if p == nil {
		return ""
	}
	return RegionForClient(r.Geo.Lookup(p))
}

func (r *P2PRouter) localRegion() string {
	if r.LocalRegion != "" {
		return r.LocalRegion
	}
	return "eu"
}

// decide applies the policy. It returns why the gathering is tunneled, or "".
func (r *P2PRouter) decide(regions []string, stations []relaylink.P2PStation) string {
	if r.Policy == nil {
		return ""
	}
	p := r.Policy()
	if p.All {
		return "everyone"
	}
	for _, s := range stations {
		if p.PIDs[s.PID] {
			return fmt.Sprintf("pid %d listed", s.PID)
		}
	}
	if p.NAT {
		for _, s := range stations {
			if s.Hard != "" {
				return fmt.Sprintf("nat: pid %d %s", s.PID, s.Hard)
			}
		}
	}
	if p.International {
		seen := map[string]bool{}
		for _, reg := range regions {
			if reg != "" {
				seen[reg] = true
			}
		}
		if len(seen) > 1 {
			var l []string
			for k := range seen {
				l = append(l, k)
			}
			sort.Strings(l)
			return "international " + strings.Join(l, "+")
		}
	}
	return ""
}

type p2pCandidate struct {
	id, ip, region string
	rtt            time.Duration
	score          int // consoles in the instance's region
	hostRegion     bool
}

// candidates lists where the tunnel could run, best first: an instance in the region of as many
// consoles as possible, then one in the host's region, then the relay with the fastest link to
// the main, the main last. A tunnel in a console's own region keeps that console's leg short; for
// an international pair either end is roughly on the path, and the host's region wins the tie.
func (r *P2PRouter) candidates(ctx context.Context, regions []string) []p2pCandidate {
	var out []p2pCandidate
	add := func(id, ip, region string, rtt time.Duration) {
		c := p2pCandidate{id: id, ip: ip, region: region, rtt: rtt}
		for i, reg := range regions {
			if reg == region && reg != "" {
				c.score++
				if i == 0 {
					c.hostRegion = true
				}
			}
		}
		out = append(out, c)
	}
	if r.Local != nil && r.LocalIP != "" {
		add(MainInstance, r.LocalIP, r.localRegion(), 0)
	}
	if r.Relays != nil && r.CallRelay != nil {
		relays, err := r.Relays(ctx)
		if err != nil {
			r.logf("p2p: relay list: %v", err)
		}
		now := r.now()
		r.mu.Lock()
		for _, x := range relays {
			if until, bad := r.broken[x.ID]; bad && now.Before(until) {
				continue
			}
			add(x.ID, x.IP, x.Region, x.RTT)
		}
		r.mu.Unlock()
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.hostRegion != b.hostRegion {
			return a.hostRegion
		}
		if (a.id == MainInstance) != (b.id == MainInstance) {
			return b.id == MainInstance
		}
		return a.rtt < b.rtt
	})
	return out
}

func (r *P2PRouter) openOn(ctx context.Context, instance string, req P2PRequest) (map[uint32]int, error) {
	if instance == MainInstance {
		if r.Local == nil {
			return nil, errors.New("the main hosts no tunnels")
		}
		return r.Local.Open(req.Key, req.Stations)
	}
	body, _ := json.Marshal(relaylink.P2POpen{Key: req.Key, Stations: req.Stations})
	out, err := r.CallRelay(ctx, instance, relaylink.MethodP2POpen, body)
	if err != nil {
		return nil, err
	}
	var res relaylink.P2POpenResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	return res.Ports, nil
}

// Open returns the tunnel for a gathering: the existing one (with any new consoles added), or a
// new one if the policy wants it. ErrNoTunnel means the consoles connect directly.
func (r *P2PRouter) Open(ctx context.Context, req P2PRequest) (*P2PResult, error) {
	if req.Key == "" || len(req.Stations) == 0 {
		return nil, ErrNoTunnel
	}
	now := r.now()
	r.mu.Lock()
	pl, ok := r.placed[req.Key]
	if ok && !now.Before(pl.expires) {
		delete(r.placed, req.Key)
		ok = false
	}
	r.mu.Unlock()
	if ok {
		ports, err := r.openOn(ctx, pl.instance, req)
		if err == nil {
			r.remember(req.Key, pl.instance, pl.ip)
			return &P2PResult{Instance: pl.instance, IP: pl.ip, Ports: ports}, nil
		}
		// The consoles were already told about this tunnel; moving it would not help them.
		r.logf("p2p: %s: could not extend the tunnel on %s: %v", req.Key, pl.instance, err)
		return nil, err
	}

	// Two consoles behind one public address (same household) could not be told apart by the
	// tunnel, and they reach each other locally anyway.
	ips := map[string]bool{}
	for _, s := range req.Stations {
		if ips[s.IP] {
			return nil, ErrNoTunnel
		}
		ips[s.IP] = true
	}
	regions := make([]string, len(req.Stations))
	for i, s := range req.Stations {
		regions[i] = r.RegionOf(s.IP)
	}
	reason := r.decide(regions, req.Stations)
	if reason == "" {
		return nil, ErrNoTunnel
	}
	var lastErr error = ErrNoTunnel
	for _, c := range r.candidates(ctx, regions) {
		ports, err := r.openOn(ctx, c.id, req)
		if err != nil {
			lastErr = err
			r.logf("p2p: %s: %s cannot host the tunnel: %v", req.Key, c.id, err)
			if c.id != MainInstance {
				r.mu.Lock()
				if r.broken == nil {
					r.broken = map[string]time.Time{}
				}
				r.broken[c.id] = now.Add(2 * time.Minute)
				r.mu.Unlock()
			}
			continue
		}
		r.remember(req.Key, c.id, c.ip)
		r.logf("p2p: %s: tunnel on %s (%s) for %v regions=%v (%s)", req.Key, c.id, c.ip, ports, regions, reason)
		return &P2PResult{Instance: c.id, IP: c.ip, Ports: ports, Reason: reason}, nil
	}
	return nil, lastErr
}

func (r *P2PRouter) remember(key, instance, ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.placed == nil {
		r.placed = map[string]p2pPlacement{}
	}
	now := r.now()
	if len(r.placed) > 4096 {
		for k, v := range r.placed {
			if !now.Before(v.expires) {
				delete(r.placed, k)
			}
		}
	}
	r.placed[key] = p2pPlacement{instance: instance, ip: ip, expires: now.Add(p2pPlacementTTL)}
}

// Close ends a gathering's tunnel, wherever it runs.
func (r *P2PRouter) Close(ctx context.Context, key string) {
	r.mu.Lock()
	pl, ok := r.placed[key]
	delete(r.placed, key)
	r.mu.Unlock()
	if !ok {
		return
	}
	if pl.instance == MainInstance {
		if r.Local != nil {
			r.Local.Close(key)
		}
		return
	}
	body, _ := json.Marshal(relaylink.P2PClose{Key: key})
	if _, err := r.CallRelay(ctx, pl.instance, relaylink.MethodP2PClose, body); err != nil {
		r.logf("p2p: %s: close on %s: %v", key, pl.instance, err)
	}
}

// RelayLister builds P2PRouter.Relays from the registry and the stream hub: enabled relays with a
// live stream, their host resolved to an IPv4 address.
func RelayLister(reg Registry, streams *StreamHub, resolve func(string) (string, error)) func(ctx context.Context) ([]P2PInstance, error) {
	return func(ctx context.Context) ([]P2PInstance, error) {
		relays, err := reg.List(ctx)
		if err != nil {
			return nil, err
		}
		live := map[string]time.Duration{}
		for _, s := range streams.Status() {
			live[s.ID] = s.RTT
		}
		var out []P2PInstance
		for _, x := range relays {
			rtt, up := live[x.ID]
			if !x.Enabled || !up {
				continue
			}
			ip, err := resolve(x.Host)
			if err != nil {
				continue
			}
			out = append(out, P2PInstance{ID: x.ID, Region: x.Region, IP: ip, RTT: rtt})
		}
		return out, nil
	}
}
