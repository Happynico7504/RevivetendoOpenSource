package main

import (
	"math/rand"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// state is one compiled, immutable view of the config; the server swaps the
// pointer atomically on reload so queries never see a half-applied config.
type state struct {
	cfg     *Config
	zone    string // fqdn, lower-case
	ns      []string
	regions []*compiledRegion
	def     *compiledRegion
	serial  uint32
	geo     Geo
}

type compiledRegion struct {
	name       string
	countries  map[string]bool
	continents map[string]bool
	targets    *targetSet
}

func compileRegion(r Region) *compiledRegion {
	cr := &compiledRegion{name: r.Name, countries: map[string]bool{}, continents: map[string]bool{}}
	for _, c := range r.Countries {
		cr.countries[strings.ToUpper(c)] = true
	}
	for _, c := range r.Continents {
		cr.continents[strings.ToUpper(c)] = true
	}
	cr.targets = newTargetSet(r.Targets, r.Health)
	return cr
}

func compile(cfg *Config, geo Geo) *state {
	st := &state{cfg: cfg, zone: dns.CanonicalName(cfg.Zone), geo: geo, serial: uint32(time.Now().Unix())}
	for _, n := range cfg.NS {
		st.ns = append(st.ns, dns.CanonicalName(n))
	}
	for _, r := range cfg.Regions {
		st.regions = append(st.regions, compileRegion(r))
	}
	d := cfg.Default
	d.Name = "default"
	st.def = compileRegion(d)
	return st
}

// pick chooses the region for a client: exact country first, then continent,
// else the default. A region with no healthy address falls back to the default.
func (s *state) pick(country, continent string) *compiledRegion {
	var chosen *compiledRegion
	for _, r := range s.regions {
		if country != "" && r.countries[country] {
			chosen = r
			break
		}
	}
	if chosen == nil {
		for _, r := range s.regions {
			if continent != "" && r.continents[continent] {
				chosen = r
				break
			}
		}
	}
	if chosen == nil || !chosen.targets.usable() {
		return s.def
	}
	return chosen
}

// Server is the DNS handler.
type Server struct {
	cur     atomic.Pointer[state]
	limiter atomic.Pointer[limiter]
}

func (s *Server) set(st *state) {
	s.cur.Store(st)
	s.limiter.Store(newLimiter(st.cfg.RateLimit.QPS, st.cfg.RateLimit.Burst))
}

func clientIP(w dns.ResponseWriter) net.IP {
	switch a := w.RemoteAddr().(type) {
	case *net.UDPAddr:
		return a.IP
	case *net.TCPAddr:
		return a.IP
	}
	return nil
}

func isUDP(w dns.ResponseWriter) bool {
	_, ok := w.RemoteAddr().(*net.UDPAddr)
	return ok
}

func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	st := s.cur.Load()
	src := clientIP(w)

	if isUDP(w) && src != nil {
		if lim := s.limiter.Load(); lim != nil && !lim.allow(src) {
			// Over the limit: drop half, answer the rest with an empty
			// truncated reply that forces a real client to retry over TCP.
			// A spoofed victim therefore receives almost nothing.
			if rand.Intn(2) == 0 {
				return
			}
			m := new(dns.Msg)
			m.SetReply(r)
			m.Truncated = true
			w.WriteMsg(m)
			return
		}
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	m.RecursionAvailable = false

	if r.Opcode != dns.OpcodeQuery {
		m.Rcode = dns.RcodeNotImplemented
		w.WriteMsg(m)
		return
	}
	if len(r.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
		w.WriteMsg(m)
		return
	}
	q := r.Question[0]
	name := dns.CanonicalName(q.Name)
	if q.Qclass != dns.ClassINET || !dns.IsSubDomain(st.zone, name) {
		m.Authoritative = false
		m.Rcode = dns.RcodeRefused
		w.WriteMsg(m)
		return
	}

	// EDNS: honour the client's UDP size and Client Subnet.
	geoIP := src
	var ecs *dns.EDNS0_SUBNET
	udpSize := 512
	if opt := r.IsEdns0(); opt != nil {
		udpSize = int(opt.UDPSize())
		if udpSize < 512 {
			udpSize = 512
		}
		if udpSize > 1232 {
			udpSize = 1232
		}
		for _, o := range opt.Option {
			if sn, ok := o.(*dns.EDNS0_SUBNET); ok && sn.SourceNetmask > 0 {
				ecs = sn
				geoIP = sn.Address
			}
		}
		m.SetEdns0(uint16(udpSize), false)
		if ecs != nil {
			ro := m.IsEdns0()
			ro.Option = append(ro.Option, &dns.EDNS0_SUBNET{
				Code: dns.EDNS0SUBNET, Family: ecs.Family, SourceNetmask: ecs.SourceNetmask,
				SourceScope: ecs.SourceNetmask, Address: ecs.Address,
			})
		}
	}

	ttl := st.cfg.TTL
	apex := name == st.zone

	switch {
	case apex && q.Qtype == dns.TypeSOA:
		m.Answer = append(m.Answer, st.soa(ttl))
	case apex && q.Qtype == dns.TypeNS:
		for _, n := range st.ns {
			m.Answer = append(m.Answer, &dns.NS{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 3600},
				Ns:  n,
			})
		}
	case q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA || q.Qtype == dns.TypeANY:
		var country, continent string
		if geoIP != nil {
			country, continent = st.geo.Lookup(geoIP)
		}
		region := st.pick(country, continent)
		if q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY {
			m.Answer = append(m.Answer, addrRecords(q.Name, region.targets.ips(false), ttl)...)
		}
		if q.Qtype == dns.TypeAAAA || q.Qtype == dns.TypeANY {
			m.Answer = append(m.Answer, addrRecords(q.Name, region.targets.ips(true), ttl)...)
		}
		if len(m.Answer) == 0 {
			m.Ns = append(m.Ns, st.soa(ttl)) // NODATA
		}
	default:
		m.Ns = append(m.Ns, st.soa(ttl)) // NODATA for every other type
	}

	if isUDP(w) {
		m.Truncate(udpSize)
	}
	w.WriteMsg(m)
}

func (s *state) soa(ttl uint32) *dns.SOA {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: s.zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: ttl},
		Ns:      s.ns[0],
		Mbox:    dns.CanonicalName(s.cfg.Hostmaster),
		Serial:  s.serial,
		Refresh: 3600, Retry: 600, Expire: 86400,
		Minttl: ttl, // negative-caching TTL
	}
}

func addrRecords(name string, ips []net.IP, ttl uint32) []dns.RR {
	ips = append([]net.IP(nil), ips...)
	rand.Shuffle(len(ips), func(i, j int) { ips[i], ips[j] = ips[j], ips[i] })
	var out []dns.RR
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}, A: v4})
		} else {
			out = append(out, &dns.AAAA{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl}, AAAA: ip})
		}
	}
	return out
}
