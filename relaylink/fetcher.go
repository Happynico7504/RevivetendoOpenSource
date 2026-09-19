package relaylink

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Paths of the sync API the main serves (see relayhub).
const InvalidationsPath = "/relay/v1/sync/invalidations"

// InvalidationBatch is the main's answer on InvalidationsPath.
type InvalidationBatch struct {
	Epoch  string  `json:"epoch"`  // changes whenever the main's log restarted
	Latest int64   `json:"latest"` // highest sequence number issued
	Reset  bool    `json:"reset"`  // relay is too far behind: flush everything
	Events []Event `json:"events"`
}

// Event invalidates every cached response carrying one of Tags.
type Event struct {
	Seq  int64    `json:"seq"`
	Tags []string `json:"tags"`
}

// Fetcher is a relay's caching front for GET requests to the main.
//
//   - Responses are cached exactly as long as the main said (capped by MaxTTL).
//   - Concurrent identical misses share one request to the main.
//   - Negative answers are cached when the main gives them a TTL.
//   - The main's invalidation stream is polled; a cache that has not heard
//     from the main for MaxStale is bypassed entirely, so a relay never serves
//     data it cannot know is current. This is a cache to save calls, not an
//     outage mode.
type Fetcher struct {
	Client       *Client
	Store        Store
	MaxTTL       time.Duration // cap on any TTL from the main (default 30 days)
	MaxStale     time.Duration // stop trusting the cache after this long without a sync (default 45s)
	PollInterval time.Duration // default 5s
	Now          func() time.Time
	Logf         func(string, ...any)

	mu      sync.Mutex
	epoch   string
	lastSeq int64
	lastOK  time.Time
	gen     uint64 // bumped whenever an invalidation is applied
	flights map[string]*flight
}

type flight struct {
	done chan struct{}
	resp *Response
	err  error
}

func (f *Fetcher) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Fetcher) logf(format string, a ...any) {
	if f.Logf != nil {
		f.Logf(format, a...)
	}
}

func (f *Fetcher) maxTTL() time.Duration {
	if f.MaxTTL > 0 {
		return f.MaxTTL
	}
	return 30 * 24 * time.Hour
}

func (f *Fetcher) maxStale() time.Duration {
	if f.MaxStale > 0 {
		return f.MaxStale
	}
	return 45 * time.Second
}

// trusted reports whether the cache may be used right now.
func (f *Fetcher) trusted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.epoch != "" && f.now().Sub(f.lastOK) <= f.maxStale()
}

// Trusted reports whether a cache guarded by this fetcher may be used right now
// (the invalidation stream has been heard from recently).
func (f *Fetcher) Trusted() bool { return f.trusted() }

// Gen returns a counter that changes whenever an invalidation or flush was
// applied. A cache records it before fetching and stores the answer only if it is
// unchanged afterwards, so a response that raced with a change is never kept.
func (f *Fetcher) Gen() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen
}

// Flush empties the store and bumps the generation (used when a write passes
// through this relay: everything cached is now suspect).
func (f *Fetcher) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Store.Flush()
	f.gen++
}

// Sync polls the invalidation stream once and applies it.
func (f *Fetcher) Sync(ctx context.Context) error {
	f.mu.Lock()
	after, epoch := f.lastSeq, f.epoch
	f.mu.Unlock()
	path := fmt.Sprintf("%s?after=%d&epoch=%s", InvalidationsPath, after, epoch)
	resp, err := f.Client.Call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if resp.Status != http.StatusOK {
		return fmt.Errorf("relaylink: invalidation sync: status %d", resp.Status)
	}
	var b InvalidationBatch
	if err := json.Unmarshal(resp.Body, &b); err != nil {
		return fmt.Errorf("relaylink: invalidation sync: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if b.Reset || b.Epoch != f.epoch {
		f.Store.Flush() // first contact, main restarted, or we fell behind
		f.gen++
	} else {
		for _, e := range b.Events {
			for _, t := range e.Tags {
				f.Store.DeleteTag(t)
			}
			f.gen++
		}
	}
	f.epoch = b.Epoch
	f.lastSeq = b.Latest
	f.lastOK = f.now()
	return nil
}

// ApplyPushed applies one invalidation delivered over the real-time stream. It
// is applied only if it is the very next event of the epoch this relay follows;
// anything else (other epoch, gap, duplicate) is ignored and the regular poll
// repairs the state. So a push can make invalidation faster but never wrong.
func (f *Fetcher) ApplyPushed(epoch string, e Event) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.epoch == "" || epoch != f.epoch || e.Seq != f.lastSeq+1 {
		return false
	}
	for _, t := range e.Tags {
		f.Store.DeleteTag(t)
	}
	f.lastSeq = e.Seq
	f.gen++
	return true
}

// Run polls until ctx is cancelled.
func (f *Fetcher) Run(ctx context.Context) {
	iv := f.PollInterval
	if iv <= 0 {
		iv = 5 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if err := f.Sync(ctx); err != nil {
			f.logf("invalidation sync failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Get returns the response for a GET path, from cache when allowed.
func (f *Fetcher) Get(ctx context.Context, path string) (*Response, error) {
	key := "GET " + path
	useCache := f.trusted()
	if useCache {
		if r, ok := f.Store.Get(key); ok {
			return r, nil
		}
	}

	f.mu.Lock()
	if f.flights == nil {
		f.flights = map[string]*flight{}
	}
	if fl, ok := f.flights[key]; ok {
		f.mu.Unlock()
		select {
		case <-fl.done:
			return fl.resp, fl.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	fl := &flight{done: make(chan struct{})}
	f.flights[key] = fl
	startGen := f.gen
	f.mu.Unlock()

	resp, err := f.Client.Call(ctx, http.MethodGet, path, nil)

	f.mu.Lock()
	// Only cache if the cache is trusted and no invalidation was applied while
	// this request was in flight (the answer might predate the change).
	ok := err == nil && resp.TTL > 0 && useCache && f.gen == startGen
	delete(f.flights, key)
	f.mu.Unlock()
	if ok && (resp.Status == http.StatusOK || resp.Status == http.StatusNotFound) {
		ttl := time.Duration(resp.TTL) * time.Second
		if ttl > f.maxTTL() {
			ttl = f.maxTTL()
		}
		f.Store.Set(key, resp, ttl)
	}
	fl.resp, fl.err = resp, err
	close(fl.done)
	return resp, err
}

// Do performs an uncached request (writes, live calls).
func (f *Fetcher) Do(ctx context.Context, method, path string, body []byte) (*Response, error) {
	return f.Client.Call(ctx, method, path, body)
}

// ApplyBatchForTest applies an invalidation batch exactly as Sync does after
// receiving it. Exported for tests in other packages.
func (f *Fetcher) ApplyBatchForTest(b InvalidationBatch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b.Reset || b.Epoch != f.epoch {
		f.Store.Flush()
		f.gen++
	}
	f.epoch, f.lastSeq, f.lastOK = b.Epoch, b.Latest, f.now()
}
