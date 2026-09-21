package relayd

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Happynico7504/relaylink"
)

// wiiUCiphers is account-proxy's legacy list: the Wii U / 3DS TLS stacks only
// speak these CBC suites.
var wiiUCiphers = []uint16{
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
	tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
}

const maxRequestBody = 16 << 20

// Front terminates console TLS connections locally and forwards the decrypted
// HTTP requests to the main.
type Front struct {
	Cfg    *Config
	Certs  *CertSet
	Client *relaylink.Client
	Logf   func(string, ...any)
	// Content, if set, caches console content for the hosts it covers (see ContentCache).
	Content *ContentCache

	staggerMu    sync.Mutex
	staggerNext  map[string]time.Time
	staggerHosts map[string]bool

	legacyMu  sync.Mutex
	legacyCfg map[string]*tls.Config
}

func (f *Front) logf(format string, a ...any) {
	if f.Logf != nil {
		f.Logf(format, a...)
	}
}

// stagger delays a handshake just enough (capped at one gap, per source IP) so
// two handshakes from the same console never start at the same instant: the
// 3DS/Wii U ssl module cannot survive concurrent handshakes to us. Same
// constants and behaviour as account-proxy's staggerHandshakePerIP.
func (f *Front) stagger(addr net.Addr) {
	const minGap = 300 * time.Millisecond
	const maxWait = minGap
	remote := "unknown"
	if addr != nil {
		if host, _, err := net.SplitHostPort(addr.String()); err == nil {
			remote = host
		}
	}
	f.staggerMu.Lock()
	if f.staggerNext == nil {
		f.staggerNext = map[string]time.Time{}
	}
	now := time.Now()
	for k, t := range f.staggerNext { // the original never pruned this map
		if now.Sub(t) > time.Minute {
			delete(f.staggerNext, k)
		}
	}
	next := f.staggerNext[remote]
	if next.Before(now) {
		next = now
	}
	wait := next.Sub(now)
	if wait > maxWait {
		wait = maxWait
		next = now.Add(maxWait)
	}
	f.staggerNext[remote] = next.Add(minGap)
	f.staggerMu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
}

func (f *Front) staggerFor(l Listener, chi *tls.ClientHelloInfo) {
	if chi.Conn == nil {
		return
	}
	switch l.Stagger {
	case "all":
		f.stagger(chi.Conn.RemoteAddr())
	case "sni":
		if f.staggerHosts == nil {
			f.staggerHosts = map[string]bool{}
			for _, h := range f.Cfg.StaggerHosts {
				f.staggerHosts[strings.ToLower(h)] = true
			}
		}
		empty := f.Cfg.StaggerEmptySNI == nil || *f.Cfg.StaggerEmptySNI
		if (chi.ServerName == "" && empty) || f.staggerHosts[strings.ToLower(chi.ServerName)] {
			f.stagger(chi.Conn.RemoteAddr())
		}
	}
}

// legacyFor returns the TLS config for a Wii U/3DS client. One config PER
// server name, each with its own random session-ticket key, so a ticket
// issued for one host can never resume on another (a strict client stack
// aborts when a resumed handshake carries the wrong host's certificate; see
// account-proxy's legacyTLSCfgFor for the incident behind this).
func (f *Front) legacyFor(l Listener, sni string) *tls.Config {
	key := strings.ToLower(sni)
	f.legacyMu.Lock()
	defer f.legacyMu.Unlock()
	if f.legacyCfg == nil {
		f.legacyCfg = map[string]*tls.Config{}
	}
	if c, ok := f.legacyCfg[key]; ok {
		return c
	}
	c := &tls.Config{
		MinVersion:     tls.VersionTLS10,
		MaxVersion:     tls.VersionTLS12, // suppress the TLS 1.3 downgrade sentinel their stacks reject
		CipherSuites:   wiiUCiphers,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) { return f.Certs.Select(chi.ServerName) },
	}
	var tk [32]byte
	if _, err := rand.Read(tk[:]); err == nil {
		c.SetSessionTicketKeys([][32]byte{tk})
	}
	f.legacyCfg[key] = c
	return c
}

// TLSConfig builds the tls.Config for a listener.
func (f *Front) TLSConfig(l Listener) *tls.Config {
	if l.Mode == "single" {
		min := uint16(tls.VersionTLS10)
		if l.MinTLS == "1.2" {
			min = tls.VersionTLS12
		}
		return &tls.Config{
			MinVersion:     min,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return f.Certs.Named(l.Cert) },
			GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
				f.staggerFor(l, chi)
				return nil, nil
			},
		}
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) { return f.Certs.Select(chi.ServerName) },
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			f.staggerFor(l, chi)
			for _, cs := range chi.CipherSuites { // TLS 1.3 suites: only modern clients send them
				if cs == 0x1301 || cs == 0x1302 || cs == 0x1303 {
					return nil, nil
				}
			}
			return f.legacyFor(l, chi.ServerName), nil // Wii U / 3DS
		},
	}
}

var hopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Transfer-Encoding": true,
	"Te": true, "Trailer": true, "Upgrade": true, "Content-Length": true,
}

// Handler forwards every request of a listener to the main.
func (f *Front) Handler(l Listener) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		sni := ""
		if r.TLS != nil {
			sni = r.TLS.ServerName
		}
		fr := relaylink.ForwardRequest{
			Backend: l.Backend, SNI: sni, Method: r.Method, Path: r.RequestURI,
			Headers: map[string][]string{"Host": {r.Host}}, Body: body, ClientIP: clientIP,
		}
		for k, vs := range r.Header {
			if !hopHeaders[http.CanonicalHeaderKey(k)] {
				fr.Headers[k] = vs
			}
		}
		var cacheKey string
		var cacheGen uint64
		cacheable := false
		endWrite := func() {} // runs exactly once
		if cc := f.Content; cc != nil && l.Backend == "olv" && cc.Covers(r) {
			if IsWrite(r.Method) {
				// A write: everything cached is suspect. It is flushed BEFORE the
				// console hears the answer (endWrite is called ahead of every reply
				// below), and concurrent reads are kept from re-filling the cache
				// with a pre-write copy while the write is in flight.
				cc.BeginWrite()
				var once sync.Once
				endWrite = func() { once.Do(cc.EndWrite) }
				defer endWrite() // safety net for any path that returns early
			} else {
				var hit *relaylink.ForwardResponse
				hit, cacheKey, cacheGen, cacheable = cc.Lookup(r)
				if hit != nil {
					writeForwarded(w, r, hit)
					return
				}
			}
		}
		payload, _ := json.Marshal(fr)
		ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
		defer cancel()
		started := time.Now()
		resp, err := f.Client.Call(ctx, http.MethodPost, relaylink.ForwardPath, payload)
		if err != nil || resp.Status != http.StatusOK {
			status := 0
			if resp != nil {
				status = resp.Status
			}
			f.logf("forward %s %s failed after %v from %s: %s", r.Method, r.URL.Path,
				time.Since(started).Round(time.Millisecond), clientHost(r),
				describeForwardFailure(r.Context().Err() != nil, ctx.Err(), err, status))
			endWrite()
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		var out relaylink.ForwardResponse
		if json.Unmarshal(resp.Body, &out) != nil {
			endWrite()
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		endWrite() // a write has completed: flush before the console sees the result
		if out.StreamID != "" {
			f.serveStreamed(w, r, &out)
			return
		}
		if cacheable {
			f.Content.Save(cacheKey, cacheGen, &out)
		}
		writeForwarded(w, r, &out)
	})
}

// streamWriteTimeout is how long one write of a streamed answer may block on the
// console before the connection is given up (a stalled console, not a slow one).
const streamWriteTimeout = 60 * time.Second

// streamWindow is how many chunks are fetched from the main at once.
const streamWindow = 4

// serveStreamed sends a large answer to the console as it arrives from the main:
// headers and the first part immediately, then the chunks in order, with several
// requested ahead so the ocean's round trip is not paid per chunk. If the main
// fails part-way the connection is aborted, so the console sees a failed download
// and never a silently truncated one.
func (f *Front) serveStreamed(w http.ResponseWriter, r *http.Request, out *relaylink.ForwardResponse) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	closeStream := func() {
		b, _ := json.Marshal(relaylink.ForwardChunkRequest{ID: out.StreamID})
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		f.Client.Call(cctx, http.MethodPost, relaylink.ForwardClosePath, b)
	}
	defer closeStream()

	for k, vs := range out.Headers {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if out.Close {
		w.Header().Set("Connection", "close")
	}
	if out.Size >= 0 { // the backend's own length: the console needs it to size the download
		w.Header().Set("Content-Length", itoa64(out.Size))
	}
	w.WriteHeader(out.Status)
	flusher, _ := w.(http.Flusher)
	// The server's WriteTimeout is a hard limit on the whole response, which a
	// large download (Badge Arcade's 23 MB BOSS file) on a slow console link would
	// exceed. Give each write its own deadline instead: the download may take as
	// long as it needs while the console keeps receiving.
	rc := http.NewResponseController(w)
	extend := func() { rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout)) }
	extend()
	if _, err := w.Write(out.Body); err != nil {
		return
	}
	if flusher != nil {
		flusher.Flush()
	}

	type result struct {
		index int
		chunk *relaylink.ForwardChunk
		err   error
	}
	results := make(chan result, streamWindow*2)
	issue := func(i int) {
		go func() {
			b, _ := json.Marshal(relaylink.ForwardChunkRequest{ID: out.StreamID, Index: i})
			resp, err := f.Client.Call(ctx, http.MethodPost, relaylink.ForwardChunkPath, b)
			if err == nil && resp.Status != http.StatusOK {
				err = fmt.Errorf("chunk %d: status %d", i, resp.Status)
			}
			var c relaylink.ForwardChunk
			if err == nil {
				err = json.Unmarshal(resp.Body, &c)
			}
			results <- result{index: i, chunk: &c, err: err}
		}()
	}
	next, issued := 0, 0
	for issued < streamWindow {
		issue(issued)
		issued++
	}
	pending := map[int]*relaylink.ForwardChunk{}
	var written int64 = int64(len(out.Body))
	for {
		res := <-results
		if res.err != nil {
			f.logf("streamed forward %s failed at chunk %d: %v", r.URL.Path, res.index, res.err)
			panic(http.ErrAbortHandler) // truncated: make the console see a failure
		}
		pending[res.index] = res.chunk
		for c, ok := pending[next]; ok; c, ok = pending[next] {
			delete(pending, next)
			if len(c.Data) > 0 {
				extend()
				if _, err := w.Write(c.Data); err != nil {
					return // the console went away
				}
				written += int64(len(c.Data))
				if flusher != nil {
					flusher.Flush()
				}
			}
			if c.EOF {
				if out.Size >= 0 && written != out.Size {
					f.logf("streamed forward %s ended at %d bytes, expected %d", r.URL.Path, written, out.Size)
					panic(http.ErrAbortHandler)
				}
				return
			}
			next++
			issue(issued)
			issued++
		}
	}
}

// writeForwarded replays a (forwarded or cached) response to the console.
func writeForwarded(w http.ResponseWriter, r *http.Request, out *relaylink.ForwardResponse) {
	for k, vs := range out.Headers {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	// The backend closes the connection on some endpoints (the WSC BOSS fix
	// relies on "Connection: close"); the forwarder reports that as Close.
	if out.Close {
		w.Header().Set("Connection", "close")
	}
	if r.Method == http.MethodHead {
		// No body, so report the backend's own length, and none at all if it sent
		// none (inventing "0" would tell the console the file is empty).
		if out.Size >= 0 {
			w.Header().Set("Content-Length", itoa64(out.Size))
		}
	} else {
		w.Header().Set("Content-Length", itoa(len(out.Body)))
	}
	w.WriteHeader(out.Status)
	if r.Method != http.MethodHead {
		w.Write(out.Body)
	}
}

func itoa(n int) string { return itoa64(int64(n)) }

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [21]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Serve runs one listener until it fails.
func (f *Front) Serve(l Listener) error {
	ln, err := net.Listen("tcp", l.Listen)
	if err != nil {
		return err
	}
	return f.ServeListener(l, ln)
}

func (f *Front) ServeListener(l Listener, ln net.Listener) error {
	if l.Mode == "plain" {
		srv := &http.Server{
			Handler:           f.Handler(l),
			ReadHeaderTimeout: 20 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      90 * time.Second,
			IdleTimeout:       120 * time.Second,
			ErrorLog:          nilLogger(),
		}
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	srv := &http.Server{
		Handler:           f.Handler(l),
		TLSConfig:         f.TLSConfig(l),
		ReadHeaderTimeout: 20 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          nilLogger(),
	}
	err := srv.Serve(tls.NewListener(ln, srv.TLSConfig))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func nilLogger() *log.Logger { return log.New(discard{}, "", 0) }

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// clientHost is the console's address without the port.
func clientHost(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

// describeForwardFailure says who gave up, so a log line can tell a console that hung up after
// two seconds (harmless, and common: consoles abort requests when they change state) from a
// relay that waited its full 50 seconds, or a main that answered with an error. clientGone is
// whether the console's own request was cancelled; waitErr is the relay's own wait context.
func describeForwardFailure(clientGone bool, waitErr, callErr error, status int) string {
	switch {
	case clientGone:
		return "the console hung up before the answer arrived"
	case errors.Is(waitErr, context.DeadlineExceeded):
		return "gave up waiting for the main (50s)"
	case callErr != nil:
		return "could not reach the main: " + callErr.Error()
	default:
		return fmt.Sprintf("the main answered with status %d", status)
	}
}
