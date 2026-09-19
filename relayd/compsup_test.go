package relayd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Happynico7504/relaylink"
)

// compScript is a stand-in component binary: `-version` prints "<name> <ver>", anything
// else "runs" (sleeps) unless crash is set, in which case it exits at once.
func compScript(name, ver string, crash bool) []byte {
	run := "exec sleep 300"
	if crash {
		run = "exit 3"
	}
	return []byte("#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then echo \"" + name + " " + ver + "\"; exit 0; fi\n" + run + "\n")
}

type compRig struct {
	*updRig
	sup *ComponentSupervisor
	mu  sync.Mutex
	log []string
}

func newCompRig(t *testing.T, current uint64, fallback []byte) *compRig {
	t.Helper()
	r := &compRig{updRig: newUpdRig(t, current)}
	r.up.Component = "wscedge"
	r.up.Dir = filepath.Join(r.dir, "components", "wscedge")
	r.up.KeepRunning = true
	r.up.Logf = func(f string, a ...any) { r.record(f, a...) }
	r.sup = &ComponentSupervisor{
		Updater: r.up, Logf: r.record,
		GoodAfter: 300 * time.Millisecond, MinDelay: 10 * time.Millisecond, MaxDelay: 40 * time.Millisecond,
	}
	r.up.Exit = r.sup.Restart
	if fallback != nil {
		r.sup.Fallback = filepath.Join(r.dir, "wscedge-installed")
		if err := os.WriteFile(r.sup.Fallback, fallback, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r *compRig) record(f string, a ...any) {
	r.mu.Lock()
	r.log = append(r.log, strings.TrimSpace(fmt.Sprintf(f, a...)))
	r.mu.Unlock()
}

func (r *compRig) logged(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.log {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func (r *compRig) publishComp(t *testing.T, version uint64, bin []byte) {
	t.Helper()
	sum := sha256.Sum256(bin)
	m := &relaylink.UpdateManifest{Component: "wscedge", Version: version, OS: "linux", Arch: "amd64", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(bin))}
	m.Sign(r.relPriv)
	r.hub.mu.Lock()
	r.hub.manifest, r.hub.bin, r.hub.none = m, bin, false
	r.hub.mu.Unlock()
}

// check is one iteration of Updater.Run: look for an update and, if one was installed,
// call Exit (which restarts the child).
func (r *compRig) check(t *testing.T, ctx context.Context) {
	t.Helper()
	installed, err := r.up.CheckOnce(ctx)
	if err != nil || !installed {
		t.Fatalf("CheckOnce: installed=%v err=%v", installed, err)
	}
	r.up.Exit()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestComponentUpdateRestartsOnlyTheChild(t *testing.T) {
	r := newCompRig(t, 0, compScript("wscedge", "1", false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.sup.Run(ctx)
	waitFor(t, "fallback v1 to start", func() bool { return r.sup.Launches() == 1 && r.logged("version 1 started") })

	r.publishComp(t, 2, compScript("wscedge", "2", false))
	r.check(t, ctx)
	waitFor(t, "child restarted on v2", func() bool { return r.sup.Launches() == 2 && r.logged("version 2 started") })

	// The request carried the component; relayd's own requests never do.
	r.hub.mu.Lock()
	for _, p := range r.hub.paths {
		if !strings.Contains(p, "component=wscedge&") {
			t.Fatalf("component request without the component: %s", p)
		}
	}
	r.hub.mu.Unlock()
	if _, err := os.Stat(filepath.Join(r.up.Dir, "bin", "wscedge")); err != nil {
		t.Fatalf("binary not installed under the component's own directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "bin", "relayd")); err == nil {
		t.Fatal("touched relayd's own binary")
	}
	// Once it has stayed up long enough it is committed (rollback watch ends).
	waitFor(t, "v2 proven healthy", func() bool { return r.logged("version 2 is healthy") })
}

func TestCrashLoopingComponentIsRolledBackAndNotRetried(t *testing.T) {
	r := newCompRig(t, 0, compScript("wscedge", "1", false))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.sup.Run(ctx)
	waitFor(t, "v1", func() bool { return r.logged("version 1 started") })

	// v2 passes the pre-flight (-version works) but dies as soon as it runs.
	r.publishComp(t, 2, compScript("wscedge", "2", true))
	r.check(t, ctx)
	waitFor(t, "rollback of v2", func() bool { return r.logged("rolled back") })
	waitFor(t, "v1 running again", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		last := ""
		for _, l := range r.log {
			if strings.Contains(l, "started (pid") {
				last = l
			}
		}
		return strings.Contains(last, "version 1 started")
	})
	if _, err := os.Stat(filepath.Join(r.up.Dir, "bin", "wscedge")); err == nil {
		t.Fatal("the bad binary is still installed")
	}
	// The same bad version is never installed again.
	if installed, _ := r.up.CheckOnce(ctx); installed {
		t.Fatal("reinstalled a version that was rolled back")
	}
	// A newer one is fine.
	r.publishComp(t, 3, compScript("wscedge", "3", false))
	r.check(t, ctx)
	waitFor(t, "v3 running", func() bool { return r.logged("version 3 started") })
}

func TestComponentThatCannotReportItsVersionIsRolledBackAtOnce(t *testing.T) {
	r := newCompRig(t, 0, compScript("wscedge", "1", false))
	// Simulate an install whose binary is broken: put it in place with the state an
	// install leaves behind, bypassing the pre-flight that would normally catch it.
	bin := filepath.Join(r.up.Dir, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "wscedge"), []byte("#!/bin/sh\necho garbage\n"), 0o755)
	if err := r.up.save(updateState{Pending: 5, Prev: 1}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.sup.Run(ctx)
	waitFor(t, "rollback", func() bool { return r.logged("rolled back") })
	waitFor(t, "fallback v1 running", func() bool { return r.logged("version 1 started") })
	if r.sup.Launches() != 1 {
		t.Fatalf("the broken binary was launched (%d launches)", r.sup.Launches())
	}
}

func TestComponentWithNothingInstalledWaitsThenRunsWhenOneArrives(t *testing.T) {
	r := newCompRig(t, 0, nil) // no fallback
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.sup.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	if r.sup.Launches() != 0 {
		t.Fatal("started something with nothing installed")
	}
	r.publishComp(t, 1, compScript("wscedge", "1", false))
	r.check(t, ctx)
	waitFor(t, "first install to start", func() bool { return r.logged("version 1 started") })
}

func TestComponentUpdaterRefusesARelaydRelease(t *testing.T) {
	r := newCompRig(t, 0, nil)
	// A hub (or attacker) answering the edge's request with a validly signed RELAYD release.
	r.publish(t, 9, script("9"))
	installed, err := r.up.CheckOnce(context.Background())
	if installed || err == nil || !strings.Contains(err.Error(), "different component") {
		t.Fatalf("relayd release accepted as the edge: installed=%v err=%v", installed, err)
	}
}

func TestStoppingTheSupervisorStopsTheChild(t *testing.T) {
	r := newCompRig(t, 0, compScript("wscedge", "1", false))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.sup.Run(ctx); close(done) }()
	waitFor(t, "start", func() bool { return r.logged("version 1 started") })
	cancel()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("supervisor did not stop")
	}
	if !r.logged("stopped") {
		t.Fatal("child was not stopped")
	}
}

func TestComponentWaitsUntilReadyAndGetsFreshEnvEachLaunch(t *testing.T) {
	// A child that reports the secret it was given, then runs.
	script := []byte("#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then echo \"wscedge 1\"; exit 0; fi\necho \"SECRET=$WSC_KERBEROS_PASSWORD\"\nexec sleep 300\n")
	r := newCompRig(t, 0, script)
	var mu sync.Mutex
	secret := ""
	r.sup.Ready = func() bool { mu.Lock(); defer mu.Unlock(); return secret != "" }
	r.sup.EnvFunc = func() []string { mu.Lock(); defer mu.Unlock(); return []string{"WSC_KERBEROS_PASSWORD=" + secret} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.sup.Run(ctx)

	time.Sleep(200 * time.Millisecond)
	if r.sup.Launches() != 0 {
		t.Fatal("launched before its secret arrived")
	}
	mu.Lock()
	secret = "first"
	mu.Unlock()
	r.sup.Restart() // what OnGames does when the secret arrives
	waitFor(t, "start with the first secret", func() bool { return r.logged("SECRET=first") })

	// The main restarted and rotated the secret: the relaunch must carry the new one.
	mu.Lock()
	secret = "second"
	mu.Unlock()
	r.sup.Restart()
	waitFor(t, "relaunch with the rotated secret", func() bool { return r.logged("SECRET=second") })
	if r.sup.Launches() != 2 {
		t.Fatalf("%d launches, want 2", r.sup.Launches())
	}
}

func TestComponentReportsHealthOnlyAfterItStaysUp(t *testing.T) {
	r := newCompRig(t, 0, compScript("wscedge", "1", false))
	var mu sync.Mutex
	var events []bool
	r.sup.OnHealthy = func(h bool) { mu.Lock(); events = append(events, h); mu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.sup.Run(ctx); close(done) }()
	waitFor(t, "started", func() bool { return r.sup.Launches() == 1 })
	mu.Lock()
	early := len(events)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("reported health before GoodAfter: %v", events)
	}
	waitFor(t, "healthy", func() bool { mu.Lock(); defer mu.Unlock(); return len(events) == 1 && events[0] })
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[1] {
		t.Fatalf("events after stopping: %v (want [true false])", events)
	}
}

func TestComponentThatCrashesBeforeGoodAfterIsNeverReportedHealthy(t *testing.T) {
	r := newCompRig(t, 0, compScript("wscedge", "1", true)) // exits at once
	healthy := false
	var mu sync.Mutex
	r.sup.OnHealthy = func(h bool) { mu.Lock(); healthy = healthy || h; mu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.sup.Run(ctx)
	waitFor(t, "several crash-restarts", func() bool { return r.sup.Launches() >= 3 })
	mu.Lock()
	defer mu.Unlock()
	if healthy {
		t.Fatal("a crash-looping edge was reported healthy (the relay would offer it to consoles)")
	}
}

// Regression: relayd once exited after every component update and ran no component at all, because
// a refactor of cmd/relayd dropped `upd.Exit = sup.Restart` and `go sup.Run(ctx)`. An Updater whose
// Exit is unset ends the WHOLE process (os.Exit(0)), so the wiring has to be pinned by tests.
func TestNewComponentWiresAnUpdateToRestartOnlyTheChild(t *testing.T) {
	c := NewComponent(ComponentConfig{Name: "wscedge"}, nil, t.TempDir(), "", nil)
	if c.Updater.Exit == nil {
		t.Fatal("the updater has no Exit: after an install it would end relayd itself (os.Exit(0))")
	}
	if !c.Updater.KeepRunning {
		t.Fatal("the updater stops checking after the first install")
	}
	c.Updater.Exit() // what Updater.Run does after an install: must ask the SUPERVISOR to restart
	select {
	case <-c.Supervisor.restart:
	default:
		t.Fatal("Exit did not ask the supervisor to restart the child")
	}
	// The component lives under its own directory, never relayd's.
	if got := c.Updater.Dir; !strings.HasSuffix(got, "components/wscedge") {
		t.Fatalf("component directory %q", got)
	}
}

func TestComponentStartActuallyRunsTheChild(t *testing.T) {
	dir := t.TempDir()
	fallback := filepath.Join(dir, "child")
	if err := os.WriteFile(fallback, compScript("wscedge", "1", false), 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewComponent(ComponentConfig{Name: "wscedge", Fallback: fallback}, nil, dir, "", func(string, ...any) {})
	c.Supervisor.GoodAfter, c.Supervisor.MinDelay = 300*time.Millisecond, 10*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	waitFor(t, "Start to launch the child", func() bool { return c.Supervisor.Launches() == 1 })
}
