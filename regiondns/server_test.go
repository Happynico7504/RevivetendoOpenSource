package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// fakeGeo maps fixed test addresses to countries/continents. Safe for the
// server goroutines to read while a test mutates it.
type fakeGeo struct {
	mu sync.Mutex
	m  map[string][2]string
}

func (f *fakeGeo) Lookup(ip net.IP) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.m[ip.String()]
	return v[0], v[1]
}

func (f *fakeGeo) set(ip string, country, continent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if country == "" {
		delete(f.m, ip)
		return
	}
	f.m[ip] = [2]string{country, continent}
}

func testConfig() *Config {
	c := &Config{
		Listen:     []string{"127.0.0.1:0"},
		Zone:       "regionselect.example.net",
		NS:         []string{"ns.example.net"},
		Hostmaster: "hostmaster.example.net",
		TTL:        60,
		Default:    Region{Targets: []string{"192.0.2.1"}},
		Regions: []Region{
			{Name: "us", Countries: []string{"US", "CA"}, Continents: []string{"NA", "SA"}, Targets: []string{"192.0.2.10", "2001:db8::10"}},
			{Name: "jp", Countries: []string{"JP"}, Continents: []string{"AS"}, Targets: []string{"192.0.2.20"}},
			{Name: "mx", Countries: []string{"MX"}, Targets: []string{"192.0.2.30"}},
		},
	}
	c.RateLimit.QPS, c.RateLimit.Burst = 1000, 1000
	return c
}

var testGeo = &fakeGeo{m: map[string][2]string{
	"198.51.100.1": {"US", "NA"},
	"198.51.100.2": {"JP", "AS"},
	"198.51.100.3": {"KR", "AS"}, // no KR rule -> continent AS -> jp
	"198.51.100.4": {"BR", "SA"}, // continent SA -> us
	"198.51.100.5": {"DE", "EU"}, // nothing -> default
	"198.51.100.6": {"MX", "NA"}, // country MX beats continent NA
}}

type harness struct {
	srv  *Server
	addr string
	stop func()
}

func start(t *testing.T, cfg *Config) *harness {
	t.Helper()
	srv := &Server{}
	srv.set(compile(cfg, testGeo))
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	us := &dns.Server{PacketConn: pc, Handler: srv}
	ts := &dns.Server{Listener: ln, Handler: srv}
	started := make(chan struct{}, 2)
	us.NotifyStartedFunc = func() { started <- struct{}{} }
	ts.NotifyStartedFunc = func() { started <- struct{}{} }
	go us.ActivateAndServe()
	go ts.ActivateAndServe()
	<-started
	<-started
	return &harness{srv: srv, addr: addr, stop: func() { us.Shutdown(); ts.Shutdown() }}
}

func (h *harness) query(t *testing.T, name string, qtype uint16, ecsIP string, tcp bool) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	if ecsIP != "" {
		m.SetEdns0(1232, false)
		ip := net.ParseIP(ecsIP)
		m.IsEdns0().Option = append(m.IsEdns0().Option, &dns.EDNS0_SUBNET{
			Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 32, Address: ip})
	}
	c := new(dns.Client)
	if tcp {
		c.Net = "tcp"
	}
	c.Timeout = 2 * time.Second
	r, _, err := c.Exchange(m, h.addr)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	return r
}

func answerIPs(r *dns.Msg) []string {
	var out []string
	for _, rr := range r.Answer {
		switch v := rr.(type) {
		case *dns.A:
			out = append(out, v.A.String())
		case *dns.AAAA:
			out = append(out, v.AAAA.String())
		}
	}
	return out
}

func TestRegionSelection(t *testing.T) {
	h := start(t, testConfig())
	defer h.stop()
	cases := []struct{ ecs, want string }{
		{"198.51.100.1", "192.0.2.10"}, // US country
		{"198.51.100.2", "192.0.2.20"}, // JP country
		{"198.51.100.3", "192.0.2.20"}, // continent AS
		{"198.51.100.4", "192.0.2.10"}, // continent SA
		{"198.51.100.5", "192.0.2.1"},  // default
		{"198.51.100.6", "192.0.2.30"}, // country beats continent
		{"203.0.113.99", "192.0.2.1"},  // unknown -> default
	}
	for _, c := range cases {
		r := h.query(t, "regionselect.example.net", dns.TypeA, c.ecs, false)
		got := answerIPs(r)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("client %s: got %v want %s", c.ecs, got, c.want)
		}
		if !r.Authoritative || r.RecursionAvailable {
			t.Errorf("flags wrong: aa=%v ra=%v", r.Authoritative, r.RecursionAvailable)
		}
	}
}

func TestSourceAddressUsedWithoutECS(t *testing.T) {
	// Tests connect from 127.0.0.1, which fakeGeo doesn't know -> default.
	h := start(t, testConfig())
	defer h.stop()
	got := answerIPs(h.query(t, "regionselect.example.net", dns.TypeA, "", false))
	if len(got) != 1 || got[0] != "192.0.2.1" {
		t.Fatalf("got %v", got)
	}
	testGeo.set("127.0.0.1", "JP", "AS")
	defer testGeo.set("127.0.0.1", "", "")
	got = answerIPs(h.query(t, "regionselect.example.net", dns.TypeA, "", false))
	if len(got) != 1 || got[0] != "192.0.2.20" {
		t.Fatalf("source address not used for geo: %v", got)
	}
}

func TestECSEchoedWithScope(t *testing.T) {
	h := start(t, testConfig())
	defer h.stop()
	r := h.query(t, "regionselect.example.net", dns.TypeA, "198.51.100.1", false)
	opt := r.IsEdns0()
	if opt == nil {
		t.Fatal("no OPT in response")
	}
	found := false
	for _, o := range opt.Option {
		if sn, ok := o.(*dns.EDNS0_SUBNET); ok {
			found = true
			if sn.SourceScope != 32 || sn.Address.String() != "198.51.100.1" {
				t.Errorf("bad ECS echo: %+v", sn)
			}
		}
	}
	if !found {
		t.Fatal("ECS not echoed")
	}
}

func TestAAAAAndNODATA(t *testing.T) {
	h := start(t, testConfig())
	defer h.stop()
	r := h.query(t, "regionselect.example.net", dns.TypeAAAA, "198.51.100.1", false)
	if got := answerIPs(r); len(got) != 1 || got[0] != "2001:db8::10" {
		t.Fatalf("AAAA: %v", got)
	}
	// JP has no IPv6: NOERROR, empty answer, SOA in authority (NODATA).
	r = h.query(t, "regionselect.example.net", dns.TypeAAAA, "198.51.100.2", false)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 || len(r.Ns) != 1 {
		t.Fatalf("expected NODATA, got rcode=%d ans=%d ns=%d", r.Rcode, len(r.Answer), len(r.Ns))
	}
	if _, ok := r.Ns[0].(*dns.SOA); !ok {
		t.Fatal("authority is not SOA")
	}
	// Types we never serve are also NODATA, not errors.
	for _, ty := range []uint16{dns.TypeTXT, dns.TypeMX, dns.TypeCNAME} {
		r = h.query(t, "regionselect.example.net", ty, "", false)
		if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
			t.Errorf("type %d: rcode=%d answers=%d", ty, r.Rcode, len(r.Answer))
		}
	}
}

func TestApexSOAandNS(t *testing.T) {
	h := start(t, testConfig())
	defer h.stop()
	r := h.query(t, "regionselect.example.net", dns.TypeSOA, "", false)
	if len(r.Answer) != 1 {
		t.Fatalf("SOA answers: %d", len(r.Answer))
	}
	soa := r.Answer[0].(*dns.SOA)
	if soa.Ns != "ns.example.net." || soa.Mbox != "hostmaster.example.net." {
		t.Fatalf("bad SOA: %v", soa)
	}
	r = h.query(t, "regionselect.example.net", dns.TypeNS, "", false)
	if len(r.Answer) != 1 || r.Answer[0].(*dns.NS).Ns != "ns.example.net." {
		t.Fatalf("bad NS answer: %v", r.Answer)
	}
}

func TestSubnamesAreGeoAnswered(t *testing.T) {
	h := start(t, testConfig())
	defer h.stop()
	got := answerIPs(h.query(t, "Any.Label.RegionSelect.Example.NET", dns.TypeA, "198.51.100.2", false))
	if len(got) != 1 || got[0] != "192.0.2.20" {
		t.Fatalf("subname (mixed case): %v", got)
	}
}

func TestOutOfZoneRefused(t *testing.T) {
	h := start(t, testConfig())
	defer h.stop()
	for _, n := range []string{"example.com", "example.net", "notregionselect.example.net"} {
		r := h.query(t, n, dns.TypeA, "", false)
		if r.Rcode != dns.RcodeRefused || r.Authoritative || len(r.Answer) != 0 {
			t.Errorf("%s: rcode=%d aa=%v", n, r.Rcode, r.Authoritative)
		}
	}
}

func TestTCP(t *testing.T) {
	h := start(t, testConfig())
	defer h.stop()
	got := answerIPs(h.query(t, "regionselect.example.net", dns.TypeA, "198.51.100.2", true))
	if len(got) != 1 || got[0] != "192.0.2.20" {
		t.Fatalf("tcp: %v", got)
	}
}

func TestBadRequests(t *testing.T) {
	h := start(t, testConfig())
	defer h.stop()
	c := &dns.Client{Timeout: 2 * time.Second}

	m := new(dns.Msg)
	m.SetQuestion("regionselect.example.net.", dns.TypeA)
	m.Opcode = dns.OpcodeNotify
	if r, _, err := c.Exchange(m, h.addr); err != nil || r.Rcode != dns.RcodeNotImplemented {
		t.Errorf("non-query opcode: %v %v", r, err)
	}
	m = new(dns.Msg)
	m.Id = dns.Id()
	m.Question = []dns.Question{
		{Name: "regionselect.example.net.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
		{Name: "regionselect.example.net.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET},
	}
	if r, _, err := c.Exchange(m, h.addr); err != nil || r.Rcode != dns.RcodeFormatError {
		t.Errorf("two questions: %v %v", r, err)
	}
	m = new(dns.Msg)
	m.SetQuestion("regionselect.example.net.", dns.TypeA)
	m.Question[0].Qclass = dns.ClassCHAOS
	if r, _, err := c.Exchange(m, h.addr); err != nil || r.Rcode != dns.RcodeRefused {
		t.Errorf("CHAOS class: %v %v", r, err)
	}
}

func TestUnhealthyRegionFallsBackToDefault(t *testing.T) {
	h := start(t, testConfig())
	defer h.stop()
	st := h.srv.cur.Load()
	for _, r := range st.regions {
		if r.name == "jp" {
			r.targets.setFailures("192.0.2.20", unhealthyAfter) // all JP targets down
		}
	}
	got := answerIPs(h.query(t, "regionselect.example.net", dns.TypeA, "198.51.100.2", false))
	if len(got) != 1 || got[0] != "192.0.2.1" {
		t.Fatalf("expected default while JP unhealthy, got %v", got)
	}
	for _, r := range st.regions {
		if r.name == "jp" {
			r.targets.setFailures("192.0.2.20", 0)
		}
	}
	got = answerIPs(h.query(t, "regionselect.example.net", dns.TypeA, "198.51.100.2", false))
	if len(got) != 1 || got[0] != "192.0.2.20" {
		t.Fatalf("expected JP after recovery, got %v", got)
	}
}

func TestHealthProbeMarksDeadTargetDown(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ts := newTargetSet([]string{"127.0.0.1"}, "tcp:"+itoa(port))
	logf := func(string, ...any) {}
	ts.refresh(t.Context(), net.DefaultResolver, logf)
	if !ts.usable() {
		t.Fatal("listening target reported unhealthy")
	}
	ln.Close()
	for i := 0; i < unhealthyAfter; i++ {
		ts.refresh(t.Context(), net.DefaultResolver, logf)
	}
	if ts.usable() {
		t.Fatal("closed target still healthy after repeated failures")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestRateLimitBlunts(t *testing.T) {
	cfg := testConfig()
	cfg.RateLimit.QPS, cfg.RateLimit.Burst = 0.001, 5 // effectively no refill during the test
	h := start(t, cfg)
	defer h.stop()
	answered, truncated, dropped := 0, 0, 0
	for i := 0; i < 60; i++ {
		m := new(dns.Msg)
		m.SetQuestion("regionselect.example.net.", dns.TypeA)
		c := &dns.Client{Timeout: 150 * time.Millisecond}
		r, _, err := c.Exchange(m, h.addr)
		switch {
		case err != nil:
			dropped++
		case r.Truncated:
			truncated++
			if len(r.Answer) != 0 {
				t.Fatal("truncated reply carries answers (amplification!)")
			}
		default:
			answered++
		}
	}
	if answered > 6 {
		t.Errorf("answered %d of 60 despite burst 5", answered)
	}
	if dropped == 0 || truncated == 0 {
		t.Errorf("expected both drops and TC replies, got dropped=%d truncated=%d", dropped, truncated)
	}
	// TCP is never rate limited: a real client can always fall back.
	if got := answerIPs(h.query(t, "regionselect.example.net", dns.TypeA, "", true)); len(got) != 1 {
		t.Errorf("TCP fallback failed: %v", got)
	}
}

func TestLimiterPrefixGrouping(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(1, 2)
	l.now = func() time.Time { return now }
	a, b := net.ParseIP("198.51.100.1"), net.ParseIP("198.51.100.200") // same /24
	if !l.allow(a) || !l.allow(b) {
		t.Fatal("burst not honoured")
	}
	if l.allow(a) {
		t.Fatal("same /24 not sharing a bucket")
	}
	if !l.allow(net.ParseIP("203.0.113.1")) {
		t.Fatal("different /24 wrongly limited")
	}
	now = now.Add(2 * time.Second)
	if !l.allow(a) {
		t.Fatal("bucket did not refill")
	}
}

func TestMultipleTargetsAllReturned(t *testing.T) {
	cfg := testConfig()
	cfg.Regions[0].Targets = []string{"192.0.2.10", "192.0.2.11", "192.0.2.12"}
	h := start(t, cfg)
	defer h.stop()
	got := answerIPs(h.query(t, "regionselect.example.net", dns.TypeA, "198.51.100.1", false))
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
}

func TestConfigValidation(t *testing.T) {
	bad := map[string]func(*Config){
		"no listen":        func(c *Config) { c.Listen = nil },
		"no ns":            func(c *Config) { c.NS = nil },
		"ns is ip":         func(c *Config) { c.NS = []string{"192.0.2.1"} },
		"no default":       func(c *Config) { c.Default.Targets = nil },
		"dup region":       func(c *Config) { c.Regions[1].Name = "us" },
		"region no match":  func(c *Config) { c.Regions[0].Countries, c.Regions[0].Continents = nil, nil },
		"region no target": func(c *Config) { c.Regions[0].Targets = nil },
		"bad health":       func(c *Config) { c.Regions[0].Health = "http:80" },
		"bad health port":  func(c *Config) { c.Regions[0].Health = "tcp:99999" },
		"no hostmaster":    func(c *Config) { c.Hostmaster = "" },
	}
	for name, mutate := range bad {
		c := testConfig()
		mutate(c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := testConfig().validate(); err != nil {
		t.Fatalf("good config rejected: %v", err)
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"listen":["127.0.0.1:0"],"zone":"z.example.net","ns":["n.example.net"],`+
		`"hostmaster":"h.example.net","default":{"targets":["192.0.2.1"]},"typo_field":1}`), 0o644)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("unknown field accepted (a typo would silently do nothing)")
	}
}
