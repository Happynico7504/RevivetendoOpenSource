package relayd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Happynico7504/relaylink"
)

type fakeHub struct {
	mu       sync.Mutex
	manifest *relaylink.UpdateManifest
	bin      []byte
	none     bool                           // no release published (404)
	tamper   func(c *relaylink.UpdateChunk) // corrupt what a hostile hub sends
	calls    int32
	down     bool
}

func (h *fakeHub) dispatch(_ context.Context, req *relaylink.Request) *relaylink.Response {
	atomic.AddInt32(&h.calls, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down {
		return &relaylink.Response{Status: 503}
	}
	switch {
	case strings.HasPrefix(req.Path, relaylink.UpdateManifestPath):
		if h.none || h.manifest == nil {
			return &relaylink.Response{Status: 404}
		}
		return relaylink.JSON(200, h.manifest, 0)
	case strings.HasPrefix(req.Path, relaylink.UpdateChunkPath):
		q := req.Path[strings.Index(req.Path, "?")+1:]
		var off int64
		for _, kv := range strings.Split(q, "&") {
			if strings.HasPrefix(kv, "offset=") {
				off, _ = strconv.ParseInt(kv[7:], 10, 64)
			}
		}
		if off < 0 || off >= int64(len(h.bin)) {
			return &relaylink.Response{Status: 404}
		}
		end := off + relaylink.UpdateChunkSize
		if end > int64(len(h.bin)) {
			end = int64(len(h.bin))
		}
		c := &relaylink.UpdateChunk{Offset: off, Data: append([]byte(nil), h.bin[off:end]...), EOF: end == int64(len(h.bin))}
		if h.tamper != nil {
			h.tamper(c)
		}
		return relaylink.JSON(200, c, 0)
	}
	return &relaylink.Response{Status: 404}
}

type updRig struct {
	hub     *fakeHub
	up      *Updater
	relPub  ed25519.PublicKey
	relPriv ed25519.PrivateKey
	client  *relaylink.Client
	dir     string
	logs    []string
}

func script(version string) []byte {
	return []byte("#!/bin/sh\necho \"relayd " + version + "\"\n")
}

func (r *updRig) publish(t *testing.T, version uint64, bin []byte) {
	t.Helper()
	sum := sha256.Sum256(bin)
	m := &relaylink.UpdateManifest{Version: version, OS: "linux", Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(bin))}
	m.Sign(r.relPriv)
	r.hub.mu.Lock()
	r.hub.manifest, r.hub.bin, r.hub.none = m, bin, false
	r.hub.mu.Unlock()
}

func newUpdRig(t *testing.T, current uint64) *updRig {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub, sk, _ := ed25519.GenerateKey(rand.Reader)
	relPub, relPriv, _ := ed25519.GenerateKey(rand.Reader)
	hub := &fakeHub{}
	srv := &relaylink.Server{Priv: priv, RelayKey: func(string) (ed25519.PublicKey, bool) { return pub, true }, Replay: &relaylink.MemoryReplay{}}
	ts := httptest.NewServer(srv.RPCHandler(hub.dispatch))
	t.Cleanup(ts.Close)
	r := &updRig{hub: hub, relPub: relPub, relPriv: relPriv, dir: t.TempDir()}
	r.client = &relaylink.Client{RelayID: "us-1", MainPub: &priv.PublicKey, Sign: sk, BaseURL: ts.URL}
	r.up = &Updater{
		Client: r.client, PubKey: relPub, Dir: r.dir, Current: current, GOOS: "linux", GOARCH: "amd64",
		Logf: func(f string, a ...any) { r.logs = append(r.logs, f) },
	}
	return r
}

func (r *updRig) active() string { return filepath.Join(r.dir, "bin", "relayd") }
func (r *updRig) read(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func (r *updRig) noLeftovers(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(r.dir, "bin", "relayd.download")); err == nil {
		t.Fatal("partial download left behind")
	}
}

func TestInstallsVerifiedUpdate(t *testing.T) {
	r := newUpdRig(t, 3)
	r.publish(t, 5, script("5"))
	ok, err := r.up.CheckOnce(context.Background())
	if err != nil || !ok {
		t.Fatalf("CheckOnce: %v %v", ok, err)
	}
	if !bytes.Equal(r.read(t, r.active()), script("5")) {
		t.Fatal("wrong binary installed")
	}
	if st, _ := os.Stat(r.active()); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if s := r.up.load(); s.Pending != 5 || s.Prev != 3 || s.Boots != 0 {
		t.Fatalf("state %+v", s)
	}
	r.noLeftovers(t)
}

func TestMultiChunkDownload(t *testing.T) {
	r := newUpdRig(t, 1)
	big := make([]byte, 2*relaylink.UpdateChunkSize+12345)
	rand.Read(big)
	r.publish(t, 2, big)
	r.up.Preflight = func(string, uint64) error { return nil }
	if ok, err := r.up.CheckOnce(context.Background()); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if !bytes.Equal(r.read(t, r.active()), big) {
		t.Fatal("reassembled binary differs")
	}
}

func TestNothingToDo(t *testing.T) {
	r := newUpdRig(t, 5)
	r.hub.none = true
	if ok, err := r.up.CheckOnce(context.Background()); ok || err != nil {
		t.Fatalf("no release: %v %v", ok, err)
	}
	r.publish(t, 5, script("5")) // same version: up to date
	if ok, err := r.up.CheckOnce(context.Background()); ok || err != nil {
		t.Fatalf("same version: %v %v", ok, err)
	}
	r.publish(t, 4, script("4")) // validly signed but OLDER: downgrade attempt
	if ok, err := r.up.CheckOnce(context.Background()); ok || err != nil {
		t.Fatalf("downgrade: %v %v", ok, err)
	}
	if _, err := os.Stat(r.active()); err == nil {
		t.Fatal("something was installed")
	}
	r.hub.down = true
	if _, err := r.up.CheckOnce(context.Background()); err == nil {
		t.Fatal("dead hub reported no error")
	}
}

func TestRefusesUntrustedOrWrongReleases(t *testing.T) {
	for name, mk := range map[string]func(*updRig){
		"signed by a different key": func(r *updRig) {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			r.relPriv = other
		},
		"other architecture": func(r *updRig) { r.up.GOARCH = "arm64" },
	} {
		r := newUpdRig(t, 1)
		mk(r)
		r.publish(t, 9, script("9"))
		ok, err := r.up.CheckOnce(context.Background())
		if ok || err == nil || !strings.Contains(err.Error(), "refusing release") {
			t.Errorf("%s: installed=%v err=%v", name, ok, err)
		}
		if _, err := os.Stat(r.active()); err == nil {
			t.Errorf("%s: a binary was installed", name)
		}
		r.noLeftovers(t)
	}
}

func TestHostileHubCannotSneakInBytes(t *testing.T) {
	cases := map[string]func(c *relaylink.UpdateChunk){
		"flipped byte":    func(c *relaylink.UpdateChunk) { c.Data[0] ^= 0xff },
		"truncated + eof": func(c *relaylink.UpdateChunk) { c.Data = c.Data[:len(c.Data)/2]; c.EOF = true },
		"extra bytes":     func(c *relaylink.UpdateChunk) { c.Data = append(c.Data, "EVIL"...) },
		"wrong offset":    func(c *relaylink.UpdateChunk) { c.Offset += 7 },
		"empty chunk":     func(c *relaylink.UpdateChunk) { c.Data = nil },
	}
	for name, tamper := range cases {
		r := newUpdRig(t, 1)
		r.publish(t, 2, script("2"))
		r.hub.tamper = tamper
		if ok, err := r.up.CheckOnce(context.Background()); ok || err == nil {
			t.Errorf("%s: installed=%v err=%v", name, ok, err)
		}
		if _, err := os.Stat(r.active()); err == nil {
			t.Errorf("%s: tampered binary installed", name)
		}
		r.noLeftovers(t)
	}
}

func TestPreflightFailureKeepsWhatWorks(t *testing.T) {
	r := newUpdRig(t, 1)
	r.publish(t, 2, script("2"))
	if ok, err := r.up.CheckOnce(context.Background()); !ok || err != nil { // v2 installed
		t.Fatal(err)
	}
	// v3 is validly signed but the binary claims to be v999 (or is not runnable).
	for name, bin := range map[string][]byte{
		"wrong version reported": script("999"),
		"not executable code":    []byte("this is not a program"),
	} {
		r.up.Current = 2
		r.publish(t, 3, bin)
		if ok, err := r.up.CheckOnce(context.Background()); ok || err == nil {
			t.Errorf("%s: installed=%v err=%v", name, ok, err)
		}
		if !bytes.Equal(r.read(t, r.active()), script("2")) {
			t.Errorf("%s: the working binary was disturbed", name)
		}
		r.noLeftovers(t)
	}
}

func TestPreviousBinaryIsKeptForRollback(t *testing.T) {
	r := newUpdRig(t, 1)
	r.publish(t, 2, script("2"))
	r.up.CheckOnce(context.Background())
	r.up.Commit()
	r.up.Current = 2
	r.publish(t, 3, script("3"))
	if ok, err := r.up.CheckOnce(context.Background()); !ok || err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.read(t, r.active()), script("3")) || !bytes.Equal(r.read(t, filepath.Join(r.dir, "bin", "relayd.prev")), script("2")) {
		t.Fatal("active/prev not rotated")
	}
}

func TestRollbackAfterRepeatedFailedStarts(t *testing.T) {
	r := newUpdRig(t, 1)
	r.publish(t, 2, script("2"))
	r.up.CheckOnce(context.Background())
	r.up.Commit() // v2 is healthy
	r.up.Current = 2
	r.publish(t, 3, script("3"))
	r.up.CheckOnce(context.Background()) // v3 installed, prev = v2

	// The new version starts (as Current=3) and never becomes healthy.
	crashing := &Updater{Dir: r.dir, Current: 3, Logf: r.up.Logf}
	for i := 1; i <= MaxBoots; i++ {
		if rb, err := crashing.OnStart(); rb || err != nil {
			t.Fatalf("start %d rolled back too early (%v %v)", i, rb, err)
		}
	}
	rb, err := crashing.OnStart()
	if !rb || err != nil {
		t.Fatalf("no rollback after %d failed starts: %v %v", MaxBoots, rb, err)
	}
	if !bytes.Equal(r.read(t, r.active()), script("2")) {
		t.Fatal("previous binary not restored")
	}
	if s := r.up.load(); s.Pending != 0 || s.Rejected != 3 {
		t.Fatalf("state after rollback: %+v", s)
	}
	// The rejected version is not offered again, a newer one is.
	r.up.Current = 2
	if ok, err := r.up.CheckOnce(context.Background()); ok || err != nil {
		t.Fatalf("rejected version reinstalled: %v %v", ok, err)
	}
	r.publish(t, 4, script("4"))
	if ok, err := r.up.CheckOnce(context.Background()); !ok || err != nil {
		t.Fatalf("newer release not installed after a rollback: %v %v", ok, err)
	}
}

func TestRollbackWithoutPreviousFallsBackToInstalledBinary(t *testing.T) {
	r := newUpdRig(t, 1) // 1 = the binary installed by hand (not under bin/)
	r.publish(t, 2, script("2"))
	r.up.CheckOnce(context.Background()) // first OTA install: no bin/relayd.prev exists
	crashing := &Updater{Dir: r.dir, Current: 2}
	for i := 0; i < MaxBoots; i++ {
		crashing.OnStart()
	}
	if rb, _ := crashing.OnStart(); !rb {
		t.Fatal("no rollback")
	}
	if _, err := os.Stat(r.active()); err == nil {
		t.Fatal("the failed OTA binary is still the active one (the launcher would keep running it)")
	}
}

func TestCommitStopsTheRollbackWatch(t *testing.T) {
	r := newUpdRig(t, 1)
	r.publish(t, 2, script("2"))
	r.up.CheckOnce(context.Background())
	u2 := &Updater{Dir: r.dir, Current: 2, Logf: r.up.Logf}
	u2.OnStart()
	u2.Commit()
	for i := 0; i < 10; i++ { // many restarts later: still no rollback
		if rb, _ := u2.OnStart(); rb {
			t.Fatal("healthy version was rolled back")
		}
	}
	// A pending marker for a version that is not the one running is dropped.
	r.up.save(updateState{Pending: 9, Boots: 5})
	u3 := &Updater{Dir: r.dir, Current: 2}
	if rb, _ := u3.OnStart(); rb || u3.load().Pending != 0 {
		t.Fatal("stale pending marker not cleared")
	}
}

func TestUpdateWindow(t *testing.T) {
	at := func(h, m int) func() time.Time {
		return func() time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
	}
	for _, c := range []struct {
		window string
		h, m   int
		want   bool
	}{
		{"", 12, 0, true}, {"03:00-05:00", 4, 0, true}, {"03:00-05:00", 5, 0, false}, {"03:00-05:00", 12, 0, false},
		{"23:00-02:00", 23, 30, true}, {"23:00-02:00", 1, 0, true}, {"23:00-02:00", 3, 0, false},
		{"garbage", 12, 0, true}, {"25:00-26:00", 12, 0, true},
	} {
		u := &Updater{Window: c.window, Now: at(c.h, c.m)}
		if got := u.inWindow(); got != c.want {
			t.Errorf("window %q at %02d:%02d = %v, want %v", c.window, c.h, c.m, got, c.want)
		}
	}
	r := newUpdRig(t, 1)
	r.publish(t, 2, script("2"))
	r.up.Window, r.up.Now = "03:00-05:00", at(12, 0)
	if ok, err := r.up.CheckOnce(context.Background()); ok || err != nil || atomic.LoadInt32(&r.hub.calls) != 0 {
		t.Fatalf("outside the window: installed=%v err=%v hub calls=%d", ok, err, r.hub.calls)
	}
}

func TestRunInstallsThenExits(t *testing.T) {
	r := newUpdRig(t, 1)
	r.publish(t, 2, script("2"))
	exited := make(chan struct{}, 1)
	r.up.Exit = func() { exited <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.up.Run(ctx, time.Hour, 20*time.Millisecond)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not restart the process after installing")
	}
	// With nothing new, Run never calls Exit.
	r2 := newUpdRig(t, 5)
	r2.publish(t, 5, script("5"))
	called := int32(0)
	r2.up.Exit = func() { atomic.AddInt32(&called, 1) }
	ctx2, cancel2 := context.WithCancel(context.Background())
	go r2.up.Run(ctx2, 30*time.Millisecond, 5*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	cancel2()
	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("Exit called although already up to date")
	}
}

func TestCurrentVersionParsing(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	for in, want := range map[string]uint64{"0": 0, "12": 12, "": 0, "dev": 0, "-3": 0} {
		Version = in
		if got := CurrentVersion(); got != want {
			t.Errorf("Version %q -> %d, want %d", in, got, want)
		}
	}
	_ = errors.New
}

func TestMarkersSharedWithTheLauncher(t *testing.T) {
	r := newUpdRig(t, 1)
	r.publish(t, 2, script("2"))
	os.MkdirAll(filepath.Join(r.dir, "bin"), 0o755)
	os.WriteFile(filepath.Join(r.dir, "bin", "boots"), []byte("2\n"), 0o644) // stale counter from an earlier install
	if ok, err := r.up.CheckOnce(context.Background()); !ok || err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "bin", "pending")); strings.TrimSpace(string(b)) != "2" {
		t.Fatalf("pending marker: %q", b)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "bin", "boots")); err == nil {
		t.Fatal("stale boot counter survived a new install (it would trigger an instant rollback)")
	}
	r.up.Current = 2
	r.up.Commit()
	if _, err := os.Stat(filepath.Join(r.dir, "bin", "pending")); err == nil {
		t.Fatal("Commit left the pending marker (the launcher would roll a healthy version back)")
	}
}

func TestVersionRejectedByTheLauncherIsNeverReinstalled(t *testing.T) {
	r := newUpdRig(t, 2)
	os.MkdirAll(filepath.Join(r.dir, "bin"), 0o755)
	os.WriteFile(filepath.Join(r.dir, "bin", "rejected"), []byte("3\n"), 0o644) // written by relayd-run
	r.publish(t, 3, script("3"))
	if ok, err := r.up.CheckOnce(context.Background()); ok || err != nil {
		t.Fatalf("rolled-back version offered again: %v %v", ok, err)
	}
	r.publish(t, 4, script("4"))
	if ok, err := r.up.CheckOnce(context.Background()); !ok || err != nil {
		t.Fatalf("a newer release was not accepted: %v %v", ok, err)
	}
}
