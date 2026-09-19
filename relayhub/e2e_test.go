package relayhub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crypto/ed25519"
	"github.com/Happynico7504/relayd"
	"github.com/Happynico7504/relaylink"
)

func TestMain(m *testing.M) {
	// The consoles' legacy TLS needs these, exactly like the real relayd binary.
	os.Setenv("GODEBUG", "tls10server=1,tlsrsakex=1")
	os.Exit(m.Run())
}

// rsaLeaf writes an RSA leaf (legacy cipher suites need RSA) signed by ca.
func rsaLeaf(t *testing.T, dir, name, cn string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns ...string) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkixName(cn),
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: dns, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
	os.WriteFile(filepath.Join(dir, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
}

type e2e struct {
	certDir  string
	hubHits  map[string]*int32
	mu       sync.Mutex
	backends map[string]*backendLog
	relay    *relaylink.Client
	certs    *relayd.CertSet
	front    *relayd.Front
	hubURL   string
}

type backendLog struct {
	mu    sync.Mutex
	last  struct{ sni, xff, method, uri, body, host string }
	close bool
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	e := &e2e{certDir: t.TempDir(), hubHits: map[string]*int32{}, backends: map[string]*backendLog{}}

	// The main's certificate directory: a CA (whose key must never leave) and leaves.
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caT := tmplFor("Main CA", true)
	caDER, _ := x509.CreateCertificate(rand.Reader, caT, caT, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	os.WriteFile(filepath.Join(e.certDir, "the-ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644)
	kd, _ := x509.MarshalECPrivateKey(caKey)
	os.WriteFile(filepath.Join(e.certDir, "the-ca.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600)
	rsaLeaf(t, e.certDir, "olv-nicochristmann-net", "CN=olv", ca, caKey, "olv.nicochristmann.net", "portal.olv.nicochristmann.net", "ctr.olv.nicochristmann.net")
	rsaLeaf(t, e.certDir, "3ds/ctr-olv-nicochristmann-net", "CN=ctr3ds", ca, caKey, "ctr.olv.nicochristmann.net")
	rsaLeaf(t, e.certDir, "boss-nicochristmann-net", "CN=boss", ca, caKey, "boss.nicochristmann.net")
	rsaLeaf(t, e.certDir, "wildcard-nicochristmann-net", "CN=wild", ca, caKey, "*.nicochristmann.net")
	rsaLeaf(t, e.certDir, "nicochristmann-nn-cert", "CN=account", ca, caKey, "act.nicochristmann.net")

	mkBackend := func(name string, tlsOn bool) Backend {
		bl := &backendLog{}
		e.backends[name] = bl
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			bl.mu.Lock()
			sni := ""
			if r.TLS != nil {
				sni = r.TLS.ServerName
			}
			bl.last.sni, bl.last.xff, bl.last.method, bl.last.uri, bl.last.body, bl.last.host =
				sni, r.Header.Get("X-Forwarded-For"), r.Method, r.URL.RequestURI(), string(b), r.Host
			bl.mu.Unlock()
			w.Header().Set("X-From", name)
			if strings.HasPrefix(r.URL.Path, "/close") {
				w.Header().Set("Connection", "close")
			}
			w.WriteHeader(200)
			w.Write([]byte("backend:" + name))
		})
		var srv *httptest.Server
		if tlsOn {
			srv = httptest.NewUnstartedServer(h)
			srv.StartTLS()
		} else {
			srv = httptest.NewServer(h)
		}
		t.Cleanup(srv.Close)
		return Backend{Addr: strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://"), TLS: tlsOn}
	}
	backends := map[string]Backend{"olv": mkBackend("olv", true), "account": mkBackend("account", true), "hpp": mkBackend("hpp", false)}

	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub, sk, _ := ed25519.GenerateKey(rand.Reader)
	hub := &Hub{
		Log:   NewInvalidationLog(10),
		Certs: &CertStore{Dir: e.certDir, Default: "olv-nicochristmann-net", Routes: DefaultCertRoutes(), TTL: time.Millisecond},
		Fwd:   &Forwarder{Backends: backends},
	}
	srv := &relaylink.Server{Priv: priv, RelayKey: func(string) (ed25519.PublicKey, bool) { return pub, true }, Replay: &relaylink.MemoryReplay{}}
	ts := httptest.NewServer(srv.RPCHandler(func(ctx context.Context, req *relaylink.Request) *relaylink.Response {
		c := e.hubHits[req.Path]
		if c == nil {
			e.mu.Lock()
			c = new(int32)
			e.hubHits[req.Path] = c
			e.mu.Unlock()
		}
		atomic.AddInt32(c, 1)
		return hub.Dispatch(ctx, req)
	}))
	t.Cleanup(ts.Close)
	e.hubURL = ts.URL
	e.relay = &relaylink.Client{RelayID: "us-1", MainPub: &priv.PublicKey, Sign: sk, BaseURL: ts.URL}

	e.certs = &relayd.CertSet{Client: e.relay, Dir: t.TempDir()}
	if err := e.certs.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.front = &relayd.Front{Cfg: &relayd.Config{StaggerHosts: relayd.DefaultStaggerHosts}, Certs: e.certs, Client: e.relay}
	return e
}

func (e *e2e) hits(path string) int {
	e.mu.Lock()
	c := e.hubHits[path]
	e.mu.Unlock()
	if c == nil {
		return 0
	}
	return int(atomic.LoadInt32(c))
}

func (e *e2e) serve(t *testing.T, l relayd.Listener) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go e.front.ServeListener(l, ln)
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

// servedCN does a TLS handshake for an SNI and reports which certificate came back.
func servedCN(t *testing.T, addr, sni string, cfg *tls.Config) string {
	t.Helper()
	c := cfg.Clone()
	c.ServerName, c.InsecureSkipVerify = sni, true
	conn, err := tls.Dial("tcp", addr, c)
	if err != nil {
		t.Fatalf("handshake sni=%q: %v", sni, err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Subject.CommonName
}

func TestRelaySelectsSameCertificatesAsTheMain(t *testing.T) {
	e := newE2E(t)
	addr := e.serve(t, relayd.Listener{Listen: "x", Backend: "olv", Mode: "sni"})
	modern := &tls.Config{}
	// Exactly the cases account-proxy's getCert() handles, including the one that
	// broke real Wii Us in 2026-08 (an explicit name must beat a wildcard).
	for sni, want := range map[string]string{
		"olv.nicochristmann.net":        "CN=olv",
		"portal.olv.nicochristmann.net": "CN=olv",
		"ctr.olv.nicochristmann.net":    "CN=ctr3ds", // both certs list this name: the route table decides
		"boss.nicochristmann.net":       "CN=boss",
		"anything.nicochristmann.net":   "CN=wild",
		"":                              "CN=olv", // empty SNI (real 3DS) -> default
		"totally.unknown.example.org":   "CN=olv",
	} {
		if got := servedCN(t, addr, sni, modern); got != want {
			t.Errorf("sni %q: served %q, want %q", sni, got, want)
		}
	}
}

func TestLegacyClientsGetLegacyTLS(t *testing.T) {
	e := newE2E(t)
	addr := e.serve(t, relayd.Listener{Listen: "x", Backend: "olv", Mode: "sni"})
	// A Wii U-style ClientHello: TLS 1.0-1.2, CBC suites only.
	wiiu := &tls.Config{
		MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS12,
		CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_128_CBC_SHA, tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA},
	}
	for _, sni := range []string{"olv.nicochristmann.net", "portal.olv.nicochristmann.net"} {
		c := wiiu.Clone()
		c.ServerName, c.InsecureSkipVerify = sni, true
		conn, err := tls.Dial("tcp", addr, c)
		if err != nil {
			t.Fatalf("legacy handshake for %s: %v", sni, err)
		}
		cs := conn.ConnectionState()
		conn.Close()
		if cs.Version == tls.VersionTLS13 {
			t.Fatalf("legacy client was negotiated to TLS 1.3")
		}
	}
	// TLS 1.0 outright (Wii U's oldest stack).
	tls10 := wiiu.Clone()
	tls10.MaxVersion = tls.VersionTLS10
	if got := servedCN(t, addr, "olv.nicochristmann.net", tls10); got != "CN=olv" {
		t.Fatalf("TLS 1.0 handshake served %q", got)
	}
}

func consoleClient(addr, sni string, cfg *tls.Config) *http.Client {
	c := cfg.Clone()
	c.ServerName, c.InsecureSkipVerify = sni, true
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: c, DisableKeepAlives: true,
			DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, n, addr)
			}},
		Timeout: 10 * time.Second,
	}
}

func TestRequestsAreForwardedToTheMainsBackend(t *testing.T) {
	e := newE2E(t)
	addr := e.serve(t, relayd.Listener{Listen: "x", Backend: "olv", Mode: "sni"})
	cl := consoleClient(addr, "boss.nicochristmann.net", &tls.Config{})
	req, _ := http.NewRequest("POST", "https://boss.nicochristmann.net/p01/tasksheet/1/x?c=US&l=en", strings.NewReader("body!"))
	req.Header.Set("X-Custom", "v")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "backend:olv" || resp.Header.Get("X-From") != "olv" {
		t.Fatalf("response: %d %q %v", resp.StatusCode, b, resp.Header)
	}
	bl := e.backends["olv"]
	bl.mu.Lock()
	defer bl.mu.Unlock()
	got := bl.last
	if got.method != "POST" || got.uri != "/p01/tasksheet/1/x?c=US&l=en" || got.body != "body!" ||
		got.host != "boss.nicochristmann.net" || got.sni != "boss.nicochristmann.net" || got.xff != "127.0.0.1" {
		t.Fatalf("backend saw %+v", got)
	}
}

func TestConnectionCloseSurvivesTheRelay(t *testing.T) {
	e := newE2E(t)
	addr := e.serve(t, relayd.Listener{Listen: "x", Backend: "olv", Mode: "sni"})
	cl := consoleClient(addr, "boss.nicochristmann.net", &tls.Config{})
	resp, err := cl.Get("https://boss.nicochristmann.net/close/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !resp.Close {
		t.Fatal("backend's Connection: close was not passed to the console (WSC BOSS fix)")
	}
	resp, _ = cl.Get("https://boss.nicochristmann.net/normal")
	resp.Body.Close()
}

func TestSingleCertListenersAndBackends(t *testing.T) {
	e := newE2E(t)
	acct := e.serve(t, relayd.Listener{Listen: "x", Backend: "account", Mode: "single", Cert: "nicochristmann-nn-cert", MinTLS: "1.0"})
	// Whatever SNI the console sends (or none), the account listener has one cert.
	for _, sni := range []string{"act.nicochristmann.net", "", "whatever.example"} {
		if got := servedCN(t, acct, sni, &tls.Config{}); got != "CN=account" {
			t.Errorf("account listener sni %q served %q", sni, got)
		}
	}
	tls10 := &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS10, CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_128_CBC_SHA}}
	if got := servedCN(t, acct, "", tls10); got != "CN=account" {
		t.Fatalf("TLS 1.0 on the account listener: %q", got)
	}
	resp, err := consoleClient(acct, "act.nicochristmann.net", &tls.Config{}).Get("https://act.nicochristmann.net/v1/api/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "backend:account" {
		t.Fatalf("account backend: %q", b)
	}

	// The HPP listener forwards to a plain-HTTP backend.
	hpp := e.serve(t, relayd.Listener{Listen: "x", Backend: "hpp", Mode: "single", Cert: "wildcard-nicochristmann-net"})
	resp, err = consoleClient(hpp, "hpp.example", &tls.Config{}).Get("https://hpp.example/hpp/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "backend:hpp" {
		t.Fatalf("hpp backend: %q", b)
	}
}

func TestHandshakeStagger(t *testing.T) {
	e := newE2E(t)
	all := e.serve(t, relayd.Listener{Listen: "x", Backend: "account", Mode: "single", Cert: "nicochristmann-nn-cert", Stagger: "all"})
	start := time.Now()
	servedCN(t, all, "", &tls.Config{})
	first := time.Since(start)
	start = time.Now()
	servedCN(t, all, "", &tls.Config{}) // same source IP right after: must be delayed
	second := time.Since(start)
	if second < 200*time.Millisecond || first > 200*time.Millisecond {
		t.Fatalf("stagger: first=%v second=%v (want ~0 then ~300ms)", first, second)
	}

	// sni mode only staggers the 3DS-sensitive hosts; others are never delayed.
	sni := e.serve(t, relayd.Listener{Listen: "x", Backend: "olv", Mode: "sni", Stagger: "sni"})
	servedCN(t, sni, "boss.nicochristmann.net", &tls.Config{})
	start = time.Now()
	servedCN(t, sni, "boss.nicochristmann.net", &tls.Config{})
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("non-sensitive host was staggered (%v)", d)
	}
}

func TestCertSyncBehaviour(t *testing.T) {
	e := newE2E(t)
	// Everything but the CA arrived, on disk, private.
	if e.certs.Count() != 5 {
		t.Fatalf("synced %d certs, want 5 (the CA must not be one of them)", e.certs.Count())
	}
	dir := e.certs
	_ = dir
	if _, err := e.certs.Named("the-ca"); err == nil {
		t.Fatal("the CA was synced")
	}
	// A second sync with nothing changed must not re-download any private key.
	before := e.hits(relaylink.CertPairPrefix + "boss-nicochristmann-net")
	if err := e.certs.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := e.hits(relaylink.CertPairPrefix + "boss-nicochristmann-net"); after != before {
		t.Fatalf("unchanged certificate downloaded again (%d -> %d)", before, after)
	}
	if e.hits(relaylink.CertManifestPath) < 2 {
		t.Fatal("manifest not re-checked")
	}
}

func TestCertsSurviveRestartAndUpdates(t *testing.T) {
	e := newE2E(t)
	diskDir := t.TempDir()
	set := &relayd.CertSet{Client: e.relay, Dir: diskDir}
	if err := set.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Keys are on disk with owner-only permissions.
	keyPath := filepath.Join(diskDir, "boss-nicochristmann-net.key")
	st, err := os.Stat(keyPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(diskDir, "3ds", "ctr-olv-nicochristmann-net.key")); err != nil {
		t.Fatalf("nested cert not written: %v", err)
	}
	// "Restart" with the main unreachable: certificates load from disk.
	dead := &relaylink.Client{RelayID: "us-1", MainPub: e.relay.MainPub, Sign: e.relay.Sign, BaseURL: "http://127.0.0.1:1"}
	set2 := &relayd.CertSet{Client: dead, Dir: diskDir}
	if err := set2.LoadFromDisk(); err != nil || set2.Count() != 5 {
		t.Fatalf("load from disk: %v (%d certs)", err, set2.Count())
	}
	if c, err := set2.Select("boss.nicochristmann.net"); err != nil || c == nil {
		t.Fatalf("select after restart: %v", err)
	}
	// A failed sync keeps what we have.
	if err := set2.Sync(context.Background()); err == nil {
		t.Fatal("sync against a dead main succeeded")
	}
	if set2.Count() != 5 {
		t.Fatal("failed sync dropped certificates")
	}
	// Certificate replaced on the main -> picked up; removed -> deleted locally.
	rsaLeaf(t, e.certDir, "boss-nicochristmann-net", "CN=boss-v2", mustCA(t, e.certDir), mustCAKey(t, e.certDir), "boss.nicochristmann.net")
	os.Remove(filepath.Join(e.certDir, "wildcard-nicochristmann-net.crt"))
	os.Remove(filepath.Join(e.certDir, "wildcard-nicochristmann-net.key"))
	time.Sleep(5 * time.Millisecond) // CertStore TTL in this rig is 1ms
	if err := set.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, _ := set.Select("boss.nicochristmann.net")
	leaf, _ := x509.ParseCertificate(c.Certificate[0])
	if leaf.Subject.CommonName != "CN=boss-v2" {
		t.Fatalf("updated certificate not picked up: %q", leaf.Subject.CommonName)
	}
	if _, err := os.Stat(filepath.Join(diskDir, "wildcard-nicochristmann-net.key")); err == nil {
		t.Fatal("removed certificate's key is still on disk")
	}
	if set.Count() != 4 {
		t.Fatalf("count %d", set.Count())
	}
}

func mustCA(t *testing.T, dir string) *x509.Certificate {
	b, _ := os.ReadFile(filepath.Join(dir, "the-ca.crt"))
	blk, _ := pem.Decode(b)
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustCAKey(t *testing.T, dir string) *ecdsa.PrivateKey {
	b, _ := os.ReadFile(filepath.Join(dir, "the-ca.key"))
	blk, _ := pem.Decode(b)
	k, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pkixName(cn string) pkix.Name { return pkix.Name{CommonName: cn} }
