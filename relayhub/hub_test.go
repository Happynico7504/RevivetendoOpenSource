package relayhub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Happynico7504/relaylink"
)

type fakeSource struct {
	mu      sync.Mutex
	pnids   map[uint64]string
	banned  map[uint64]bool
	redirs  []RedirectInfo
	queries int32
}

func (f *fakeSource) PNIDForPID(_ context.Context, pid uint64) (string, error) {
	atomic.AddInt32(&f.queries, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.pnids[pid]; ok {
		return s, nil
	}
	return "", ErrNotFound
}
func (f *fakeSource) PIDForPNID(_ context.Context, pnid string) (uint64, error) {
	atomic.AddInt32(&f.queries, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	for pid, s := range f.pnids {
		if s == pnid {
			return pid, nil
		}
	}
	return 0, ErrNotFound
}
func (f *fakeSource) Redirects(context.Context) ([]RedirectInfo, error) {
	atomic.AddInt32(&f.queries, 1)
	return f.redirs, nil
}
func (f *fakeSource) IsBanned(_ context.Context, pid uint64) (bool, error) {
	atomic.AddInt32(&f.queries, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.banned[pid], nil
}

type stack struct {
	src     *fakeSource
	log     *InvalidationLog
	reg     *MemRegistry
	fetcher *relaylink.Fetcher
	store   *relaylink.MemoryStore
	url     string
	mainPub *rsa.PublicKey
	sk      ed25519.PrivateKey
}

func newStack(t *testing.T) *stack {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub, sk, _ := ed25519.GenerateKey(rand.Reader)
	src := &fakeSource{pnids: map[uint64]string{1435853600: "ExampleUser1"}, banned: map[uint64]bool{}}
	log := NewInvalidationLog(100)
	reg := NewMemRegistry()
	if err := reg.Add(context.Background(), &Relay{ID: "us-1", Region: "na", Host: "203.0.113.5", PublicKey: pub, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	keys := &KeyLookup{Reg: reg, TTL: time.Millisecond}
	hub := &Hub{Src: src, Log: log}
	srv := &relaylink.Server{Priv: priv, RelayKey: keys.Lookup, Replay: &relaylink.MemoryReplay{}}
	ts := httptest.NewServer(srv.RPCHandler(hub.Dispatch))
	t.Cleanup(ts.Close)
	store := &relaylink.MemoryStore{}
	f := &relaylink.Fetcher{
		Client: &relaylink.Client{RelayID: "us-1", MainPub: &priv.PublicKey, Sign: sk, BaseURL: ts.URL},
		Store:  store,
	}
	return &stack{src: src, log: log, reg: reg, fetcher: f, store: store, url: ts.URL, mainPub: &priv.PublicKey, sk: sk}
}

func (s *stack) get(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	r, err := s.fetcher.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get %s: %v", path, err)
	}
	var m map[string]any
	json.Unmarshal(r.Body, &m)
	return r.Status, m
}

func TestIdentityLookupsAndCaching(t *testing.T) {
	s := newStack(t)
	if err := s.fetcher.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		st, m := s.get(t, "/relay/v1/identity/pid/1435853600")
		if st != 200 || m["pnid"] != "ExampleUser1" {
			t.Fatalf("got %d %v", st, m)
		}
	}
	if q := atomic.LoadInt32(&s.src.queries); q != 1 {
		t.Fatalf("5 identical lookups hit the database %d times", q)
	}
	st, m := s.get(t, "/relay/v1/identity/pnid/ExampleUser1")
	if st != 200 || uint64(m["pid"].(float64)) != 1435853600 {
		t.Fatalf("reverse lookup: %d %v", st, m)
	}
}

func TestUnknownIsNegativeCachedThenInvalidated(t *testing.T) {
	s := newStack(t)
	s.fetcher.Sync(context.Background())
	for i := 0; i < 3; i++ {
		if st, _ := s.get(t, "/relay/v1/identity/pid/777"); st != 404 {
			t.Fatalf("status %d", st)
		}
	}
	if q := atomic.LoadInt32(&s.src.queries); q != 1 {
		t.Fatalf("404 not cached: %d queries", q)
	}
	// The mapping appears: the writer invalidates the tag, the relay drops the 404.
	s.src.mu.Lock()
	s.src.pnids[777] = "NewPlayer"
	s.src.mu.Unlock()
	s.log.Append([]string{TagPID(777)})
	s.fetcher.Sync(context.Background())
	if st, m := s.get(t, "/relay/v1/identity/pid/777"); st != 200 || m["pnid"] != "NewPlayer" {
		t.Fatalf("after invalidation: %d %v", st, m)
	}
}

func TestBanTakesEffectOnInvalidation(t *testing.T) {
	s := newStack(t)
	s.fetcher.Sync(context.Background())
	if _, m := s.get(t, "/relay/v1/bans/42"); m["banned"] != false {
		t.Fatalf("unexpected: %v", m)
	}
	s.src.mu.Lock()
	s.src.banned[42] = true
	s.src.mu.Unlock()
	s.log.Append([]string{TagBans()})
	s.fetcher.Sync(context.Background())
	if _, m := s.get(t, "/relay/v1/bans/42"); m["banned"] != true {
		t.Fatalf("ban not visible after invalidation: %v", m)
	}
}

func TestRedirectsConfig(t *testing.T) {
	s := newStack(t)
	s.src.redirs = []RedirectInfo{{ID: 1, Type: "iosu", FromHost: "a", ToHost: "b", GameServerID: "1012F100", Port: 60000, AccessMode: "open"}}
	s.fetcher.Sync(context.Background())
	r, err := s.fetcher.Get(context.Background(), "/relay/v1/config/redirects")
	if err != nil || r.Status != 200 || !strings.Contains(string(r.Body), "1012F100") {
		t.Fatalf("%v %v", r, err)
	}
}

func TestBadAndForbiddenRequests(t *testing.T) {
	s := newStack(t)
	s.fetcher.Sync(context.Background())
	for path, want := range map[string]int{
		"/relay/v1/identity/pid/abc":                         400,
		"/relay/v1/identity/pid/0":                           400,
		"/relay/v1/identity/pid/-5":                          400,
		"/relay/v1/bans/x":                                   400,
		"/relay/v1/identity/pnid/":                           400,
		"/relay/v1/identity/pnid/" + strings.Repeat("a", 65): 400,
		"/relay/v1/nope":                                     404,
		"/admin/secrets":                                     404,
	} {
		if r, _ := s.fetcher.Get(context.Background(), path); r == nil || r.Status != want {
			t.Errorf("%s: got %v want %d", path, r, want)
		}
	}
	// Writes are not part of this API at all.
	r, err := s.fetcher.Do(context.Background(), "POST", "/relay/v1/identity/pid/1", []byte("x"))
	if err != nil || r.Status != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %v %v", r, err)
	}
	// Failed lookups are not cached as errors.
	if s.store.Len() != 0 {
		t.Fatalf("error responses were cached (%d entries)", s.store.Len())
	}
}

func TestRevokedRelayIsRejected(t *testing.T) {
	s := newStack(t)
	if err := s.fetcher.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.reg.SetEnabled(context.Background(), "us-1", false)
	time.Sleep(5 * time.Millisecond) // KeyLookup TTL in this rig is 1ms
	if _, err := s.fetcher.Get(context.Background(), "/relay/v1/ping"); err == nil {
		t.Fatal("disabled relay still served")
	}
	s.reg.SetEnabled(context.Background(), "us-1", true)
	time.Sleep(5 * time.Millisecond)
	if _, err := s.fetcher.Get(context.Background(), "/relay/v1/ping"); err != nil {
		t.Fatalf("re-enabled relay rejected: %v", err)
	}
}

func TestHubRestartFlushesRelayCache(t *testing.T) {
	s := newStack(t)
	s.fetcher.Sync(context.Background())
	s.get(t, "/relay/v1/identity/pid/1435853600")
	if s.store.Len() != 1 {
		t.Fatal("not cached")
	}
	s.log.mu.Lock()
	s.log.epoch = "new-epoch" // simulate a hub restart
	s.log.mu.Unlock()
	s.fetcher.Sync(context.Background())
	if s.store.Len() != 0 {
		t.Fatal("relay cache survived a hub restart")
	}
}

func TestInvalidationLog(t *testing.T) {
	l := NewInvalidationLog(3)
	ep := l.Batch(0, "").Epoch
	if b := l.Batch(0, ""); !b.Reset {
		t.Fatal("first contact must reset")
	}
	if b := l.Batch(0, ep); b.Reset || len(b.Events) != 0 {
		t.Fatalf("empty log: %+v", b)
	}
	for i := 0; i < 5; i++ {
		l.Append([]string{"t"})
	}
	if b := l.Batch(4, ep); b.Reset || len(b.Events) != 1 || b.Events[0].Seq != 5 {
		t.Fatalf("tail: %+v", b)
	}
	if b := l.Batch(1, ep); !b.Reset { // events 2 was trimmed (log holds 3,4,5)
		t.Fatalf("trimmed history must reset: %+v", b)
	}
	if b := l.Batch(2, ep); b.Reset || len(b.Events) != 3 {
		t.Fatalf("exact boundary: %+v", b)
	}
	if b := l.Batch(99, ep); !b.Reset { // relay claims a future sequence
		t.Fatal("future sequence must reset")
	}
	if l.Append(nil) != 5 {
		t.Fatal("empty append changed sequence")
	}
}

func TestRelayValidation(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	good := Relay{ID: "us-1", Region: "na", Host: "h", PublicKey: pub}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Relay){
		"upper id":   func(r *Relay) { r.ID = "US" },
		"empty id":   func(r *Relay) { r.ID = "" },
		"id with /":  func(r *Relay) { r.ID = "a/b" },
		"bad region": func(r *Relay) { r.Region = "mars" },
		"no host":    func(r *Relay) { r.Host = "" },
		"bad port":   func(r *Relay) { r.HealthPort = 70000 },
		"bad key":    func(r *Relay) { r.PublicKey = pub[:5] },
	} {
		r := good
		mut(&r)
		if r.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	reg := NewMemRegistry()
	reg.Add(context.Background(), &good)
	if err := reg.Add(context.Background(), &good); err == nil {
		t.Error("duplicate id accepted")
	}
}

func TestBuildDNSConfig(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	mk := func(id, region, host string, port int, on bool) *Relay {
		return &Relay{ID: id, Region: region, Host: host, HealthPort: port, PublicKey: pub, Enabled: on}
	}
	base := `{"listen":["0.0.0.0:53"],"zone":"z.example","default":{"targets":["192.0.2.1"]},"regions":[{"name":"stale"}]}`
	out, err := BuildDNSConfig([]byte(base), []*Relay{
		mk("us-2", "na", "relay-us2.example", 443, true),
		mk("us-1", "na", "relay-us1.example", 443, true),
		mk("jp-1", "jp", "203.0.113.20", 0, true),
		mk("eu-1", "eu", "relay-eu.example", 0, false), // disabled: must not appear
	})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Zone    string
		Default map[string]any
		Regions []struct {
			Name       string
			Countries  []string
			Continents []string
			Targets    []string
			Health     string
		}
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Zone != "z.example" || cfg.Default == nil {
		t.Fatalf("base fields lost: %s", out)
	}
	// asia has no relay of its own, so jp-1 serves it too (RegionFallback).
	if len(cfg.Regions) != 3 || cfg.Regions[0].Name != "jp" || cfg.Regions[1].Name != "na" || cfg.Regions[2].Name != "asia" || cfg.Regions[2].Targets[0] != "203.0.113.20" {
		t.Fatalf("regions (country-specific must come first, disabled/stale dropped): %s", out)
	}
	na := cfg.Regions[1]
	if len(na.Targets) != 2 || na.Targets[0] != "relay-us1.example" || na.Health != "tcp:443" {
		t.Fatalf("na: %+v", na)
	}
	if cfg.Regions[0].Health != "" {
		t.Fatalf("jp got a health probe it wasn't configured for")
	}
	if _, err := BuildDNSConfig([]byte("not json"), nil); err == nil {
		t.Fatal("bad base accepted")
	}
}
