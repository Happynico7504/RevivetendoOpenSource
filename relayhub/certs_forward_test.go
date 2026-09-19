package relayhub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Happynico7504/relaylink"
)

func writePair(t *testing.T, dir, name string, tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (certPEM, keyPEM []byte, key *ecdsa.PrivateKey) {
	t.Helper()
	key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	signer := parentKey
	if signer == nil {
		signer = key
	}
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	kd, _ := x509.MarshalECPrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
	os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
	os.WriteFile(filepath.Join(dir, name+".crt"), certPEM, 0o644)
	os.WriteFile(filepath.Join(dir, name+".key"), keyPEM, 0o600)
	return
}

func tmplFor(cn string, ca bool, dns ...string) *x509.Certificate {
	t := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: dns, BasicConstraintsValid: true, IsCA: ca,
	}
	if ca {
		t.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	} else {
		t.KeyUsage = x509.KeyUsageDigitalSignature
		t.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	return t
}

func TestCertStoreServesOnlyLeafPairs(t *testing.T) {
	dir := t.TempDir()
	caT := tmplFor("Test CA", true)
	caPEM, caKeyPEM, caKey := writePair(t, dir, "test-ca", caT, nil, nil)
	caCert, _ := x509.ParseCertificate(mustDER(t, caPEM))

	writePair(t, dir, "olv-example", tmplFor("olv", false, "olv.example.net"), caCert, caKey)
	writePair(t, dir, "3ds/olv3ds-example", tmplFor("olv3ds", false, "olv3ds.example.net"), caCert, caKey)
	writePair(t, dir, "wildcard-example", tmplFor("*.example.net", false, "*.example.net"), caCert, caKey)

	// Things that must never be served:
	os.WriteFile(filepath.Join(dir, "olv-example.csr"), []byte("csr"), 0o644)
	os.WriteFile(filepath.Join(dir, "test-ca.srl"), []byte("01"), 0o644)
	c, _, _ := writePair(t, dir, "old-backup", tmplFor("old", false, "old.example.net"), caCert, caKey)
	os.Rename(filepath.Join(dir, "old-backup.crt"), filepath.Join(dir, "olv-example.crt.bak"))
	os.Remove(filepath.Join(dir, "old-backup.key"))
	_ = c
	os.WriteFile(filepath.Join(dir, "orphan.crt"), caPEM, 0o644) // certificate with no key
	writePair(t, dir, "fullchain-x", tmplFor("x", false, "x.example.net"), caCert, caKey)
	os.Rename(filepath.Join(dir, "fullchain-x.crt"), filepath.Join(dir, "x-fullchain.crt"))
	// A cert whose key file belongs to a different key:
	writePair(t, dir, "mismatch", tmplFor("m", false, "m.example.net"), caCert, caKey)
	_, otherKey, _ := writePair(t, t.TempDir(), "other", tmplFor("o", false, "o.example.net"), caCert, caKey)
	os.WriteFile(filepath.Join(dir, "mismatch.key"), otherKey, 0o600)

	s := &CertStore{Dir: dir, Default: "olv-example"}
	m, err := s.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ci := range m.Certs {
		names = append(names, ci.Name)
	}
	want := "3ds/olv3ds-example olv-example wildcard-example"
	if strings.Join(names, " ") != want {
		t.Fatalf("manifest = %v, want [%s]", names, want)
	}
	if m.Default != "olv-example" {
		t.Fatalf("default %q", m.Default)
	}

	// The CA key must be unreachable however it is asked for.
	for _, name := range []string{"test-ca", "test-ca.key", "../test-ca", "3ds/../test-ca", "orphan", "mismatch", "x-fullchain", ""} {
		if p, ok := s.Pair(name); ok {
			t.Errorf("Pair(%q) served: %+v", name, p)
		}
	}
	for _, name := range names {
		p, ok := s.Pair(name)
		if !ok {
			t.Fatalf("Pair(%q) missing", name)
		}
		if strings.Contains(p.KeyPEM, string(caKeyPEM)) || strings.Contains(p.CertPEM, "PRIVATE") {
			t.Fatalf("Pair(%q) leaks the CA key", name)
		}
	}
	// Info carries what a relay needs to pick a cert.
	for _, ci := range m.Certs {
		if ci.Name == "wildcard-example" && (len(ci.DNSNames) != 1 || ci.DNSNames[0] != "*.example.net") {
			t.Fatalf("wildcard info: %+v", ci)
		}
	}
	// Default naming a missing cert is dropped rather than advertised.
	s2 := &CertStore{Dir: dir, Default: "nope"}
	if m2, _ := s2.Manifest(); m2.Default != "" {
		t.Fatal("unknown default advertised")
	}
}

func mustDER(t *testing.T, p []byte) []byte {
	b, _ := pem.Decode(p)
	if b == nil {
		t.Fatal("bad pem")
	}
	return b.Bytes
}

func TestCertAPIIsNeverCacheable(t *testing.T) {
	dir := t.TempDir()
	caPEM, _, caKey := writePair(t, dir, "ca", tmplFor("CA", true), nil, nil)
	caCert, _ := x509.ParseCertificate(mustDER(t, caPEM))
	writePair(t, dir, "leaf", tmplFor("l", false, "l.example.net"), caCert, caKey)
	h := &Hub{Log: NewInvalidationLog(10), Certs: &CertStore{Dir: dir}}
	for _, path := range []string{relaylink.CertManifestPath, relaylink.CertPairPrefix + "leaf"} {
		r := h.Dispatch(context.Background(), &relaylink.Request{Method: "GET", Path: path})
		if r.Status != 200 || r.TTL != 0 || len(r.Tags) != 0 {
			t.Fatalf("%s: status=%d ttl=%d tags=%v (must be uncacheable)", path, r.Status, r.TTL, r.Tags)
		}
	}
	if r := h.Dispatch(context.Background(), &relaylink.Request{Method: "GET", Path: relaylink.CertPairPrefix + "ca"}); r.Status != 404 {
		t.Fatalf("CA pair status %d", r.Status)
	}
	if r := h.Dispatch(context.Background(), &relaylink.Request{Method: "POST", Path: relaylink.CertPairPrefix + "leaf"}); r.Status != 405 {
		t.Fatalf("POST cert status %d", r.Status)
	}
	// No CertStore configured: the API simply does not exist.
	h2 := &Hub{Log: NewInvalidationLog(10)}
	if r := h2.Dispatch(context.Background(), &relaylink.Request{Method: "GET", Path: relaylink.CertManifestPath}); r.Status != 404 {
		t.Fatalf("no store: %d", r.Status)
	}
}

func fwd(t *testing.T, f *Forwarder, fr relaylink.ForwardRequest) (*relaylink.ForwardResponse, int) {
	t.Helper()
	resp := f.Do(context.Background(), "us-1", &fr)
	if resp.Status != 200 {
		return nil, resp.Status
	}
	var out relaylink.ForwardResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if resp.TTL != 0 {
		t.Fatal("forward responses must not be cacheable")
	}
	return &out, 200
}

func TestForwardReplaysRequestAgainstBackend(t *testing.T) {
	type seen struct {
		method, path, host, xff, sni, body, custom, auth string
		hopHeader                                        string
	}
	var got seen
	be := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{r.Method, r.URL.RequestURI(), r.Host, r.Header.Get("X-Forwarded-For"), r.TLS.ServerName, string(b),
			r.Header.Get("X-Custom"), r.Header.Get("Authorization"), r.Header.Get("Keep-Alive")}
		switch r.URL.Path {
		case "/redirect":
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(302)
		default:
			w.Header().Set("X-Backend", "yes")
			w.Header().Set("Connection", "close")
			w.WriteHeader(201)
			w.Write([]byte("hello"))
		}
	}))
	be.StartTLS()
	defer be.Close()
	f := &Forwarder{Backends: map[string]Backend{"olv": {Addr: strings.TrimPrefix(be.URL, "https://"), TLS: true}}}

	out, st := fwd(t, f, relaylink.ForwardRequest{
		Backend: "olv", SNI: "boss.example.net", Method: "POST", Path: "/p01/x?a=1&b=2",
		Headers: map[string][]string{
			"Host": {"boss.example.net"}, "X-Custom": {"v"}, "Authorization": {"Bearer t"},
			"X-Forwarded-For": {"6.6.6.6"}, "X-Real-Ip": {"7.7.7.7"}, // relay's console tried to spoof these
			"Keep-Alive": {"timeout=5"}, "Connection": {"close"},
		},
		Body: []byte("payload"), ClientIP: "203.0.113.77",
	})
	if st != 200 || out.Status != 201 || string(out.Body) != "hello" || out.Headers["X-Backend"][0] != "yes" {
		t.Fatalf("response: %+v (%d)", out, st)
	}
	if _, hop := out.Headers["Connection"]; hop {
		t.Fatal("hop-by-hop response header passed through")
	}
	if !out.Close {
		t.Fatal("backend's Connection: close was lost (WSC BOSS responses depend on it)")
	}
	want := seen{"POST", "/p01/x?a=1&b=2", "boss.example.net", "203.0.113.77", "boss.example.net", "payload", "v", "Bearer t", ""}
	if got != want {
		t.Fatalf("backend saw\n %+v\nwant\n %+v", got, want)
	}

	// Redirects are the console's business.
	out, _ = fwd(t, f, relaylink.ForwardRequest{Backend: "olv", Method: "GET", Path: "/redirect", ClientIP: "203.0.113.77"})
	if out.Status != 302 || out.Headers["Location"][0] != "/elsewhere" {
		t.Fatalf("redirect followed: %+v", out)
	}
}

func TestForwardRejectsBadInput(t *testing.T) {
	f := &Forwarder{Backends: map[string]Backend{"olv": {Addr: "127.0.0.1:1", TLS: true}}}
	for name, fr := range map[string]relaylink.ForwardRequest{
		"unknown backend": {Backend: "evil", Method: "GET", Path: "/"},
		"no method":       {Backend: "olv", Path: "/"},
		"absolute url":    {Backend: "olv", Method: "GET", Path: "http://internal.example/"},
		"empty path":      {Backend: "olv", Method: "GET", Path: ""},
		"huge body":       {Backend: "olv", Method: "POST", Path: "/", Body: make([]byte, maxForwardBody+1)},
	} {
		if _, st := fwd(t, f, fr); st != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, st)
		}
	}
	// Backend down: a clean 502, not a hang or a crash.
	if _, st := fwd(t, f, relaylink.ForwardRequest{Backend: "olv", Method: "GET", Path: "/"}); st != http.StatusBadGateway {
		t.Errorf("dead backend: %d", st)
	}
}

func TestHubRoutesForward(t *testing.T) {
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("plain")) }))
	defer be.Close()
	h := &Hub{Log: NewInvalidationLog(10), Fwd: &Forwarder{Backends: map[string]Backend{"hpp": {Addr: strings.TrimPrefix(be.URL, "http://")}}}}
	body, _ := json.Marshal(relaylink.ForwardRequest{Backend: "hpp", Method: "GET", Path: "/x", ClientIP: "1.2.3.4"})
	r := h.Dispatch(context.Background(), &relaylink.Request{Method: "POST", Path: relaylink.ForwardPath, Body: body})
	var out relaylink.ForwardResponse
	json.Unmarshal(r.Body, &out)
	if r.Status != 200 || string(out.Body) != "plain" {
		t.Fatalf("%d %+v", r.Status, out)
	}
	if r := h.Dispatch(context.Background(), &relaylink.Request{Method: "GET", Path: relaylink.ForwardPath}); r.Status != 405 {
		t.Fatalf("GET forward: %d", r.Status)
	}
	if r := h.Dispatch(context.Background(), &relaylink.Request{Method: "POST", Path: relaylink.ForwardPath, Body: []byte("not json")}); r.Status != 400 {
		t.Fatalf("bad json: %d", r.Status)
	}
	if r := (&Hub{Log: NewInvalidationLog(10)}).Dispatch(context.Background(), &relaylink.Request{Method: "POST", Path: relaylink.ForwardPath, Body: body}); r.Status != 405 {
		t.Fatalf("forwarding disabled: %d", r.Status)
	}
}
