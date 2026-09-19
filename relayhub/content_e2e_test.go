package relayhub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Happynico7504/relayd"
	"github.com/Happynico7504/relaylink"
)

const olvHost = "olv.nicochristmann.net"

type contentBackend struct {
	mu     sync.Mutex
	counts map[string]int // "METHOD path token" -> requests that reached the backend
	slow   chan struct{}  // GET /slow blocks until closed
	delay  time.Duration
	posts  atomic.Int32
}

func (b *contentBackend) count(method, path, token string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counts[method+" "+path+" "+token]
}

func (b *contentBackend) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, v := range b.counts {
		n += v
	}
	return n
}

func (b *contentBackend) handler(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Nintendo-Servicetoken")
	b.mu.Lock()
	b.counts[r.Method+" "+r.URL.Path+" "+token]++
	n := b.counts[r.Method+" "+r.URL.Path+" "+token]
	slow := b.slow
	b.mu.Unlock()
	if b.delay > 0 {
		time.Sleep(b.delay)
	}
	switch {
	case r.Method == http.MethodPost:
		b.posts.Add(1)
		w.WriteHeader(201)
		w.Write([]byte("created"))
	case r.URL.Path == "/slow":
		<-slow
		fmt.Fprintf(w, "slow-for-%s-#%d", token, n)
	case r.URL.Path == "/cookie":
		w.Header().Set("Set-Cookie", "sid=abc")
		w.Write([]byte("cookie"))
	case r.URL.Path == "/missing":
		http.NotFound(w, r)
	default:
		// Personalised, like Miiverse: the answer depends on who asks.
		fmt.Fprintf(w, "posts-for-%s-#%d", token, n)
	}
}

type contentRig struct {
	be      *contentBackend
	hub     *Hub
	log     *InvalidationLog
	streams *StreamHub
	rpc     *httptest.Server
	streamL net.Listener
	priv    *rsa.PrivateKey
	pub     ed25519.PublicKey
	sk      ed25519.PrivateKey
}

type contentRelay struct {
	id      string
	content *relayd.ContentCache
	fetcher *relaylink.Fetcher
	url     string
	handler http.Handler
}

func newContentRig(t *testing.T) *contentRig {
	t.Helper()
	be := &contentBackend{counts: map[string]int{}, slow: make(chan struct{})}
	bs := httptest.NewUnstartedServer(http.HandlerFunc(be.handler))
	bs.StartTLS()
	t.Cleanup(bs.Close)

	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub, sk, _ := ed25519.GenerateKey(rand.Reader)
	log := NewInvalidationLog(100)
	hub := &Hub{Log: log, Fwd: &Forwarder{Backends: map[string]Backend{"olv": {Addr: strings.TrimPrefix(bs.URL, "https://"), TLS: true}}}}
	hub.Fwd.OnWrite = func() { log.Append([]string{relaylink.ContentTag}) }
	streams := NewStreamHub()
	log.OnAppend = streams.PushInvalidation
	srv := &relaylink.Server{Priv: priv, Replay: &relaylink.MemoryReplay{},
		RelayKey: func(id string) (ed25519.PublicKey, bool) { return pub, id == "a" || id == "b" }}
	rpc := httptest.NewServer(srv.RPCHandler(hub.Dispatch))
	t.Cleanup(rpc.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeStream(ln, relaylink.StreamOptions{}, streams.Handlers, streams.OnConn)
	t.Cleanup(func() { ln.Close() })
	return &contentRig{be: be, hub: hub, log: log, streams: streams, rpc: rpc, streamL: ln, priv: priv, pub: pub, sk: sk}
}

// newRelay builds a relay exactly the way relayd's main does: a forwarding front
// with a content cache, invalidation by poll AND pushed over the stream.
func (r *contentRig) newRelay(t *testing.T, id string, synced bool) *contentRelay {
	t.Helper()
	client := &relaylink.Client{RelayID: id, MainPub: &r.priv.PublicKey, Sign: r.sk, BaseURL: r.rpc.URL}
	store := &relaylink.MemoryStore{}
	fetcher := &relaylink.Fetcher{Client: client, Store: store, MaxStale: time.Minute}
	content := relayd.NewContentCache(relayd.ContentConfig{Enabled: true}, fetcher, store)
	if synced {
		if err := fetcher.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		conn, err := client.DialStream(context.Background(), r.streamL.Addr().String(), relaylink.StreamHandlers{
			Event: func(_ *relaylink.StreamConn, topic string, body []byte) {
				if topic == TopicInvalidate {
					var m struct {
						Epoch string   `json:"epoch"`
						Seq   int64    `json:"seq"`
						Tags  []string `json:"tags"`
					}
					json.Unmarshal(body, &m)
					fetcher.ApplyPushed(m.Epoch, relaylink.Event{Seq: m.Seq, Tags: m.Tags})
				}
			},
		}, relaylink.StreamOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(conn.Close)
	}
	front := &relayd.Front{Cfg: &relayd.Config{}, Client: client, Content: content}
	h := front.Handler(relayd.Listener{Backend: "olv", Mode: "plain"})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &contentRelay{id: id, content: content, fetcher: fetcher, url: ts.URL, handler: h}
}

func (cr *contentRelay) do(t *testing.T, method, host, path, token string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, cr.url+path, nil)
	req.Host = host
	if token != "" {
		req.Header.Set("X-Nintendo-Servicetoken", token)
		req.Header.Set("X-Nintendo-Parampack", "pack-"+token)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestContentCacheServesRepeatedReadsWithoutTheMain(t *testing.T) {
	r := newContentRig(t)
	relay := r.newRelay(t, "a", true)
	for i := 0; i < 5; i++ {
		if _, body, _ := relay.do(t, "GET", olvHost, "/v1/communities", "alice"); body != "posts-for-alice-#1" {
			t.Fatalf("read %d: %q", i, body)
		}
	}
	if n := r.be.count("GET", "/v1/communities", "alice"); n != 1 {
		t.Fatalf("the main was asked %d times for 5 identical reads", n)
	}
	if s := relay.content.Stats(); s.Hits != 4 || s.Misses != 1 || s.Stores != 1 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestCacheIsMuchFasterThanTheMain(t *testing.T) {
	r := newContentRig(t)
	r.be.delay = 60 * time.Millisecond // stands in for the ocean
	relay := r.newRelay(t, "a", true)
	t0 := time.Now()
	relay.do(t, "GET", olvHost, "/v1/communities", "alice")
	miss := time.Since(t0)
	t0 = time.Now()
	relay.do(t, "GET", olvHost, "/v1/communities", "alice")
	hit := time.Since(t0)
	if miss < 60*time.Millisecond || hit > 30*time.Millisecond {
		t.Fatalf("miss %v, hit %v", miss, hit)
	}
	t.Logf("miss %v -> hit %v", miss.Round(time.Millisecond), hit.Round(time.Microsecond))
}

func TestUsersNeverSeeEachOthersContent(t *testing.T) {
	r := newContentRig(t)
	relay := r.newRelay(t, "a", true)
	for i := 0; i < 3; i++ {
		for _, u := range []string{"alice", "bob", "carol"} {
			if _, body, _ := relay.do(t, "GET", olvHost, "/v1/communities/0/posts", u); !strings.HasPrefix(body, "posts-for-"+u+"-") {
				t.Fatalf("user %s was served %q", u, body)
			}
		}
	}
	for _, u := range []string{"alice", "bob", "carol"} {
		if n := r.be.count("GET", "/v1/communities/0/posts", u); n != 1 {
			t.Fatalf("%s: backend asked %d times (each user should have exactly one miss)", u, n)
		}
	}
}

func TestAWriteFlushesEveryUsersEntriesOnEveryRelay(t *testing.T) {
	r := newContentRig(t)
	a := r.newRelay(t, "a", true)
	b := r.newRelay(t, "b", true)
	a.do(t, "GET", olvHost, "/v1/communities", "alice")
	a.do(t, "GET", olvHost, "/v1/communities", "bob")
	b.do(t, "GET", olvHost, "/v1/communities", "carol")
	if a.content.Stats().Stores != 2 || b.content.Stats().Stores != 1 {
		t.Fatal("setup did not cache")
	}

	// Alice posts (a write) through relay A.
	if code, body, _ := a.do(t, "POST", olvHost, "/v1/posts", "alice"); code != 201 || body != "created" {
		t.Fatalf("the write: %d %q", code, body)
	}
	// Relay A: everything gone immediately, including Bob's entry (full resync).
	if _, body, _ := a.do(t, "GET", olvHost, "/v1/communities", "bob"); body != "posts-for-bob-#2" {
		t.Fatalf("Bob's entry survived a write: %q", body)
	}
	if _, body, _ := a.do(t, "GET", olvHost, "/v1/communities", "alice"); body != "posts-for-alice-#2" {
		t.Fatalf("Alice does not see fresh data right after her own write: %q", body)
	}
	// Relay B never saw the write, yet it must flush too, via the push from the main.
	start := time.Now()
	deadline := time.Now().Add(2 * time.Second)
	for b.content.Stores() != 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if _, body, _ := b.do(t, "GET", olvHost, "/v1/communities", "carol"); body != "posts-for-carol-#2" {
		t.Fatalf("relay B kept a pre-write copy: %q", body)
	}
	t.Logf("the other relay flushed %v after the write", time.Since(start).Round(time.Millisecond))
	// The main was told exactly once per write.
	if r.log.Latest() < 1 {
		t.Fatal("the hub did not announce the write")
	}
}

func TestAReadThatOverlapsAWriteIsNotKept(t *testing.T) {
	r := newContentRig(t)
	relay := r.newRelay(t, "a", true)
	done := make(chan string, 1)
	go func() { _, body, _ := relay.do(t, "GET", olvHost, "/slow", "alice"); done <- body }()
	// wait until that read is inside the backend
	deadline := time.Now().Add(2 * time.Second)
	for r.be.count("GET", "/slow", "alice") == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	relay.do(t, "POST", olvHost, "/v1/posts", "bob") // the write completes while the read is still in flight
	close(r.be.slow)                                 // now the pre-write read finishes
	if body := <-done; !strings.HasPrefix(body, "slow-for-alice") {
		t.Fatalf("the reader itself still gets its answer: %q", body)
	}
	relay.do(t, "GET", olvHost, "/slow", "alice")
	if n := r.be.count("GET", "/slow", "alice"); n != 2 {
		t.Fatalf("the pre-write copy was cached (backend saw %d reads, want 2)", n)
	}
}

func TestOnlyWhatIsSafeIsCached(t *testing.T) {
	r := newContentRig(t)
	relay := r.newRelay(t, "a", true)
	close(r.be.slow)
	cases := []struct{ name, host, path string }{
		{"notification badges (real time)", olvHost, "/users/notifications.json"},
		{"a host that is not Miiverse", "boss.nicochristmann.net", "/p01/tasksheet/1/x"},
		{"an error answer", olvHost, "/missing"},
		{"an answer that sets a cookie", olvHost, "/cookie"},
	}
	for _, c := range cases {
		relay.do(t, "GET", c.host, c.path, "alice")
		relay.do(t, "GET", c.host, c.path, "alice")
		if n := r.be.count("GET", c.path, "alice"); n != 2 {
			t.Errorf("%s: cached (backend saw %d of 2 reads)", c.name, n)
		}
	}
}

func TestStaticAssetsAreSharedBetweenUsers(t *testing.T) {
	r := newContentRig(t)
	relay := r.newRelay(t, "a", true)
	// The backend keys its counter by token; assets are fetched with different tokens.
	relay.do(t, "GET", olvHost, "/assets/portal/js/juxt.global.js", "alice")
	relay.do(t, "GET", olvHost, "/assets/portal/js/juxt.global.js", "bob")
	relay.do(t, "GET", olvHost, "/assets/portal/js/juxt.global.js", "carol")
	if r.be.total() != 1 {
		t.Fatalf("static asset fetched %d times for 3 users", r.be.total())
	}
}

func TestNoCachingUntilTheInvalidationStreamIsWorking(t *testing.T) {
	r := newContentRig(t)
	relay := r.newRelay(t, "a", false) // never synced with the main
	relay.do(t, "GET", olvHost, "/v1/communities", "alice")
	relay.do(t, "GET", olvHost, "/v1/communities", "alice")
	if n := r.be.count("GET", "/v1/communities", "alice"); n != 2 {
		t.Fatalf("cached without any way to be invalidated (backend saw %d)", n)
	}
}

// orderRecorder notes the cache's flush counter at the moment the first byte of the
// answer is handed to the console.
type orderRecorder struct {
	*httptest.ResponseRecorder
	onFirst func()
	once    sync.Once
}

func (o *orderRecorder) WriteHeader(c int) { o.once.Do(o.onFirst); o.ResponseRecorder.WriteHeader(c) }
func (o *orderRecorder) Write(b []byte) (int, error) {
	o.once.Do(o.onFirst)
	return o.ResponseRecorder.Write(b)
}

func TestTheCacheIsFlushedBeforeTheConsoleHearsTheResultOfAWrite(t *testing.T) {
	r := newContentRig(t)
	relay := r.newRelay(t, "a", true)
	relay.do(t, "GET", olvHost, "/v1/communities", "alice") // something to flush
	req := httptest.NewRequest("POST", "/v1/posts", nil)
	req.Host = olvHost
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Nintendo-Servicetoken", "alice")
	flushedAtFirstByte := int64(-1)
	rec := &orderRecorder{ResponseRecorder: httptest.NewRecorder()}
	rec.onFirst = func() { flushedAtFirstByte = relay.content.Stats().Flushes }
	relay.handler.ServeHTTP(rec, req)
	if flushedAtFirstByte != 1 {
		t.Fatalf("the console got the write's answer before the cache was flushed (flushes at first byte: %d)", flushedAtFirstByte)
	}
	if rec.Code != 201 {
		t.Fatalf("status %d", rec.Code)
	}
}
