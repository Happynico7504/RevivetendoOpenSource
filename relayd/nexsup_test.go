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
	waitUntil(t, "wsc restarted and mk8 started", func() bool { return len(r.lines()) == 3 && r.sup.Ready() })
	count := map[string]int{}
	for _, l := range r.lines() {
		count[l]++
	}
	if count["start:wsc"] != 2 || count["start:mk8"] != 1 {
		t.Fatalf("starts: %v (each game runs alone in its own process)", r.lines())
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
	pid := r.sup.runs["wsc"].child.cmd.Process.Pid
	done := r.sup.runs["wsc"].child.done
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

func mk8Game() relaylink.NexGame {
	g := relaylink.NexGameDefaults()["mk8"]
	g.SecureHost, g.SecurePort, g.KerberosPassword = "203.0.113.1", "60003", "k2"
	return g
}

// childPID returns the pid of a game's child process (0 if it has none).
func (r *supRig) childPID(game string) int {
	r.sup.mu.Lock()
	defer r.sup.mu.Unlock()
	if run := r.sup.runs[game]; run != nil && run.child != nil {
		return run.child.cmd.Process.Pid
	}
	return 0
}

// The auth library keeps its configuration in package-level globals, so two auth servers in one
// process make every port use the LAST game's settings (this is what broke plain WSC logins when
// wsc-edge was added, and had silently affected mk8 and badge-arcade before). Each game must run
// in its own process.
func TestEachGameRunsInItsOwnProcess(t *testing.T) {
	r := newSupRig(t)
	r.sup.SetGames([]relaylink.NexGame{wscGame(), mk8Game()})
	r.waitReady(t)
	pw, pm := r.childPID("wsc"), r.childPID("mk8")
	if pw == 0 || pm == 0 || pw == pm {
		t.Fatalf("wsc pid %d, mk8 pid %d: they must be two different processes", pw, pm)
	}
	for _, l := range r.lines() {
		if strings.Contains(l, ",") {
			t.Fatalf("a child was told to host several games: %v", r.lines())
		}
	}
}

func TestChangingOneGameRestartsOnlyThatGame(t *testing.T) {
	r := newSupRig(t)
	r.sup.SetGames([]relaylink.NexGame{wscGame(), mk8Game()})
	r.waitReady(t)
	wscBefore, mk8Before := r.childPID("wsc"), r.childPID("mk8")

	g := mk8Game()
	g.KerberosPassword = "rotated" // only mk8's secret changed
	r.sup.SetGames([]relaylink.NexGame{wscGame(), g})
	waitUntil(t, "mk8 to be restarted", func() bool {
		p := r.childPID("mk8")
		return p != 0 && p != mk8Before && r.sup.Ready()
	})
	if got := r.childPID("wsc"); got != wscBefore {
		t.Fatalf("wsc was restarted (pid %d -> %d) although only mk8 changed: its credential store would have been wiped", wscBefore, got)
	}
}

func TestAddingAGameLeavesTheRunningOnesAlone(t *testing.T) {
	r := newSupRig(t)
	r.sup.SetGames([]relaylink.NexGame{wscGame()})
	r.waitReady(t)
	before := r.childPID("wsc")
	// What happens 30 s after the edge comes up: the hello grows by one game.
	edge := relaylink.EdgeGame(wscGame(), "203.0.113.5")
	r.sup.SetGames([]relaylink.NexGame{wscGame(), edge})
	waitUntil(t, "the new game", func() bool { return r.childPID(relaylink.WSCEdgeGame) != 0 && r.sup.Ready() })
	if got := r.childPID("wsc"); got != before {
		t.Fatalf("adding wsc-edge restarted wsc (pid %d -> %d)", before, got)
	}
}

func TestRemovedGameIsStoppedAndOthersKeepWorking(t *testing.T) {
	r := newSupRig(t)
	r.sup.SetGames([]relaylink.NexGame{wscGame(), mk8Game()})
	r.waitReady(t)
	r.sup.mu.Lock()
	gone := r.sup.runs["mk8"].child
	r.sup.mu.Unlock()
	r.sup.SetGames([]relaylink.NexGame{wscGame()})
	select {
	case <-gone.done:
	case <-time.After(5 * time.Second):
		t.Fatal("mk8's child kept running after mk8 was removed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.sup.PutCred(ctx, relaylink.NexCredPut{Game: "mk8", PID: 1, Password: "p", TTLSeconds: 60}); err == nil {
		t.Fatal("PutCred reached a game that is no longer hosted")
	}
	if err := r.sup.PutCred(ctx, relaylink.NexCredPut{Game: "wsc", PID: 1, Password: "p", TTLSeconds: 60}); err != nil {
		t.Fatalf("wsc stopped working when mk8 was removed: %v", err)
	}
}

func TestCredentialsGoToTheRightGamesChild(t *testing.T) {
	// Both children watch for mk8:7; only the mk8 child can ever have it.
	r := newSupRig(t, "RELAYD_TEST_WATCH=mk8:7")
	r.sup.SetGames([]relaylink.NexGame{wscGame(), mk8Game()})
	r.waitReady(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.sup.PutCred(ctx, relaylink.NexCredPut{Game: "mk8", PID: 7, Password: "tok", TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the mk8 child to store it", func() bool {
		for _, l := range r.lines() {
			if l == "stored:tok" {
				return true
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	n := 0
	for _, l := range r.lines() {
		if l == "stored:tok" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d children stored the credential, want exactly the mk8 one", n)
	}
}
