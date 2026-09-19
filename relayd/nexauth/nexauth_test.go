package nexauth

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/PretendoNetwork/nex-protocols-common-go/authentication"
	"io"
	"strings"
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
	f := passwordFrom("wsc", s, pull, nil)
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
	if pw, code := passwordFrom("wsc", s, nil, nil)(99); pw != "" || code != nex.Errors.RendezVous.InvalidUsername {
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
		// Copied from wiiu-chat-secure (nex/authentication.go: version 3.3.2, access key e7a47214).
		"wiiu-chat": {60004, "e7a47214", "Pretendo WiiU Chat Auth", "1005A000", 3, 3, 2},
		// The Wii U Chat edge variant: the same values on its own auth port.
		"wiiu-chat-edge": {60104, "e7a47214", "Pretendo WiiU Chat Auth", "1005A000", 3, 3, 2},
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

// Every way a console's login can end in InvalidUsername (the 156-byte, ticket-less answer
// that makes the console report 106-0102) must leave a line saying why, and no line may
// ever contain a password.
func TestLookupOutcomesAreLoggedWithoutLeakingPasswords(t *testing.T) {
	s := &Store{}
	s.Put("wsc", 1, "SECRET-IN-STORE", time.Hour)
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	pull := func(game string, pid uint32) (string, error) {
		switch pid {
		case 2:
			return "SECRET-FROM-MAIN", nil
		case 3:
			return "", errors.New("unknown")
		}
		return "", errors.New("timed out waiting for the main")
	}
	f := passwordFrom("wsc", s, pull, logf)

	f(1) // a plain hit says nothing (it is the normal case)
	if len(lines) != 0 {
		t.Fatalf("a store hit logged: %v", lines)
	}
	f(2)
	if len(lines) != 1 || !strings.Contains(lines[0], "pid=2") || !strings.Contains(lines[0], "fetched from the main") {
		t.Fatalf("pull not logged: %v", lines)
	}
	f(3)
	f(4)
	if len(lines) != 3 || !strings.Contains(lines[1], "pid=3") || !strings.Contains(lines[1], "main said: unknown") ||
		!strings.Contains(lines[2], "timed out waiting for the main") {
		t.Fatalf("misses not explained: %v", lines)
	}
	passwordFrom("wsc", s, nil, logf)(5)
	if !strings.Contains(lines[len(lines)-1], "no way to ask the main") {
		t.Fatalf("no-upstream case: %v", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "SECRET") {
			t.Fatalf("a password leaked into a log line: %q", l)
		}
	}
}

func TestEngineRefusesSeveralGamesInOneProcess(t *testing.T) {
	a := relaylink.NexGameDefaults()["wsc"]
	b := relaylink.NexGameDefaults()["mk8"]
	for _, g := range []*relaylink.NexGame{&a, &b} {
		g.SecureHost, g.SecurePort, g.KerberosPassword = "203.0.113.1", "60015", "secret"
	}
	err := (&Engine{}).Start([]relaylink.NexGame{a, b}, &Store{}, nil)
	if err == nil || !strings.Contains(err.Error(), "own process") {
		t.Fatalf("two games in one process were accepted: %v", err)
	}
}

// nex-go v2 secure servers (Wii U Chat) accept a Kerberos ticket for only two minutes after it was
// issued. The upstream v1 auth library stamped every ticket with DateTime 0, so a relay's ticket was
// always "Kerberos ticket expired" there (found with a real console: the relay's login succeeded and
// the secure server then rejected the connect every two seconds). The patched copy in
// third_party/nex-protocols-common-go stamps the real time; this decrypts a ticket the way a secure
// server does and checks it.
func TestIssuedTicketsCarryTheCurrentTime(t *testing.T) {
	srv := nex.NewServer()
	srv.SetKerberosPassword("server-secret")
	auth := authentication.NewCommonAuthenticationProtocol(srv)
	auth.SetPasswordFromPIDFunction(func(pid uint32) (string, uint32) { return "user-pw", 0 })

	const userPID, serverPID = 1435853600, 2
	raw, code := authentication.TicketForTests(userPID, serverPID)
	if code != 0 || len(raw) == 0 {
		t.Fatalf("no ticket: code %d", code)
	}
	// Outer layer: encrypted for the user (what the console decrypts with its password).
	outer := nex.NewStreamIn(nex.NewKerberosEncryption(nex.DeriveKerberosKey(userPID, []byte("user-pw"))).Decrypt(raw), srv)
	outer.ReadBytesNext(int64(srv.KerberosKeySize())) // session key
	if target := outer.ReadUInt32LE(); target != serverPID {
		t.Fatalf("ticket is for pid %d, want the secure server (%d)", target, serverPID)
	}
	internal, err := outer.ReadBuffer()
	if err != nil {
		t.Fatal(err)
	}
	// Inner layer: encrypted for the secure server (what it decrypts with its Kerberos password).
	in := nex.NewStreamIn(nex.NewKerberosEncryption(nex.DeriveKerberosKey(serverPID, []byte("server-secret"))).Decrypt(internal), srv)
	issued := in.ReadDateTime().Value()
	// A DateTime packs year, month, day, hour, minute, second most-significant first, so packed
	// values order like the times they stand for.
	now := time.Now().UTC()
	dt := nex.NewDateTime(0)
	if lo, hi := dt.FromTimestamp(now.Add(-10*time.Second)), dt.FromTimestamp(now.Add(5*time.Second)); issued == 0 || issued < lo || issued > hi {
		t.Fatalf("the ticket is stamped %d, want about now (%d..%d): a nex-go v2 server allows two minutes", issued, lo, hi)
	}
}
