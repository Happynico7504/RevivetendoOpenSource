package relaylink

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMain is a stand-in for the hub: a data map with per-path TTL/tags, an
// invalidation log, and call counters.
type fakeMain struct {
	mu     sync.Mutex
	epoch  string
	events []Event
	seq    int64
	data   map[string]*Response
	calls  map[string]*int32
	delay  chan struct{} // if non-nil, data requests block until it is closed
	down   atomic.Bool
}

func (m *fakeMain) counter(path string) *int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.calls[path]
	if c == nil {
		c = new(int32)
		m.calls[path] = c
	}
	return c
}

func (m *fakeMain) invalidate(tags ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.events = append(m.events, Event{Seq: m.seq, Tags: tags})
}

func (m *fakeMain) dispatch(_ context.Context, req *Request) *Response {
	if m.down.Load() {
		return &Response{Status: 503}
	}
	path := req.Path
	if strings.HasPrefix(path, InvalidationsPath) {
		m.mu.Lock()
		defer m.mu.Unlock()
		var after int64
		var epoch string
		q := path[strings.Index(path, "?")+1:]
		for _, kv := range strings.Split(q, "&") {
			p := strings.SplitN(kv, "=", 2)
			switch p[0] {
			case "after":
				after = atoi64(p[1])
			case "epoch":
				epoch = p[1]
			}
		}
		b := InvalidationBatch{Epoch: m.epoch, Latest: m.seq}
		if epoch != m.epoch {
			b.Reset = true
		} else {
			for _, e := range m.events {
				if e.Seq > after {
					b.Events = append(b.Events, e)
				}
			}
		}
		return JSON(200, b, 0)
	}
	atomic.AddInt32(m.counter(path), 1)
	if m.delay != nil {
		<-m.delay
	}
	m.mu.Lock()
	r := m.data[path]
	m.mu.Unlock()
	if r == nil {
		return &Response{Status: 404, TTL: 30, Tags: []string{"missing"}}
	}
	cp := *r
	return &cp
}

func atoi64(s string) int64 {
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}

type cacheRig struct {
	main  *fakeMain
	f     *Fetcher
	store *MemoryStore
	now   *time.Time
	srv   *httptest.Server
	mu    sync.Mutex
}

func newCacheRig(t *testing.T) *cacheRig {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub, sk, _ := ed25519.GenerateKey(rand.Reader)
	m := &fakeMain{epoch: "e1", data: map[string]*Response{}, calls: map[string]*int32{}}
	server := &Server{
		Priv:     priv,
		RelayKey: func(string) (ed25519.PublicKey, bool) { return pub, true },
		Replay:   &MemoryReplay{},
	}
	srv := httptest.NewServer(server.RPCHandler(m.dispatch))
	t.Cleanup(srv.Close)
	now := time.Unix(1_800_000_000, 0)
	r := &cacheRig{main: m, now: &now, srv: srv}
	clock := func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return *r.now }
	r.store = &MemoryStore{Now: clock}
	r.f = &Fetcher{
		Client: &Client{RelayID: "us-1", MainPub: &priv.PublicKey, Sign: sk, BaseURL: srv.URL},
		Store:  r.store, Now: clock, MaxStale: 30 * time.Second,
	}
	return r
}

func (r *cacheRig) advance(d time.Duration) { r.mu.Lock(); *r.now = r.now.Add(d); r.mu.Unlock() }

func (r *cacheRig) get(t *testing.T, path string) *Response {
	t.Helper()
	resp, err := r.f.Get(context.Background(), path)
	if err != nil {
		t.Fatalf("Get %s: %v", path, err)
	}
	return resp
}

func (r *cacheRig) calls(path string) int { return int(atomic.LoadInt32(r.main.counter(path))) }

func TestCacheHitAvoidsMainCall(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/x"] = &Response{Status: 200, Body: []byte("v1"), TTL: 60, Tags: []string{"t"}}
	if err := r.f.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if got := r.get(t, "/x"); string(got.Body) != "v1" {
			t.Fatalf("body %q", got.Body)
		}
	}
	if c := r.calls("/x"); c != 1 {
		t.Fatalf("main called %d times, want 1", c)
	}
}

func TestTTLExpiryAndZeroTTL(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/short"] = &Response{Status: 200, Body: []byte("s"), TTL: 10}
	r.main.data["/never"] = &Response{Status: 200, Body: []byte("n"), TTL: 0}
	r.f.Sync(context.Background())
	r.get(t, "/short")
	r.get(t, "/never")
	r.get(t, "/never")
	if c := r.calls("/never"); c != 2 {
		t.Fatalf("TTL 0 was cached (%d calls)", c)
	}
	r.advance(5 * time.Second)
	r.f.Sync(context.Background())
	r.get(t, "/short")
	if c := r.calls("/short"); c != 1 {
		t.Fatalf("expired too early: %d", c)
	}
	r.advance(6 * time.Second)
	r.f.Sync(context.Background())
	r.get(t, "/short")
	if c := r.calls("/short"); c != 2 {
		t.Fatalf("did not refetch after TTL: %d", c)
	}
}

func TestMaxTTLCap(t *testing.T) {
	r := newCacheRig(t)
	r.f.MaxTTL = 20 * time.Second
	r.main.data["/long"] = &Response{Status: 200, Body: []byte("l"), TTL: 3600}
	r.f.Sync(context.Background())
	r.get(t, "/long")
	r.advance(25 * time.Second)
	r.f.Sync(context.Background())
	r.get(t, "/long")
	if c := r.calls("/long"); c != 2 {
		t.Fatalf("MaxTTL not applied: %d calls", c)
	}
}

func TestNegativeCaching(t *testing.T) {
	r := newCacheRig(t)
	r.f.Sync(context.Background())
	for i := 0; i < 4; i++ {
		if got := r.get(t, "/nope"); got.Status != 404 {
			t.Fatalf("status %d", got.Status)
		}
	}
	if c := r.calls("/nope"); c != 1 {
		t.Fatalf("404 not cached: %d calls", c)
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/boom"] = &Response{Status: 500, Body: []byte("x"), TTL: 60}
	r.f.Sync(context.Background())
	r.get(t, "/boom")
	r.get(t, "/boom")
	if c := r.calls("/boom"); c != 2 {
		t.Fatalf("5xx cached: %d", c)
	}
}

func TestTagInvalidation(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/a"] = &Response{Status: 200, Body: []byte("a1"), TTL: 600, Tags: []string{"pid:1"}}
	r.main.data["/b"] = &Response{Status: 200, Body: []byte("b1"), TTL: 600, Tags: []string{"pid:2"}}
	r.f.Sync(context.Background())
	r.get(t, "/a")
	r.get(t, "/b")

	r.main.data["/a"] = &Response{Status: 200, Body: []byte("a2"), TTL: 600, Tags: []string{"pid:1"}}
	r.main.invalidate("pid:1")
	r.f.Sync(context.Background())

	if got := r.get(t, "/a"); string(got.Body) != "a2" {
		t.Fatalf("stale after invalidation: %q", got.Body)
	}
	r.get(t, "/b")
	if c := r.calls("/b"); c != 1 {
		t.Fatalf("unrelated key evicted: %d calls", c)
	}
}

func TestMainRestartFlushesEverything(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/a"] = &Response{Status: 200, Body: []byte("a"), TTL: 600}
	r.f.Sync(context.Background())
	r.get(t, "/a")
	r.main.mu.Lock()
	r.main.epoch = "e2" // hub restarted: its log (and sequence numbers) are gone
	r.main.seq = 0
	r.main.events = nil
	r.main.mu.Unlock()
	r.f.Sync(context.Background())
	r.get(t, "/a")
	if c := r.calls("/a"); c != 2 {
		t.Fatalf("cache survived a main restart: %d calls", c)
	}
}

func TestStaleCacheIsBypassed(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/a"] = &Response{Status: 200, Body: []byte("a"), TTL: 600}
	r.f.Sync(context.Background())
	r.get(t, "/a")
	// No successful sync for longer than MaxStale: cached data may be outdated
	// and we cannot know, so every read must go to the main.
	r.advance(31 * time.Second)
	r.get(t, "/a")
	r.get(t, "/a")
	if c := r.calls("/a"); c != 3 {
		t.Fatalf("stale cache still served: %d calls", c)
	}
	// After the sync recovers, the main reports no invalidation for the entry
	// it already held, so that entry is known-current and is served again.
	r.f.Sync(context.Background())
	r.get(t, "/a")
	r.get(t, "/a")
	if c := r.calls("/a"); c != 3 {
		t.Fatalf("cache did not resume after sync: %d calls", c)
	}
	// ...but if it HAD been invalidated during the gap, the catch-up drops it.
	r.main.data["/a"] = &Response{Status: 200, Body: []byte("a2"), TTL: 600}
	r.main.invalidate("nothing-to-do-with-a")
	r.advance(31 * time.Second)
	r.f.Sync(context.Background())
	if got := r.get(t, "/a"); string(got.Body) != "a" {
		t.Fatalf("entry unrelated to the event was dropped: %q", got.Body)
	}
}

func TestNothingCachedBeforeFirstSync(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/a"] = &Response{Status: 200, Body: []byte("a"), TTL: 600}
	r.get(t, "/a")
	r.get(t, "/a")
	if c := r.calls("/a"); c != 2 {
		t.Fatalf("cached before the first sync: %d", c)
	}
	if r.store.Len() != 0 {
		t.Fatal("store populated before first sync")
	}
}

func TestSingleflightSharesConcurrentMisses(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/hot"] = &Response{Status: 200, Body: []byte("h"), TTL: 60}
	r.f.Sync(context.Background())
	r.main.delay = make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := r.f.Get(context.Background(), "/hot"); err != nil || string(got.Body) != "h" {
				t.Errorf("get: %v %v", got, err)
			}
		}()
	}
	time.Sleep(200 * time.Millisecond) // let them pile up behind the one in flight
	close(r.main.delay)
	wg.Wait()
	if c := r.calls("/hot"); c != 1 {
		t.Fatalf("25 concurrent misses caused %d main calls", c)
	}
}

func TestInvalidationDuringFetchIsNotCached(t *testing.T) {
	// Response fetched before a change, invalidation applied before it is
	// stored: storing it would resurrect stale data until its TTL.
	r := newCacheRig(t)
	r.main.data["/a"] = &Response{Status: 200, Body: []byte("old"), TTL: 600, Tags: []string{"t"}}
	r.f.Sync(context.Background())
	r.main.delay = make(chan struct{})
	done := make(chan *Response)
	go func() { resp, _ := r.f.Get(context.Background(), "/a"); done <- resp }()
	time.Sleep(150 * time.Millisecond) // request is now inside the main, holding "old"
	r.main.mu.Lock()
	r.main.data["/a"] = &Response{Status: 200, Body: []byte("new"), TTL: 600, Tags: []string{"t"}}
	r.main.mu.Unlock()
	r.main.invalidate("t")
	r.f.Sync(context.Background()) // relay applies the invalidation mid-fetch
	close(r.main.delay)
	<-done
	r.main.delay = nil
	if got := r.get(t, "/a"); string(got.Body) != "new" {
		t.Fatalf("stale response was cached: %q", got.Body)
	}
}

func TestWritesAreNeverCached(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/w"] = &Response{Status: 200, Body: []byte("w"), TTL: 600}
	r.f.Sync(context.Background())
	r.f.Do(context.Background(), "POST", "/w", []byte("x"))
	r.f.Do(context.Background(), "POST", "/w", []byte("x"))
	if c := r.calls("/w"); c != 2 || r.store.Len() != 0 {
		t.Fatalf("write path cached: calls=%d entries=%d", c, r.store.Len())
	}
}

func TestMainDownIsAnErrorNotStaleData(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/a"] = &Response{Status: 200, Body: []byte("a"), TTL: 600}
	r.f.Sync(context.Background())
	r.get(t, "/a")
	r.main.down.Store(true)
	r.advance(60 * time.Second) // beyond MaxStale
	resp, err := r.f.Get(context.Background(), "/a")
	if err == nil && resp.Status == 200 {
		t.Fatal("served cached data after losing contact with the main")
	}
}

func TestMemoryStoreCapAndTags(t *testing.T) {
	s := &MemoryStore{Max: 3}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		s.Set(k, &Response{Status: 200, Tags: []string{"all"}}, time.Minute)
	}
	if s.Len() > 3 {
		t.Fatalf("cap exceeded: %d", s.Len())
	}
	s.DeleteTag("all")
	if s.Len() != 0 {
		t.Fatalf("tag delete left %d", s.Len())
	}
	if j, _ := json.Marshal(JSON(200, map[string]int{"a": 1}, 5, "t")); len(j) == 0 {
		t.Fatal("JSON helper")
	}
}

func TestPushedInvalidationIsAppliedOnlyInOrder(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/a"] = &Response{Status: 200, Body: []byte("a1"), TTL: 600, Tags: []string{"t"}}
	r.f.Sync(context.Background())
	r.get(t, "/a")
	if r.store.Len() != 1 {
		t.Fatal("not cached")
	}
	// Wrong epoch, a gap and a duplicate must all be ignored (the poll repairs).
	if r.f.ApplyPushed("other-epoch", Event{Seq: 1, Tags: []string{"t"}}) || r.store.Len() != 1 {
		t.Fatal("event from another epoch applied")
	}
	if r.f.ApplyPushed("e1", Event{Seq: 5, Tags: []string{"t"}}) || r.store.Len() != 1 {
		t.Fatal("event after a gap applied")
	}
	if !r.f.ApplyPushed("e1", Event{Seq: 1, Tags: []string{"t"}}) || r.store.Len() != 0 {
		t.Fatal("the next event in order was not applied")
	}
	if r.f.ApplyPushed("e1", Event{Seq: 1, Tags: []string{"t"}}) {
		t.Fatal("duplicate applied twice")
	}
	// The regular poll then finds nothing left to do (it is already at seq 1).
	r.main.mu.Lock()
	r.main.seq, r.main.events = 1, []Event{{Seq: 1, Tags: []string{"t"}}}
	r.main.mu.Unlock()
	if err := r.f.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Before the first sync nothing is applied at all.
	fresh := newCacheRig(t)
	if fresh.f.ApplyPushed("e1", Event{Seq: 1, Tags: []string{"t"}}) {
		t.Fatal("push applied before the first sync")
	}
}

func TestMemoryStoreByteCapAndAccounting(t *testing.T) {
	s := &MemoryStore{MaxBytes: 10_000}
	big := make([]byte, 3000)
	for i := 0; i < 20; i++ {
		s.Set(fmt.Sprintf("k%d", i), &Response{Status: 200, Body: big, Tags: []string{"t"}}, time.Minute)
	}
	if s.Bytes() > 10_000 {
		t.Fatalf("byte budget exceeded: %d", s.Bytes())
	}
	if s.Len() == 0 || s.Len() > 3 {
		t.Fatalf("entries after eviction: %d", s.Len())
	}
	// One entry bigger than the whole budget is refused, not stored.
	s.Set("huge", &Response{Status: 200, Body: make([]byte, 20_000)}, time.Minute)
	if _, ok := s.Get("huge"); ok {
		t.Fatal("an entry larger than the budget was stored")
	}
	// Replacing a key does not double-count it; removal and flush return the bytes.
	s.Flush()
	s.Set("a", &Response{Status: 200, Body: big}, time.Minute)
	s.Set("a", &Response{Status: 200, Body: big}, time.Minute)
	one := s.Bytes()
	s.DeleteTag("nothing")
	if one > 3200 || one < 3000 {
		t.Fatalf("accounting after a replace: %d", one)
	}
	s.Flush()
	if s.Bytes() != 0 {
		t.Fatalf("flush left %d bytes accounted", s.Bytes())
	}
}

func TestFetcherFlushBumpsTheGeneration(t *testing.T) {
	r := newCacheRig(t)
	r.main.data["/a"] = &Response{Status: 200, Body: []byte("a"), TTL: 600}
	r.f.Sync(context.Background())
	r.get(t, "/a")
	g := r.f.Gen()
	r.f.Flush()
	if r.store.Len() != 0 || r.f.Gen() == g {
		t.Fatal("Flush did not empty the store and change the generation")
	}
	if !r.f.Trusted() {
		t.Fatal("a synced fetcher must stay trusted after a flush")
	}
}
