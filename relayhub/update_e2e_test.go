package relayhub

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Happynico7504/relayd"
	"github.com/Happynico7504/relaylink"
)

type otaRig struct {
	relDir  string
	pubKey  ed25519.PublicKey
	privKey ed25519.PrivateKey
	up      *relayd.Updater
	state   string
}

func newOTA(t *testing.T, current uint64) *otaRig {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub, sk, _ := ed25519.GenerateKey(rand.Reader)
	rpub, rpriv, _ := ed25519.GenerateKey(rand.Reader)
	r := &otaRig{relDir: t.TempDir(), pubKey: rpub, privKey: rpriv, state: t.TempDir()}
	hub := &Hub{Log: NewInvalidationLog(10), Rel: &ReleaseStore{Dir: r.relDir}}
	srv := &relaylink.Server{Priv: priv, RelayKey: func(string) (ed25519.PublicKey, bool) { return pub, true }, Replay: &relaylink.MemoryReplay{}}
	ts := httptest.NewServer(srv.RPCHandler(hub.Dispatch))
	t.Cleanup(ts.Close)
	r.up = &relayd.Updater{
		Client: &relaylink.Client{RelayID: "us-1", MainPub: &priv.PublicKey, Sign: sk, BaseURL: ts.URL},
		PubKey: rpub, Dir: r.state, Current: current, GOOS: "linux", GOARCH: "amd64",
	}
	return r
}

func prog(v string) []byte { return []byte("#!/bin/sh\necho \"relayd " + v + "\"\n") }

func (r *otaRig) publish(t *testing.T, ver uint64, bin []byte) {
	t.Helper()
	if _, err := PublishRelease(r.relDir, bin, ver, "test", "linux", "amd64", r.privKey, false); err != nil {
		t.Fatal(err)
	}
}

func TestOTAEndToEndWithRealHub(t *testing.T) {
	r := newOTA(t, 3)
	r.publish(t, 5, prog("5"))
	ok, err := r.up.CheckOnce(context.Background()) // real pre-flight: runs the script with -version
	if err != nil || !ok {
		t.Fatalf("CheckOnce: %v %v", ok, err)
	}
	got, _ := os.ReadFile(filepath.Join(r.state, "bin", "relayd"))
	if !bytes.Equal(got, prog("5")) {
		t.Fatal("installed binary differs from the published one")
	}
	// Already current afterwards.
	r.up.Current = 5
	if ok, err := r.up.CheckOnce(context.Background()); ok || err != nil {
		t.Fatalf("second check: %v %v", ok, err)
	}
}

func TestOTALargeBinaryOverRealAPI(t *testing.T) {
	r := newOTA(t, 1)
	big := make([]byte, 3*relaylink.UpdateChunkSize+777)
	rand.Read(big)
	r.publish(t, 2, big)
	r.up.Preflight = func(string, uint64) error { return nil } // random bytes are not a program
	if ok, err := r.up.CheckOnce(context.Background()); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	got, _ := os.ReadFile(filepath.Join(r.state, "bin", "relayd"))
	if !bytes.Equal(got, big) {
		t.Fatal("multi-chunk transfer corrupted the binary")
	}
}

func TestPublishRules(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := PublishRelease(dir, prog("2"), 2, "", "linux", "amd64", priv, false); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func() error{
		"same version":   func() error { _, e := PublishRelease(dir, prog("2"), 2, "", "linux", "amd64", priv, false); return e },
		"older version":  func() error { _, e := PublishRelease(dir, prog("1"), 1, "", "linux", "amd64", priv, false); return e },
		"version zero":   func() error { _, e := PublishRelease(dir, prog("0"), 0, "", "linux", "arm64", priv, false); return e },
		"empty binary":   func() error { _, e := PublishRelease(dir, nil, 9, "", "linux", "amd64", priv, false); return e },
		"path traversal": func() error { _, e := PublishRelease(dir, prog("9"), 9, "", "../etc", "amd64", priv, false); return e },
		"odd arch":       func() error { _, e := PublishRelease(dir, prog("9"), 9, "", "linux", "amd/64", priv, false); return e },
	} {
		if fn() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := PublishRelease(dir, prog("1"), 1, "", "linux", "amd64", priv, true); err != nil {
		t.Fatalf("-force rejected: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("rejected publishes left directories behind: %v", entries)
	}
}

func TestHubServesNothingHalfPublishedOrTampered(t *testing.T) {
	r := newOTA(t, 1)
	r.publish(t, 2, prog("2"))
	bin := filepath.Join(r.relDir, "linux-amd64", "relayd")

	// Someone edits the binary on the hub's disk without re-signing: the hub
	// refuses to serve a manifest for bytes that do not match it.
	os.WriteFile(bin, []byte("#!/bin/sh\necho evil\n"), 0o755)
	if ok, err := r.up.CheckOnce(context.Background()); ok || err != nil {
		t.Fatalf("tampered file served: installed=%v err=%v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(r.state, "bin", "relayd")); err == nil {
		t.Fatal("tampered binary installed")
	}

	// A compromised hub replaces BOTH files with a consistent, self-signed
	// release: the hub accepts it, but the relay's pinned key does not.
	_, evilKey, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := PublishRelease(r.relDir, prog("999"), 999, "evil", "linux", "amd64", evilKey, true); err != nil {
		t.Fatal(err)
	}
	ok, err := r.up.CheckOnce(context.Background())
	if ok || err == nil || !strings.Contains(err.Error(), "refusing release") {
		t.Fatalf("forged release accepted: installed=%v err=%v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(r.state, "bin", "relayd")); err == nil {
		t.Fatal("forged binary installed")
	}

	// Malformed manifest / wrong platform / traversal in the query: plain 404s.
	os.WriteFile(filepath.Join(r.relDir, "linux-amd64", "manifest.json"), []byte("{not json"), 0o644)
	if ok, err := r.up.CheckOnce(context.Background()); ok || err != nil {
		t.Fatalf("garbage manifest: %v %v", ok, err)
	}
	s := &ReleaseStore{Dir: r.relDir}
	for _, p := range [][2]string{{"../etc", "passwd"}, {"linux", "../../x"}, {"", ""}, {"linux", "riscv64"}} {
		if _, err := s.Manifest(p[0], p[1]); err == nil {
			t.Errorf("Manifest(%q,%q) served", p[0], p[1])
		}
	}
	if _, err := s.Chunk("linux", "amd64", 2, -1); err == nil {
		t.Error("negative offset served")
	}
}

func TestHubUpdateEndpointsValidateInput(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	PublishRelease(dir, prog("2"), 2, "", "linux", "amd64", priv, false)
	h := &Hub{Log: NewInvalidationLog(10), Rel: &ReleaseStore{Dir: dir}}
	call := func(method, path string) int {
		return h.Dispatch(context.Background(), &relaylink.Request{Method: method, Path: path}).Status
	}
	for path, want := range map[string]int{
		relaylink.UpdateManifestPath + "?os=linux&arch=amd64":                        200,
		relaylink.UpdateManifestPath + "?os=linux&arch=arm64":                        404,
		relaylink.UpdateChunkPath + "?os=linux&arch=amd64&version=2&offset=0":        200,
		relaylink.UpdateChunkPath + "?os=linux&arch=amd64&version=3&offset=0":        404, // wrong version
		relaylink.UpdateChunkPath + "?os=linux&arch=amd64&version=2&offset=99999999": 404,
		relaylink.UpdateChunkPath + "?os=linux&arch=amd64&version=x&offset=0":        400,
		relaylink.UpdateChunkPath + "?os=linux&arch=amd64&version=2&offset=-5":       404,
	} {
		if got := call("GET", path); got != want {
			t.Errorf("%s -> %d, want %d", path, got, want)
		}
	}
	if call("POST", relaylink.UpdateManifestPath+"?os=linux&arch=amd64") != 405 {
		t.Error("POST accepted")
	}
	if r := (&Hub{Log: NewInvalidationLog(10)}).Dispatch(context.Background(), &relaylink.Request{Method: "GET", Path: relaylink.UpdateManifestPath + "?os=linux&arch=amd64"}); r.Status != 404 {
		t.Errorf("OTA disabled: %d", r.Status)
	}
	// Responses are never cacheable (a stale manifest must never be reused).
	if r := h.Dispatch(context.Background(), &relaylink.Request{Method: "GET", Path: relaylink.UpdateManifestPath + "?os=linux&arch=amd64"}); r.TTL != 0 || len(r.Tags) != 0 {
		t.Errorf("manifest cacheable: ttl=%d", r.TTL)
	}
}
