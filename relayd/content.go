package relayd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Happynico7504/relaylink"
)

// ContentTag is carried by every cached response: invalidating it (a write
// anywhere in the network) drops the whole content cache, the "full resync".
const ContentTag = relaylink.ContentTag

// ContentConfig controls the console-content cache (config key "content_cache").
// It is OFF unless enabled, and only ever applies to the hosts listed.
type ContentConfig struct {
	Enabled        bool     `json:"enabled"`
	TTLSeconds     int      `json:"ttl_seconds"`     // default 300
	MaxMB          int      `json:"max_mb"`          // total memory budget, default 32
	MaxEntryKB     int      `json:"max_entry_kb"`    // largest cacheable response, default 1024
	Hosts          []string `json:"hosts"`           // default: the OLV (Miiverse) hostnames
	NeverCache     []string `json:"never_cache"`     // path prefixes that must always reach the main
	SharedPrefixes []string `json:"shared_prefixes"` // path prefixes identical for every user (static assets)
}

// DefaultContentHosts are the OLV (Miiverse / Juxtaposition) hostnames.
var DefaultContentHosts = []string{
	"olv.nicochristmann.net", "portal.olv.nicochristmann.net", "ctr.olv.nicochristmann.net", "olv3ds.nicochristmann.net",
	"discovery.olv.nintendo.net", "api.olv.nintendo.net", "discovery.olv.pretendo.cc",
}

// DefaultNeverCache lists responses that change in real time and are watched by
// the user as they happen: notification badges must not lag by minutes.
var DefaultNeverCache = []string{"/users/notifications", "/v1/notifications"}

// DefaultSharedPrefixes are identical for every user, so all users share one entry.
var DefaultSharedPrefixes = []string{"/assets/", "/favicon.ico"}

func (c *ContentConfig) applyDefaults() {
	if c.TTLSeconds <= 0 {
		c.TTLSeconds = 300
	}
	if c.MaxMB <= 0 {
		c.MaxMB = 32
	}
	if c.MaxEntryKB <= 0 {
		c.MaxEntryKB = 1024
	}
	if c.Hosts == nil {
		c.Hosts = DefaultContentHosts
	}
	if c.NeverCache == nil {
		c.NeverCache = DefaultNeverCache
	}
	if c.SharedPrefixes == nil {
		c.SharedPrefixes = DefaultSharedPrefixes
	}
}

// ContentStats are counters for logging and tests.
type ContentStats struct{ Hits, Misses, Stores, Flushes, Bypassed int64 }

// ContentCache caches GET responses of the console content hosts for a few
// minutes. Correctness rules, all enforced here:
//
//   - The cache key contains the host, the exact path and query, AND every
//     request header except a short ignore list. Miiverse requests carry the
//     user's service token and a parameter pack (device, region, language,
//     content restrictions): keying on all of them means one user can never be
//     served another's personalised answer. Only the configured static-asset
//     prefixes are shared between users.
//   - Only 200 responses without Set-Cookie / no-store / private, of bounded
//     size, are stored. Anything else always goes to the main.
//   - ANY write (non-GET/HEAD/OPTIONS) through this relay flushes everything at
//     once, and a read that overlapped a write is never stored. The main tells
//     every other relay to flush too (the shared invalidation tag).
//   - If the relay has not heard from the main recently, the cache is bypassed.
type ContentCache struct {
	cfg     ContentConfig
	ttl     time.Duration
	maxBody int
	hosts   map[string]bool
	Fetcher *relaylink.Fetcher // invalidation sync: trust, generation, flush
	Store   *relaylink.MemoryStore

	inflightWrites                          atomic.Int32
	hits, misses, stores, flushes, bypassed atomic.Int64
}

// NewContentCache builds the cache; store must be the one the fetcher invalidates.
func NewContentCache(cfg ContentConfig, fetcher *relaylink.Fetcher, store *relaylink.MemoryStore) *ContentCache {
	cfg.applyDefaults()
	c := &ContentCache{cfg: cfg, ttl: time.Duration(cfg.TTLSeconds) * time.Second, maxBody: cfg.MaxEntryKB << 10, Fetcher: fetcher, Store: store, hosts: map[string]bool{}}
	store.MaxBytes = int64(cfg.MaxMB) << 20
	for _, h := range cfg.Hosts {
		c.hosts[strings.ToLower(h)] = true
	}
	return c
}

// Config returns the effective configuration (defaults applied).
func (c *ContentCache) Config() ContentConfig { return c.cfg }

func (c *ContentCache) Stats() ContentStats {
	return ContentStats{c.hits.Load(), c.misses.Load(), c.stores.Load(), c.flushes.Load(), c.bypassed.Load()}
}

func hostOf(r *http.Request) string {
	h := strings.ToLower(r.Host)
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h
}

// Covers reports whether a request is for one of the cached hosts at all.
func (c *ContentCache) Covers(r *http.Request) bool { return c.hosts[hostOf(r)] }

// IsWrite reports whether a request changes state.
func IsWrite(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

func (c *ContentCache) shared(path string) bool {
	for _, p := range c.cfg.SharedPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// eligible: may this GET be served from / stored in the cache?
func (c *ContentCache) eligible(r *http.Request) bool {
	if r.Method != http.MethodGet || !c.Covers(r) {
		return false
	}
	if r.Header.Get("Range") != "" || r.Header.Get("If-Range") != "" {
		return false
	}
	path := r.URL.Path
	for _, p := range c.cfg.NeverCache {
		if strings.HasPrefix(path, p) {
			return false
		}
	}
	return true
}

// headers that never change what the server answers (or that identify the
// transport, not the user), so they do not split the key.
var keyIgnored = map[string]bool{
	"date": true, "connection": true, "keep-alive": true, "proxy-connection": true,
	"content-length": true, "transfer-encoding": true, "te": true, "upgrade": true, "trailer": true,
	"x-forwarded-for": true, "x-real-ip": true, "x-request-id": true,
}

// Key derives the cache key of an eligible request. Deterministic: header names
// are lowercased and sorted, so the order a console sends them in is irrelevant.
func (c *ContentCache) Key(r *http.Request) string {
	h := sha256.New()
	h.Write([]byte(hostOf(r) + "\x00" + r.URL.RequestURI() + "\x00"))
	if c.shared(r.URL.Path) {
		h.Write([]byte("shared\x00" + r.Header.Get("Accept-Encoding")))
		return hex.EncodeToString(h.Sum(nil))
	}
	// Read the values straight from the map (not by name): a header stored under
	// a different spelling must still count, or two different requests could
	// collide on one key.
	byName := map[string][]string{}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if !keyIgnored[lk] {
			byName[lk] = append(byName[lk], vs...)
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		vals := append([]string(nil), byName[n]...)
		sort.Strings(vals)
		h.Write([]byte(n + "\x01" + strings.Join(vals, "\x02") + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Lookup returns a cached response for an eligible request, or the key and
// generation to store the fresh answer under.
func (c *ContentCache) Lookup(r *http.Request) (resp *relaylink.ForwardResponse, key string, gen uint64, cacheable bool) {
	if !c.eligible(r) {
		return nil, "", 0, false
	}
	if !c.Fetcher.Trusted() { // cannot vouch for freshness: never serve from cache
		c.bypassed.Add(1)
		return nil, "", 0, false
	}
	key = c.Key(r)
	gen = c.Fetcher.Gen()
	if e, ok := c.Store.Get(key); ok {
		var fr relaylink.ForwardResponse
		if json.Unmarshal(e.Body, &fr) == nil {
			c.hits.Add(1)
			return &fr, key, gen, true
		}
	}
	c.misses.Add(1)
	return nil, key, gen, true
}

func storableResponse(fr *relaylink.ForwardResponse, maxBody int) bool {
	if fr.Status != http.StatusOK || fr.Close || len(fr.Body) > maxBody {
		return false
	}
	for k, vs := range fr.Headers {
		switch strings.ToLower(k) {
		case "set-cookie", "set-cookie2":
			return false
		case "cache-control":
			for _, v := range vs {
				lv := strings.ToLower(v)
				if strings.Contains(lv, "no-store") || strings.Contains(lv, "private") || strings.Contains(lv, "no-cache") {
					return false
				}
			}
		}
	}
	return true
}

// Save stores a fresh response unless something invalidated the cache (or a
// write is in flight) since the request started.
func (c *ContentCache) Save(key string, gen uint64, fr *relaylink.ForwardResponse) bool {
	if key == "" || !storableResponse(fr, c.maxBody) {
		return false
	}
	if c.inflightWrites.Load() != 0 || c.Fetcher.Gen() != gen || !c.Fetcher.Trusted() {
		return false
	}
	body, _ := json.Marshal(fr)
	c.Store.Set(key, &relaylink.Response{Status: 200, Body: body, Tags: []string{ContentTag}}, c.ttl)
	c.stores.Add(1)
	return true
}

// BeginWrite / EndWrite bracket a write that passes through this relay. The
// caller flushes (EndWrite does) before the console gets its answer, so the
// console's very next read is fresh.
func (c *ContentCache) BeginWrite() { c.inflightWrites.Add(1) }

func (c *ContentCache) EndWrite() {
	c.Fetcher.Flush()
	c.flushes.Add(1)
	c.inflightWrites.Add(-1)
}

// Stores returns the number of entries currently cached.
func (c *ContentCache) Stores() int { return c.Store.Len() }
