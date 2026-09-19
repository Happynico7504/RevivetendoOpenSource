package nexauth

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	nex "github.com/PretendoNetwork/nex-go"

	"github.com/Happynico7504/relaylink"
)

func TestStoreExpiryAndBounds(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &Store{Now: func() time.Time { return now }}
	s.Put("wsc", 42, "pw", 10*time.Second)
	if pw, ok := s.Get("wsc", 42); !ok || pw != "pw" {
		t.Fatalf("get: %q %v", pw, ok)
	}
	if _, ok := s.Get("mk8", 42); ok {
		t.Fatal("password leaked across games")
	}
	if _, ok := s.Get("wsc", 43); ok {
		t.Fatal("password leaked across PIDs")
	}
	now = now.Add(11 * time.Second)
	if _, ok := s.Get("wsc", 42); ok {
		t.Fatal("expired password still served")
	}
	s.Put("wsc", 1, "", time.Hour) // an empty password must never authenticate anyone
	s.Put("wsc", 2, "x", 0)
	s.Put("wsc", 3, "x", -time.Second)
	if s.Len() != 0 {
		t.Fatalf("invalid entries stored: %d", s.Len())
	}
	// A new login replaces the old password (each nex_token mints a fresh one).
	s.Put("wsc", 5, "old", time.Hour)
	s.Put("wsc", 5, "new", time.Hour)
	if pw, _ := s.Get("wsc", 5); pw != "new" {
		t.Fatalf("stale password kept: %q", pw)
	}
	// The store sweeps expired entries as it grows instead of leaking.
	for i := 0; i < 5000; i++ {
		s.Put("wsc", uint32(1000+i), "x", time.Second)
	}
	now = now.Add(time.Minute)
	for i := 0; i < 10; i++ {
		s.Put("wsc", uint32(9000+i), "y", time.Hour)
	}
	if s.Len() > 100 {
		t.Fatalf("expired entries were never swept: %d", s.Len())
	}
}

func TestPasswordFromPIDLogic(t *testing.T) {
	s := &Store{}
	s.Put("wsc", 7, "local", time.Hour)
	calls := 0
	pull := func(game string, pid uint32) (string, error) {
		calls++
		if pid == 8 {
			return "from-main", nil
		}
		if pid == 9 {
			return "", errors.New("unknown")
		}
		return "", nil
	}
	f := passwordFrom("wsc", s, pull)
	if pw, code := f(7); pw != "local" || code != 0 || calls != 0 {
		t.Fatalf("local hit went to the main: %q %d calls=%d", pw, code, calls)
	}
	if pw, code := f(8); pw != "from-main" || code != 0 || calls != 1 {
		t.Fatalf("pull: %q %d", pw, code)
	}
	if pw, _ := f(8); pw != "from-main" || calls != 1 {
		t.Fatalf("a pulled password was not cached (calls=%d)", calls)
	}
	for _, pid := range []uint32{9, 10} {
		if pw, code := f(pid); pw != "" || code != nex.Errors.RendezVous.InvalidUsername {
			t.Fatalf("unknown pid %d: %q %d", pid, pw, code)
		}
	}
	if pw, code := passwordFrom("wsc", s, nil)(99); pw != "" || code != nex.Errors.RendezVous.InvalidUsername {
		t.Fatalf("no upstream: %q %d", pw, code)
	}
}

func TestValidate(t *testing.T) {
	good := relaylink.NexGameDefaults()["wsc"]
	good.SecureHost, good.SecurePort, good.KerberosPassword = "203.0.113.1", "60015", "secret"
	if err := Validate(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*relaylink.NexGame){
		"no kerberos": func(g *relaylink.NexGame) { g.KerberosPassword = "" },
		"no secure":   func(g *relaylink.NexGame) { g.SecureHost = "" },
		"no port":     func(g *relaylink.NexGame) { g.SecurePort = "" },
		"no access":   func(g *relaylink.NexGame) { g.AccessKey = "" },
		"bad port":    func(g *relaylink.NexGame) { g.Port = 70000 },
		"no name":     func(g *relaylink.NexGame) { g.Name = "" },
	} {
		g := good
		mut(&g)
		if Validate(g) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDefaultsMatchTheMainsAuthServers(t *testing.T) {
	// Values copied from wsc-/mk8-/badge-arcade-authentication/main.go: a typo
	// here would make every console fail its login on the relay.
	want := map[string]struct {
		port           int
		key, build, id string
		maj, min, pat  int
	}{
		"wsc": {60014, "4d324052", "Pretendo WSC", "1012F100", 3, 4, 0},
		// The WSC edge variant: WSC's values on its own auth port (see relaylink.EdgeGame).
		"wsc-edge":     {60114, "4d324052", "Pretendo WSC", "1012F100", 3, 4, 0},
		"mk8":          {60002, "25dbf96a", "Pretendo MK7", "1010EB00", 3, 5, 4},
		"badge-arcade": {60018, "82d5962d", "Badge Arcade Auth", "00134600", 3, 7, 16},
	}
	got := relaylink.NexGameDefaults()
	if len(got) != len(want) {
		t.Fatalf("%d games", len(got))
	}
	for name, w := range want {
		g := got[name]
		if g.Port != w.port || g.AccessKey != w.key || g.BuildName != w.build || g.GameServerID != w.id ||
			g.NEXMajor != w.maj || g.NEXMinor != w.min || g.NEXPatch != w.pat || g.Name != name {
			t.Errorf("%s: %+v", name, g)
		}
	}
}

// ---- the child's protocol ---------------------------------------------------------

type fakeRunner struct {
	mu      sync.Mutex
	games   []relaylink.NexGame
	store   *Store
	pull    PullFunc
	starts  int
	err     error
	onStart func(*fakeRunner)
}

func (f *fakeRunner) Start(games []relaylink.NexGame, s *Store, p PullFunc) error {
	f.mu.Lock()
	f.games, f.store, f.pull, f.starts = games, s, p, f.starts+1
	err, hook := f.err, f.onStart
	f.mu.Unlock()
	if hook != nil {
		hook(f)
	}
	return err
}

type childRig struct {
	toChild   io.WriteCloser
	fromChild *bufio.Scanner
	done      chan error
	run       *fakeRunner
}

func newChildRig(t *testing.T, run *fakeRunner) *childRig {
	t.Helper()
	pr, pw := io.Pipe() // parent -> child
	cr, cw := io.Pipe() // child -> parent
	r := &childRig{toChild: pw, fromChild: bufio.NewScanner(cr), done: make(chan error, 1), run: run}
	go func() { r.done <- ServeChild(pr, cw, run, nil); cw.Close() }()
	t.Cleanup(func() { pw.Close() })
	return r
}

func (r *childRig) send(t *testing.T, m Msg) {
	t.Helper()
	b, _ := json.Marshal(m)
	if _, err := r.toChild.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (r *childRig) next(t *testing.T) Msg {
	t.Helper()
	ch := make(chan Msg, 1)
	go func() {
		if r.fromChild.Scan() {
			var m Msg
			json.Unmarshal(r.fromChild.Bytes(), &m)
			ch <- m
		} else {
			ch <- Msg{T: "EOF"}
		}
	}()
	select {
	case m := <-ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no message from the child")
	}
	return Msg{}
}

func TestChildProtocolConfigCredAndPull(t *testing.T) {
	run := &fakeRunner{}
	r := newChildRig(t, run)
	games := []relaylink.NexGame{{Name: "wsc", Port: 60014}}
	r.send(t, Msg{T: TConfig, Games: games})
	if m := r.next(t); m.T != TReady || m.Err != "" {
		t.Fatalf("ready: %+v", m)
	}
	// A second configuration is ignored: the parent restarts the process to change it.
	r.send(t, Msg{T: TConfig, Games: []relaylink.NexGame{{Name: "mk8"}}})
	r.send(t, Msg{T: TCred, ID: 5, Game: "wsc", PID: 42, Password: "tok", TTL: 60})
	if m := r.next(t); m.T != TCredAck || m.ID != 5 {
		t.Fatalf("ack: %+v", m)
	}
	run.mu.Lock()
	starts, name := run.starts, run.games[0].Name
	run.mu.Unlock()
	if starts != 1 || name != "wsc" {
		t.Fatalf("configuration applied %d times (%s)", starts, name)
	}
	if pw, ok := run.store.Get("wsc", 42); !ok || pw != "tok" {
		t.Fatalf("credential not stored: %q %v", pw, ok)
	}

	// A login for an unknown console: the child asks the parent, which answers.
	got := make(chan string, 1)
	go func() { pw, err := run.pull("wsc", 77); got <- pw + "|" + errString(err) }()
	req := r.next(t)
	if req.T != TCredReq || req.Game != "wsc" || req.PID != 77 {
		t.Fatalf("credreq: %+v", req)
	}
	r.send(t, Msg{T: TCredResp, ID: req.ID, Password: "from-main"})
	if v := <-got; v != "from-main|" {
		t.Fatalf("pull result %q", v)
	}
	// An error from the main is passed to the login path.
	go func() { pw, err := run.pull("wsc", 78); got <- pw + "|" + errString(err) }()
	req = r.next(t)
	r.send(t, Msg{T: TCredResp, ID: req.ID, Err: "unknown console"})
	if v := <-got; v != "|unknown console" {
		t.Fatalf("pull error %q", v)
	}
	// If the main never answers, the console's login is not held forever.
	go func() { _, err := run.pull("wsc", 79); got <- errString(err) }()
	r.next(t) // request seen, never answered
	select {
	case v := <-got:
		if v == "" {
			t.Fatal("a pull with no answer succeeded")
		}
	case <-time.After(PullTimeout + time.Second):
		t.Fatal("pull did not time out")
	}
	// The parent going away ends the child (no orphaned auth servers).
	r.toChild.Close()
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the child kept running after its parent went away")
	}
}

func TestChildReportsStartupFailure(t *testing.T) {
	r := newChildRig(t, &fakeRunner{err: errors.New("port in use")})
	r.send(t, Msg{T: TConfig, Games: []relaylink.NexGame{{Name: "wsc"}}})
	if m := r.next(t); m.T != TReady || m.Err != "port in use" {
		t.Fatalf("%+v", m)
	}
	select {
	case err := <-r.done:
		if err == nil {
			t.Fatal("child returned success after a startup failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("child did not exit")
	}
}

func TestChildIgnoresGarbage(t *testing.T) {
	r := newChildRig(t, &fakeRunner{})
	r.toChild.Write([]byte("not json\n{\"t\":\"unknown\"}\n\n"))
	r.send(t, Msg{T: TConfig, Games: []relaylink.NexGame{{Name: "wsc"}}})
	if m := r.next(t); m.T != TReady {
		t.Fatalf("%+v", m)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
