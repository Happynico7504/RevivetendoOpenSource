package relayhub

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Happynico7504/relaylink"
)

// Backend is a local service the main will replay relay requests against.
type Backend struct {
	Addr string // host:port
	TLS  bool   // dial with TLS (certificate not verified: it is loopback)
}

// DefaultBackends mirrors what nginx / the account listener expose today:
//
//	olv     127.0.0.1:7443  account-proxy's SNI-routed HTTPS listener (nginx :443 stream)
//	account 127.0.0.1:6666  account-proxy's account API listener
//	hpp     127.0.0.1:9010  swapdoodle HPP (nginx terminates TLS on :9013 today)
//	web     127.0.0.1:80    nginx on plain HTTP (the consoles' conntest.* connection test, routed by Host)
func DefaultBackends() map[string]Backend {
	return map[string]Backend{
		"web":     {Addr: "127.0.0.1:80", TLS: false},
		"olv":     {Addr: "127.0.0.1:7443", TLS: true},
		"account": {Addr: "127.0.0.1:6666", TLS: true},
		"hpp":     {Addr: "127.0.0.1:9010", TLS: false},
	}
}

const maxForwardBody = 16 << 20

// Forwarder replays relay requests against the configured backends over
// pooled keep-alive connections (the backends throttle new TLS handshakes per
// source address, so reusing connections matters).
type Forwarder struct {
	Backends map[string]Backend
	// OnWrite is called for every non-GET/HEAD/OPTIONS request forwarded to the
	// "olv" backend (Miiverse content), whether or not it succeeded: relays'
	// content caches must be flushed (the main appends relaylink.ContentTag to the
	// invalidation log, which is pushed to every relay).
	OnWrite func()
	// IdleTimeout ends a streamed forward the relay stopped reading (default 60s).
	IdleTimeout time.Duration

	mu         sync.Mutex
	transports map[string]*http.Transport

	smu      sync.Mutex
	sessions map[string]*fwdSession
}

var hopByHop = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Transfer-Encoding": true,
	"Te": true, "Trailer": true, "Upgrade": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Content-Length": true, // recomputed
}

func (f *Forwarder) transport(name string, be Backend, sni string) *http.Transport {
	key := name + "|" + strings.ToLower(sni)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.transports == nil {
		f.transports = map[string]*http.Transport{}
	}
	if t, ok := f.transports[key]; ok {
		return t
	}
	t := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "tcp", be.Addr)
		},
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true,
		ForceAttemptHTTP2:     false,
	}
	if be.TLS {
		t.TLSClientConfig = &tls.Config{ServerName: sni, InsecureSkipVerify: true}
		t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{} // stay on HTTP/1.1
	}
	f.transports[key] = t
	return t
}

// Do performs one forwarded request for the relay `relayID`. Small answers come
// back inline; a large one (more than ForwardInlineMax) is streamed: the answer
// carries the first part and a StreamID, and the relay fetches the rest with
// Chunk.
func (f *Forwarder) Do(ctx context.Context, relayID string, fr *relaylink.ForwardRequest) *relaylink.Response {
	be, ok := f.Backends[fr.Backend]
	if !ok {
		return &relaylink.Response{Status: http.StatusBadRequest}
	}
	if fr.Method == "" || !strings.HasPrefix(fr.Path, "/") || len(fr.Body) > maxForwardBody {
		return &relaylink.Response{Status: http.StatusBadRequest}
	}
	scheme := "http"
	if be.TLS {
		scheme = "https"
	}
	// The backend request must outlive this call when the answer is streamed, so it
	// gets its own context (not the relay call's): bounded to 45s for an inline
	// answer, and owned by the streaming session otherwise.
	bctx, bcancel := context.WithCancel(context.Background())
	deadline := time.AfterFunc(45*time.Second, bcancel)
	stopWatch := context.AfterFunc(ctx, bcancel) // the relay gave up: stop the backend request
	release := func() { deadline.Stop(); stopWatch(); bcancel() }

	req, err := http.NewRequestWithContext(bctx, fr.Method, scheme+"://"+be.Addr+fr.Path, bytes.NewReader(fr.Body))
	if err != nil {
		release()
		return &relaylink.Response{Status: http.StatusBadRequest}
	}
	for k, vs := range fr.Headers {
		ck := http.CanonicalHeaderKey(k)
		if hopByHop[ck] || ck == "X-Forwarded-For" || ck == "X-Real-Ip" || ck == "Host" {
			continue // identity headers are set by the hub, never trusted from the relay's client
		}
		for _, v := range vs {
			req.Header.Add(ck, v)
		}
	}
	if host := firstValue(fr.Headers, "Host"); host != "" {
		req.Host = host
	}
	// account-proxy identifies the console by X-Forwarded-For (its realIP()).
	if net.ParseIP(fr.ClientIP) != nil {
		req.Header.Set("X-Forwarded-For", fr.ClientIP)
	}
	if f.OnWrite != nil && fr.Backend == "olv" && fr.Method != http.MethodGet && fr.Method != http.MethodHead && fr.Method != http.MethodOptions {
		defer f.OnWrite() // after the write has been attempted
	}
	client := &http.Client{
		Transport: f.transport(fr.Backend, be, fr.SNI),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // redirects belong to the console, not us
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		release()
		return &relaylink.Response{Status: http.StatusBadGateway}
	}
	out := relaylink.ForwardResponse{Status: resp.StatusCode, Close: resp.Close, Size: resp.ContentLength, Headers: map[string][]string{}}
	for k, vs := range resp.Header {
		if !hopByHop[http.CanonicalHeaderKey(k)] {
			out.Headers[k] = vs
		}
	}
	first, err := io.ReadAll(io.LimitReader(resp.Body, relaylink.ForwardInlineMax+1))
	if err != nil {
		resp.Body.Close()
		release()
		return &relaylink.Response{Status: http.StatusBadGateway}
	}
	if len(first) <= relaylink.ForwardInlineMax {
		resp.Body.Close()
		release()
		out.Body = first
		return relaylink.JSON(200, out, 0) // never cacheable
	}
	// Too large to send at once: stream the rest.
	out.Body = first[:relaylink.ForwardInlineMax]
	rest := io.MultiReader(bytes.NewReader(first[relaylink.ForwardInlineMax:]), resp.Body)
	deadline.Stop()
	stopWatch() // the streaming session, not the relay's call, now owns the backend request
	id, err := f.startFwdStream(relayID, rest, func() { resp.Body.Close(); bcancel() })
	if err != nil {
		resp.Body.Close()
		bcancel()
		return &relaylink.Response{Status: http.StatusServiceUnavailable}
	}
	out.StreamID = id
	return relaylink.JSON(200, out, 0)
}

func firstValue(h map[string][]string, key string) string {
	for k, vs := range h {
		if strings.EqualFold(k, key) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

func decodeForward(b []byte) (*relaylink.ForwardRequest, bool) {
	var fr relaylink.ForwardRequest
	if json.Unmarshal(b, &fr) != nil {
		return nil, false
	}
	return &fr, true
}
