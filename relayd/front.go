package relayd

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
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
		payload, _ := json.Marshal(fr)
		ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
		defer cancel()
		resp, err := f.Client.Call(ctx, http.MethodPost, relaylink.ForwardPath, payload)
		if err != nil || resp.Status != http.StatusOK {
			f.logf("forward %s %s failed: %v", r.Method, r.URL.Path, err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		var out relaylink.ForwardResponse
		if json.Unmarshal(resp.Body, &out) != nil {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
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
		w.Header().Set("Content-Length", itoa(len(out.Body)))
		w.WriteHeader(out.Status)
		if r.Method != http.MethodHead {
			w.Write(out.Body)
		}
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
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
