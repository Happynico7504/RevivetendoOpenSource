package relayd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Happynico7504/relaylink"
)

func newTestContent(t *testing.T, cfg ContentConfig) (*ContentCache, *relaylink.Fetcher, *relaylink.MemoryStore) {
	t.Helper()
	store := &relaylink.MemoryStore{}
	f := &relaylink.Fetcher{Store: store, MaxStale: time.Minute}
	f.ApplyBatchForTest(relaylink.InvalidationBatch{Epoch: "e1", Latest: 0}) // as if the first sync succeeded
	cfg.Enabled = true
	return NewContentCache(cfg, f, store), f, store
}

func req(method, host, target string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(method, "http://"+host+target, nil)
	r.Host = host
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

const olv = "olv.nicochristmann.net"

func TestKeySeparatesEveryUserAndContext(t *testing.T) {
	c, _, _ := newTestContent(t, ContentConfig{})
	base := map[string]string{"X-Nintendo-Servicetoken": "tok-alice", "X-Nintendo-Parampack": "pp-1", "User-Agent": "WiiU/POLV"}
	key := func(host, target string, h map[string]string) string { return c.Key(req("GET", host, target, h)) }
	ref := key(olv, "/v1/communities/0/posts?limit=50", base)

	if key(olv, "/v1/communities/0/posts?limit=50", base) != ref {
		t.Fatal("the key is not deterministic")
	}
	// Everything that could change the answer must change the key.
	variants := map[string]string{
		"another user (service token)":                     key(olv, "/v1/communities/0/posts?limit=50", with(base, "X-Nintendo-Servicetoken", "tok-bob")),
		"another region/language/restrictions (parampack)": key(olv, "/v1/communities/0/posts?limit=50", with(base, "X-Nintendo-Parampack", "pp-2")),
		"another platform (user agent)":                    key(olv, "/v1/communities/0/posts?limit=50", with(base, "User-Agent", "CTR/3DS")),
		"another query":                                    key(olv, "/v1/communities/0/posts?limit=51", base),
		"another path":                                     key(olv, "/v1/communities/1/posts?limit=50", base),
		"another host":                                     key("portal.olv.nicochristmann.net", "/v1/communities/0/posts?limit=50", base),
		"an extra Authorization header":                    key(olv, "/v1/communities/0/posts?limit=50", with(base, "Authorization", "Bearer x")),
		"an extra Cookie":                                  key(olv, "/v1/communities/0/posts?limit=50", with(base, "Cookie", "sid=1")),
		"no identity at all":                               key(olv, "/v1/communities/0/posts?limit=50", nil),
	}
	seen := map[string]string{ref: "reference"}
	for name, k := range variants {
		if prev, dup := seen[k]; dup {
			t.Errorf("%s produced the same key as %s: two different requests would share one cache entry", name, prev)
		}
		seen[k] = name
	}
	// Things that never change the answer must NOT split the cache.
	same := map[string]map[string]string{
		"client address header": with(base, "X-Forwarded-For", "203.0.113.9"),
		"connection headers":    with(with(base, "Connection", "keep-alive"), "Keep-Alive", "timeout=5"),
		"a date":                with(base, "Date", "Sat, 19 Sep 2026 10:00:00 GMT"),
	}
	for name, h := range same {
		if key(olv, "/v1/communities/0/posts?limit=50", h) != ref {
			t.Errorf("%s changed the key", name)
		}
	}
	// Header NAME case and the order they arrive in are irrelevant.
	a := req("GET", olv, "/x", nil)
	a.Header["x-nintendo-servicetoken"] = []string{"t"}
	a.Header.Set("Accept", "*/*")
	b := req("GET", olv, "/x", nil)
	b.Header.Set("Accept", "*/*")
	b.Header.Set("X-Nintendo-Servicetoken", "t")
	if c.Key(a) != c.Key(b) {
		t.Error("header case/order changed the key")
	}
	// Host with a port is the same host.
	if c.Key(req("GET", olv+":443", "/x", base)) != c.Key(req("GET", olv, "/x", base)) {
		t.Error("a port in the Host header split the key")
	}
}

func with(m map[string]string, k, v string) map[string]string {
	out := map[string]string{}
	for a, b := range m {
		out[a] = b
	}
	out[k] = v
	return out
}

func TestStaticAssetsAreSharedBetweenUsersButNothingElse(t *testing.T) {
	c, _, _ := newTestContent(t, ContentConfig{})
	alice := map[string]string{"X-Nintendo-Servicetoken": "alice", "User-Agent": "WiiU"}
	bob := map[string]string{"X-Nintendo-Servicetoken": "bob", "User-Agent": "CTR"}
	if c.Key(req("GET", olv, "/assets/portal/js/juxt.global.js", alice)) != c.Key(req("GET", olv, "/assets/portal/js/juxt.global.js", bob)) {
		t.Error("static assets are not shared, so the cache barely helps")
	}
	if c.Key(req("GET", olv, "/v1/communities", alice)) == c.Key(req("GET", olv, "/v1/communities", bob)) {
		t.Fatal("PERSONAL content shared between two users")
	}
	// Compression variants of an asset must not be mixed up.
	if c.Key(req("GET", olv, "/assets/x.js", with(alice, "Accept-Encoding", "gzip"))) == c.Key(req("GET", olv, "/assets/x.js", with(alice, "Accept-Encoding", "br"))) {
		t.Error("gzip and br copies share a key")
	}
	// A path that merely CONTAINS the prefix later on is not shared.
	if c.Key(req("GET", olv, "/v1/assets/private", alice)) == c.Key(req("GET", olv, "/v1/assets/private", bob)) {
		t.Error("prefix match is not anchored at the start of the path")
	}
}

func TestEligibility(t *testing.T) {
	c, _, _ := newTestContent(t, ContentConfig{})
	ok := func(r *http.Request) bool { return c.eligible(r) }
	if !ok(req("GET", olv, "/v1/communities", nil)) {
		t.Fatal("a normal GET on an OLV host is not cacheable")
	}
	for name, r := range map[string]*http.Request{
		"POST":                   req("POST", olv, "/v1/posts", nil),
		"PUT":                    req("PUT", olv, "/x", nil),
		"DELETE":                 req("DELETE", olv, "/x", nil),
		"HEAD":                   req("HEAD", olv, "/x", nil),
		"a host that is not OLV": req("GET", "boss.nicochristmann.net", "/p01/tasksheet/1/x", nil),
		"the account API":        req("GET", "act.nicochristmann.net", "/v1/api/people/@me", nil),
		"notifications":          req("GET", olv, "/users/notifications.json", nil),
		"notifications (API)":    req("GET", olv, "/v1/notifications", nil),
		"a Range request":        req("GET", olv, "/x", map[string]string{"Range": "bytes=0-9"}),
	} {
		if ok(r) {
			t.Errorf("%s was treated as cacheable", name)
		}
	}
	if !IsWrite("POST") || !IsWrite("PATCH") || !IsWrite("DELETE") || !IsWrite("PUT") || IsWrite("GET") || IsWrite("HEAD") || IsWrite("OPTIONS") {
		t.Fatal("IsWrite classifies methods wrongly")
	}
}

func fr(status int, body string, hdr map[string][]string) *relaylink.ForwardResponse {
	return &relaylink.ForwardResponse{Status: status, Body: []byte(body), Headers: hdr}
}

func TestOnlySafeResponsesAreStored(t *testing.T) {
	c, _, store := newTestContent(t, ContentConfig{MaxEntryKB: 1})
	r := req("GET", olv, "/v1/communities", map[string]string{"X-Nintendo-Servicetoken": "a"})
	_, key, gen, cacheable := c.Lookup(r)
	if !cacheable {
		t.Fatal("not cacheable")
	}
	for name, resp := range map[string]*relaylink.ForwardResponse{
		"404":                    fr(404, "nope", nil),
		"redirect":               fr(302, "", map[string][]string{"Location": {"/x"}}),
		"server error":           fr(500, "boom", nil),
		"Set-Cookie":             fr(200, "ok", map[string][]string{"Set-Cookie": {"sid=1"}}),
		"cache-control private":  fr(200, "ok", map[string][]string{"Cache-Control": {"private, max-age=0"}}),
		"cache-control no-store": fr(200, "ok", map[string][]string{"Cache-Control": {"no-store"}}),
		"connection close":       {Status: 200, Body: []byte("ok"), Close: true},
		"too large":              fr(200, strings.Repeat("x", 2048), nil),
	} {
		if c.Save(key, gen, resp) {
			t.Errorf("%s was stored", name)
		}
	}
	if store.Len() != 0 {
		t.Fatalf("%d entries stored", store.Len())
	}
	if !c.Save(key, gen, fr(200, "fine", map[string][]string{"Content-Type": {"application/xml"}})) {
		t.Fatal("a plain 200 was refused")
	}
	if store.Len() != 1 {
		t.Fatal("not stored")
	}
}

func TestHitMissAndExpiry(t *testing.T) {
	c, _, store := newTestContent(t, ContentConfig{TTLSeconds: 300})
	now := time.Unix(1_800_000_000, 0)
	store.Now = func() time.Time { return now }
	r := req("GET", olv, "/v1/communities", map[string]string{"X-Nintendo-Servicetoken": "a"})
	hit, key, gen, _ := c.Lookup(r)
	if hit != nil {
		t.Fatal("hit on an empty cache")
	}
	c.Save(key, gen, fr(200, "posts-for-a", map[string][]string{"Content-Type": {"application/xml"}}))
	hit, _, _, _ = c.Lookup(r)
	if hit == nil || string(hit.Body) != "posts-for-a" || hit.Headers["Content-Type"][0] != "application/xml" || hit.Status != 200 {
		t.Fatalf("hit: %+v", hit)
	}
	// Another user asking for the very same URL gets nothing from this entry.
	if other, _, _, _ := c.Lookup(req("GET", olv, "/v1/communities", map[string]string{"X-Nintendo-Servicetoken": "b"})); other != nil {
		t.Fatal("user B was served user A's cached response")
	}
	now = now.Add(299 * time.Second)
	if hit, _, _, _ = c.Lookup(r); hit == nil {
		t.Fatal("expired before the 5 minutes were up")
	}
	now = now.Add(2 * time.Second)
	if hit, _, _, _ = c.Lookup(r); hit != nil {
		t.Fatal("still served after the 5 minute TTL")
	}
	s := c.Stats()
	if s.Hits != 2 || s.Stores != 1 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestAWriteFlushesEverythingAndRacingReadsAreNotKept(t *testing.T) {
	c, _, store := newTestContent(t, ContentConfig{})
	users := []string{"a", "b", "c"}
	for _, u := range users { // three users each have entries
		r := req("GET", olv, "/v1/communities", map[string]string{"X-Nintendo-Servicetoken": u})
		_, k, g, _ := c.Lookup(r)
		c.Save(k, g, fr(200, "for-"+u, nil))
	}
	if store.Len() != 3 {
		t.Fatalf("setup: %d entries", store.Len())
	}

	// A read starts (its generation is noted) before the write...
	slow := req("GET", olv, "/v1/communities/0/posts", map[string]string{"X-Nintendo-Servicetoken": "a"})
	_, slowKey, slowGen, _ := c.Lookup(slow)
	c.BeginWrite()
	// ...a read that starts DURING the write must not be stored either...
	during := req("GET", olv, "/v1/communities/0/posts", map[string]string{"X-Nintendo-Servicetoken": "b"})
	_, dKey, dGen, _ := c.Lookup(during)
	if c.Save(dKey, dGen, fr(200, "during", nil)) {
		t.Fatal("a response fetched while a write was in flight was stored")
	}
	c.EndWrite()
	// ...and the one that started before it finishes AFTER the flush: also refused.
	if c.Save(slowKey, slowGen, fr(200, "pre-write copy", nil)) {
		t.Fatal("a pre-write response was stored after the flush (it would serve stale data for 5 minutes)")
	}
	if store.Len() != 0 {
		t.Fatalf("the write left %d entries behind (a full resync must drop everything)", store.Len())
	}
	if c.Stats().Flushes != 1 {
		t.Fatalf("flushes: %d", c.Stats().Flushes)
	}
	// After the write, normal caching resumes.
	_, k, g, _ := c.Lookup(during)
	if !c.Save(k, g, fr(200, "fresh", nil)) {
		t.Fatal("caching did not resume after the write")
	}
	// Two writes overlapping: caching stays off until BOTH are done.
	c.BeginWrite()
	c.BeginWrite()
	c.EndWrite()
	_, k, g, _ = c.Lookup(during)
	if c.Save(k, g, fr(200, "x", nil)) {
		t.Fatal("stored while one write was still in flight")
	}
	c.EndWrite()
}

func TestNeverServedWhenTheMainCannotBeHeardFrom(t *testing.T) {
	store := &relaylink.MemoryStore{}
	f := &relaylink.Fetcher{Store: store, MaxStale: time.Minute} // never synced
	c := NewContentCache(ContentConfig{Enabled: true}, f, store)
	r := req("GET", olv, "/v1/communities", map[string]string{"X-Nintendo-Servicetoken": "a"})
	if hit, key, _, cacheable := c.Lookup(r); hit != nil || key != "" || cacheable {
		t.Fatal("the cache was used although the invalidation stream was never heard from")
	}
	if c.Save("k", 0, fr(200, "x", nil)) || store.Len() != 0 {
		t.Fatal("stored without a working invalidation stream")
	}
	// Once synced it works; when the main goes quiet for longer than MaxStale it stops again.
	now := time.Unix(1_800_000_000, 0)
	f.Now = func() time.Time { return now }
	f.ApplyBatchForTest(relaylink.InvalidationBatch{Epoch: "e", Latest: 0})
	_, k, g, cacheable := c.Lookup(r)
	if !cacheable || !c.Save(k, g, fr(200, "x", nil)) {
		t.Fatal("not usable after a sync")
	}
	now = now.Add(2 * time.Minute)
	if hit, _, _, _ := c.Lookup(r); hit != nil {
		t.Fatal("a stale cache (no contact with the main for 2 minutes) was still served")
	}
	if c.Stats().Bypassed == 0 {
		t.Fatal("bypasses were not counted")
	}
}

func TestMemoryBudgetIsEnforced(t *testing.T) {
	c, _, store := newTestContent(t, ContentConfig{MaxMB: 1, MaxEntryKB: 400})
	body := strings.Repeat("x", 300<<10)
	for i := 0; i < 30; i++ {
		r := req("GET", olv, "/v1/communities", map[string]string{"X-Nintendo-Servicetoken": string(rune('a' + i))})
		_, k, g, _ := c.Lookup(r)
		c.Save(k, g, fr(200, body, nil))
	}
	if store.Bytes() > 1<<20 {
		t.Fatalf("budget of 1 MB exceeded: %d bytes", store.Bytes())
	}
	if store.Len() == 0 {
		t.Fatal("nothing kept at all")
	}
}
