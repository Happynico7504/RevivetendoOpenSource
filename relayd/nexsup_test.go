package relayd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Happynico7504/relayd/nexauth"
	"github.com/Happynico7504/relaylink"
)

// The supervisor tests run a REAL child process: this test binary re-executes
// itself in a fake-child mode (see TestMain) speaking the real protocol.
func TestMain(m *testing.M) {
	if os.Getenv("RELAYD_TEST_NEXCHILD") == "1" {
		nexauth.ServeChild(os.NewFile(3, "in"), os.NewFile(4, "out"), &fakeChildRunner{}, nil)
		return
	}
	os.Exit(m.Run())
}

type fakeChildRunner struct{}

func appendLog(line string) {
	f, err := os.OpenFile(os.Getenv("RELAYD_TEST_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		f.WriteString(line + "\n")
		f.Close()
	}
}

func (fakeChildRunner) Start(games []relaylink.NexGame, s *nexauth.Store, pull nexauth.PullFunc) error {
	var names []string
	for _, g := range games {
		names = append(names, g.Name)
	}
	appendLog("start:" + strings.Join(names, ","))
	if marker := os.Getenv("RELAYD_TEST_CRASH_ONCE"); marker != "" {
		if _, err := os.Stat(marker); err != nil {
			os.WriteFile(marker, nil, 0o644)
			os.Exit(3) // simulate nex-go panicking
		}
	}
	if os.Getenv("RELAYD_TEST_START_FAILS") == "1" {
		return errors.New("bind: address already in use")
	}
	if p := os.Getenv("RELAYD_TEST_PULL"); p != "" { // "game:pid"
		parts := strings.SplitN(p, ":", 2)
		pid, _ := strconv.Atoi(parts[1])
		go func() {
			pw, err := pull(parts[0], uint32(pid))
			if err != nil {
				appendLog("pulled-error:" + err.Error())
			} else {
				appendLog("pulled:" + pw)
			}
		}()
	}
	if w := os.Getenv("RELAYD_TEST_WATCH"); w != "" { // "game:pid": log the password once it is stored
		parts := strings.SplitN(w, ":", 2)
		pid, _ := strconv.Atoi(parts[1])
		go func() {
			for i := 0; i < 500; i++ {
				if pw, ok := s.Get(parts[0], uint32(pid)); ok {
					appendLog("stored:" + pw)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	return nil
}

type supRig struct {
	sup    *NexSupervisor
	log    string
	cancel context.CancelFunc
}

func newSupRig(t *testing.T, env ...string) *supRig {
	t.Helper()
	exe, _ := os.Executable()
	log := filepath.Join(t.TempDir(), "child.log")
	sup := &NexSupervisor{
		Exe: exe, Args: []string{"-test.run=NONE"}, MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
		Env: append([]string{"RELAYD_TEST_NEXCHILD=1", "RELAYD_TEST_LOG=" + log}, env...),
	}
	ctx, cancel := context.WithCancel(context.Background())
	go sup.Run(ctx)
	t.Cleanup(cancel)
	return &supRig{sup: sup, log: log, cancel: cancel}
}

func (r *supRig) lines() []string {
	b, _ := os.ReadFile(r.log)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (r *supRig) waitReady(t *testing.T) {
	t.Helper()
	waitUntil(t, "the child to become ready", func() bool { return r.sup.Ready() })
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func wscGame() relaylink.NexGame {
	g := relaylink.NexGameDefaults()["wsc"]
	g.SecureHost, g.SecurePort, g.KerberosPassword = "203.0.113.1", "60015", "secret"
	return g
}

func TestSupervisorStartsChildAndStoresCredentials(t *testing.T) {
	r := newSupRig(t, "RELAYD_TEST_WATCH=wsc:42")
	if r.sup.Ready() {
		t.Fatal("ready before any configuration")
	}
	if err := r.sup.PutCred(context.Background(), relaylink.NexCredPut{Game: "wsc", PID: 42, Password: "tok", TTLSeconds: 60}); err == nil {
		t.Fatal("PutCred succeeded with no child running")
	}
	r.sup.SetGames([]relaylink.NexGame{wscGame()})
	r.waitReady(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.sup.PutCred(ctx, relaylink.NexCredPut{Game: "wsc", PID: 42, Password: "tok", TTLSeconds: 60}); err != nil {
		t.Fatalf("PutCred: %v", err)
	}
	waitUntil(t, "the child to report the stored password", func() bool {
		for _, l := range r.lines() {
			if l == "stored:tok" {
				return true
			}
		}
		return false
	})
}

func TestSupervisorRestartsOnlyWhenTheConfigurationChanges(t *testing.T) {
	r := newSupRig(t)
	r.sup.SetGames([]relaylink.NexGame{wscGame()})
	r.waitReady(t)
	r.sup.SetGames([]relaylink.NexGame{wscGame()}) // identical: no restart
	time.Sleep(200 * time.Millisecond)
	if n := len(r.lines()); n != 1 {
		t.Fatalf("an unchanged configuration restarted the child (%d starts): %v", n, r.lines())
	}
	// The main restarted and issued a new Kerberos password.
	g := wscGame()
	g.KerberosPassword = "rotated"
	mk8 := relaylink.NexGameDefaults()["mk8"]
	mk8.SecureHost, mk8.SecurePort, mk8.KerberosPassword = "203.0.113.1", "60003", "k2"
	r.sup.SetGames([]relaylink.NexGame{g, mk8})
	waitUntil(t, "the restart with the new configuration", func() bool { return len(r.lines()) == 2 && r.sup.Ready() })
	if l := r.lines(); l[1] != "start:wsc,mk8" {
		t.Fatalf("second start: %v", l)
	}
}

func TestSupervisorRestartsACrashedChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crashed-once")
	r := newSupRig(t, "RELAYD_TEST_CRASH_ONCE="+marker)
	r.sup.SetGames([]relaylink.NexGame{wscGame()})
	waitUntil(t, "the child to come back after a crash", func() bool { return len(r.lines()) >= 2 && r.sup.Ready() })
	if err := r.sup.PutCred(context.Background(), relaylink.NexCredPut{Game: "wsc", PID: 1, Password: "p", TTLSeconds: 60}); err != nil {
		t.Fatalf("PutCred after the restart: %v", err)
	}
}

func TestSupervisorKeepsRetryingAFailingConfiguration(t *testing.T) {
	r := newSupRig(t, "RELAYD_TEST_START_FAILS=1")
	r.sup.SetGames([]relaylink.NexGame{wscGame()})
	waitUntil(t, "several retries", func() bool { return len(r.lines()) >= 3 })
	if r.sup.Ready() {
		t.Fatal("reported ready although the servers cannot start")
	}
	if err := r.sup.PutCred(context.Background(), relaylink.NexCredPut{Game: "wsc", PID: 1, Password: "p", TTLSeconds: 60}); err == nil {
		t.Fatal("PutCred succeeded without a working child")
	}
}

func TestSupervisorAnswersThePullsOfTheChild(t *testing.T) {
	var calls atomic.Int32
	r := newSupRig(t, "RELAYD_TEST_PULL=wsc:99")
	r.sup.Pull = func(_ context.Context, game string, pid uint32) (string, error) {
		calls.Add(1)
		if game != "wsc" || pid != 99 {
			return "", errors.New("wrong request")
		}
		return "pw-from-hub", nil
	}
	r.sup.SetGames([]relaylink.NexGame{wscGame()})
	waitUntil(t, "the child's pull to be answered", func() bool {
		for _, l := range r.lines() {
			if l == "pulled:pw-from-hub" {
				return true
			}
		}
		return false
	})
	// And when the hub refuses (the console was not sent to this relay):
	r2 := newSupRig(t, "RELAYD_TEST_PULL=wsc:5")
	r2.sup.Pull = func(context.Context, string, uint32) (string, error) { return "", errors.New("not assigned") }
	r2.sup.SetGames([]relaylink.NexGame{wscGame()})
	waitUntil(t, "the refusal to reach the child", func() bool {
		for _, l := range r2.lines() {
			if l == "pulled-error:not assigned" {
				return true
			}
		}
		return false
	})
}

func TestSupervisorStopsTheChildWhenTheParentContextEnds(t *testing.T) {
	r := newSupRig(t)
	r.sup.SetGames([]relaylink.NexGame{wscGame()})
	r.waitReady(t)
	r.sup.mu.Lock()
	pid := r.sup.child.cmd.Process.Pid
	done := r.sup.child.done
	r.sup.mu.Unlock()
	r.cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("child process %d outlived the supervisor", pid)
	}
}

func TestNexSupervisorReportsChangesAndLooksUpGames(t *testing.T) {
	s := &NexSupervisor{}
	var calls int
	s.OnGames = func([]relaylink.NexGame) { calls++ }
	if _, ok := s.Game("wsc"); ok {
		t.Fatal("a game before any configuration")
	}
	a := []relaylink.NexGame{{Name: "wsc", KerberosPassword: "one"}, {Name: "wsc-edge", KerberosPassword: "one"}}
	s.SetGames(a)
	s.SetGames(append([]relaylink.NexGame(nil), a...)) // identical: no callback
	if calls != 1 {
		t.Fatalf("OnGames called %d times, want 1", calls)
	}
	if g, ok := s.Game("wsc"); !ok || g.KerberosPassword != "one" {
		t.Fatalf("lookup: %+v %v", g, ok)
	}
	s.SetGames([]relaylink.NexGame{{Name: "wsc", KerberosPassword: "two"}})
	if calls != 2 {
		t.Fatalf("a rotated secret did not call OnGames (%d)", calls)
	}
	if g, _ := s.Game("wsc"); g.KerberosPassword != "two" {
		t.Fatalf("stale secret: %+v", g)
	}
	if _, ok := s.Game("wsc-edge"); ok {
		t.Fatal("a game that was dropped from the configuration is still found")
	}
}
