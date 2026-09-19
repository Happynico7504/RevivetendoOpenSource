package relayd

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Happynico7504/relaylink"
)

// Version is this build's release number, stamped at build time:
//
//	go build -ldflags "-X github.com/Happynico7504/relayd.Version=12" ./cmd/relayd
//
// An unstamped (development) build is version 0 and will accept any release.
var Version = "0"

func CurrentVersion() uint64 {
	v, _ := strconv.ParseUint(Version, 10, 64)
	return v
}

// MaxBoots is how many times a freshly installed version may start without
// proving itself healthy (see Commit) before it is rolled back.
const MaxBoots = 3

// updateState is persisted in <dir>/update.json.
type updateState struct {
	Pending  uint64 `json:"pending,omitempty"`  // installed but not yet proven healthy
	Prev     uint64 `json:"prev,omitempty"`     // version that was running before the install
	Boots    int    `json:"boots,omitempty"`    // starts since the install
	Rejected uint64 `json:"rejected,omitempty"` // highest version that was rolled back: never retry it
}

// Updater performs over-the-air updates of relayd itself.
//
// Layout under Dir (the service's writable state directory):
//
//	bin/relayd           the active OTA-installed binary (the launcher prefers it)
//	bin/relayd.prev      the previous one, kept for rollback
//	bin/relayd.download  a download in progress (never executed unverified)
//	update.json          state (see updateState)
//
// An update is only ever installed if its manifest is signed by the pinned
// release key, is for this OS/arch, is strictly newer than both the running
// version and any previously rolled-back version, and the downloaded bytes
// match the signed size and SHA-256. It is then executed once with -version as
// a pre-flight check before it replaces anything.
type Updater struct {
	// Component is what this updater keeps current. Empty means the relay daemon itself.
	// Any other component (say "wscedge") is a separately shipped binary: give it its
	// own Dir, so its files, state and rollback markers never mix with relayd's.
	Component string
	Client    *relaylink.Client
	mu        sync.Mutex // guards Current when a supervisor changes it while checks run
	PubKey    ed25519.PublicKey
	Dir       string
	Current   uint64
	GOOS      string // default runtime.GOOS
	GOARCH    string // default runtime.GOARCH
	Window    string // "HH:MM-HH:MM" local time in which updates may be applied; "" = any time
	Exit      func() // called after a successful install so the supervisor restarts us (default os.Exit(0))
	// KeepRunning makes Run continue checking after an install instead of returning:
	// for a component whose Exit restarts a child process rather than ending this one.
	KeepRunning bool
	Logf        func(string, ...any)
	Now         func() time.Time
	// Preflight runs the downloaded binary before installing it. Default: run
	// `<path> -version` and require "<component> <version>".
	Preflight func(path string, want uint64) error
}

// Name is the component name ("relayd" for the daemon itself): the binary's file name
// and the first word its -version output must print.
func (u *Updater) Name() string {
	if u.Component == "" {
		return relaylink.ComponentRelayd
	}
	return u.Component
}

// query is the component part of an update request. The daemon sends none, so requests
// from relays and hubs of any age still match.
func (u *Updater) query() string {
	if u.Name() == relaylink.ComponentRelayd {
		return ""
	}
	return "component=" + u.Name() + "&"
}

// SetCurrent records the version of the binary that is now running (a supervisor calls
// this each time it launches the component).
func (u *Updater) SetCurrent(v uint64) {
	u.mu.Lock()
	u.Current = v
	u.mu.Unlock()
}

func (u *Updater) cur() uint64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.Current
}

func (u *Updater) logf(f string, a ...any) {
	if u.Logf != nil {
		u.Logf(f, a...)
	}
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) platform() (string, string) {
	goos, goarch := u.GOOS, u.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

func (u *Updater) binDir() string { return filepath.Join(u.Dir, "bin") }

// Marker files shared with the launcher script (deploy/relayd-run), which is the
// AUTHORITY for rolling back: it works even when a new binary crashes before any
// of relayd's own code runs.
//
//	bin/pending    version installed but not yet proven healthy (written here)
//	bin/boots      launches since the install (counted by the launcher)
//	bin/rejected   highest version the launcher rolled back (never reinstalled)
func (u *Updater) markerPath(name string) string { return filepath.Join(u.binDir(), name) }

func (u *Updater) rejectedFloor(s updateState) uint64 {
	floor := s.Rejected
	if raw, err := os.ReadFile(u.markerPath("rejected")); err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil && v > floor {
			floor = v
		}
	}
	return floor
}
func (u *Updater) statePath() string { return filepath.Join(u.Dir, "update.json") }

func (u *Updater) load() updateState {
	var s updateState
	if raw, err := os.ReadFile(u.statePath()); err == nil {
		json.Unmarshal(raw, &s)
	}
	return s
}

func (u *Updater) save(s updateState) error {
	raw, _ := json.Marshal(s)
	if err := os.MkdirAll(u.Dir, 0o700); err != nil {
		return err
	}
	tmp := u.statePath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, u.statePath())
}

// OnStart must run first thing at process start. If a freshly installed
// version keeps failing to prove itself, it rolls back to the previous binary
// and reports true: the caller must then exit so the supervisor restarts into
// the restored version.
func (u *Updater) OnStart() (rolledBack bool, err error) {
	s := u.load()
	if s.Pending == 0 {
		return false, nil
	}
	if s.Pending != u.cur() {
		// The running binary is not the pending one (e.g. the launcher fell back
		// to the system binary): nothing to prove or roll back.
		s.Pending, s.Boots = 0, 0
		return false, u.save(s)
	}
	s.Boots++
	if s.Boots <= MaxBoots {
		return false, u.save(s)
	}
	return true, u.rollback(s, "failed to become healthy in %d starts", MaxBoots)
}

// rollback restores the previous binary (or removes the OTA one so the fallback runs),
// and remembers the bad version so it is never installed again.
func (u *Updater) rollback(s updateState, why string, args ...any) error {
	active, prev := filepath.Join(u.binDir(), u.Name()), filepath.Join(u.binDir(), u.Name()+".prev")
	var err error
	if _, serr := os.Stat(prev); serr == nil {
		err = os.Rename(prev, active)
	} else {
		err = os.Remove(active) // no previous OTA binary: fall back to the installed one
	}
	u.logf("%s version %d "+why+": rolled back", append([]any{u.Name(), s.Pending}, args...)...)
	if s.Pending > s.Rejected {
		s.Rejected = s.Pending
	}
	s.Pending, s.Boots, s.Prev = 0, 0, 0
	if serr := u.save(s); err == nil {
		err = serr
	}
	return err
}

// ForceRollback rolls back a pending (not yet proven) install right now, for a binary
// that cannot even report its version. It reports whether there was anything to roll back.
func (u *Updater) ForceRollback(reason string) (bool, error) {
	s := u.load()
	if s.Pending == 0 {
		return false, nil
	}
	return true, u.rollback(s, "%s", reason)
}

// Commit marks the running version healthy (call after it has served for a
// while). It clears the rollback watch.
func (u *Updater) Commit() {
	os.Remove(u.markerPath("pending"))
	os.Remove(u.markerPath("boots"))
	s := u.load()
	if s.Pending == 0 {
		return
	}
	if s.Pending == u.cur() {
		u.logf("version %d is healthy", u.cur())
	}
	s.Pending, s.Boots = 0, 0
	u.save(s)
}

func (u *Updater) inWindow() bool {
	if u.Window == "" {
		return true
	}
	parts := strings.SplitN(u.Window, "-", 2)
	if len(parts) != 2 {
		return true
	}
	parse := func(s string) (int, bool) {
		var h, m int
		if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, false
		}
		return h*60 + m, true
	}
	from, ok1 := parse(parts[0])
	to, ok2 := parse(parts[1])
	if !ok1 || !ok2 {
		return true
	}
	n := u.now()
	cur := n.Hour()*60 + n.Minute()
	if from <= to {
		return cur >= from && cur < to
	}
	return cur >= from || cur < to // window crossing midnight
}

func (u *Updater) defaultPreflight(path string, want uint64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		return fmt.Errorf("pre-flight run failed: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != u.Name()+" "+strconv.FormatUint(want, 10) {
		return fmt.Errorf("pre-flight: binary reports %q, expected version %d", got, want)
	}
	return nil
}

// CheckOnce looks for a newer release and installs it if there is one.
func (u *Updater) CheckOnce(ctx context.Context) (installed bool, err error) {
	if !u.inWindow() {
		return false, nil
	}
	goos, goarch := u.platform()
	resp, err := u.Client.Call(ctx, http.MethodGet, fmt.Sprintf("%s?%sos=%s&arch=%s", relaylink.UpdateManifestPath, u.query(), goos, goarch), nil)
	if err != nil {
		return false, err
	}
	if resp.Status == http.StatusNotFound {
		return false, nil // nothing published for this platform
	}
	if resp.Status != http.StatusOK {
		return false, fmt.Errorf("update manifest: status %d", resp.Status)
	}
	var m relaylink.UpdateManifest
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return false, err
	}
	state := u.load()
	floor := u.cur()
	if r := u.rejectedFloor(state); r > floor {
		floor = r
	}
	if err := m.VerifyComponent(u.PubKey, u.Name(), goos, goarch, floor); err != nil {
		if errors.Is(err, relaylink.ErrUpdateNotNewer) {
			return false, nil // up to date
		}
		return false, fmt.Errorf("refusing release: %w", err)
	}

	u.logf("%s update %d available (running %d, %d bytes): downloading", u.Name(), m.Version, u.cur(), m.Size)
	if err := os.MkdirAll(u.binDir(), 0o755); err != nil {
		return false, err
	}
	tmp := filepath.Join(u.binDir(), u.Name()+".download")
	os.Remove(tmp)
	if err := u.download(ctx, &m, tmp); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("download: %w", err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return false, err
	}
	pre := u.Preflight
	if pre == nil {
		pre = u.defaultPreflight
	}
	if err := pre(tmp, m.Version); err != nil {
		os.Remove(tmp)
		return false, err
	}

	active, prev := filepath.Join(u.binDir(), u.Name()), filepath.Join(u.binDir(), u.Name()+".prev")
	if _, err := os.Stat(active); err == nil {
		if err := os.Rename(active, prev); err != nil {
			os.Remove(tmp)
			return false, err
		}
	}
	if err := os.Rename(tmp, active); err != nil {
		os.Rename(prev, active) // put the old one back
		return false, err
	}
	state.Pending, state.Prev, state.Boots = m.Version, u.cur(), 0
	if err := u.save(state); err != nil {
		return false, err
	}
	os.Remove(u.markerPath("boots"))
	if err := os.WriteFile(u.markerPath("pending"), []byte(strconv.FormatUint(m.Version, 10)+"\n"), 0o644); err != nil {
		return false, err
	}
	u.logf("%s update %d installed: restarting", u.Name(), m.Version)
	return true, nil
}

func (u *Updater) download(ctx context.Context, m *relaylink.UpdateManifest, dest string) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	goos, goarch := u.platform()
	h := sha256.New()
	var off int64
	for {
		path := fmt.Sprintf("%s?%sos=%s&arch=%s&version=%d&offset=%d", relaylink.UpdateChunkPath, u.query(), goos, goarch, m.Version, off)
		resp, err := u.Client.Call(ctx, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if resp.Status != http.StatusOK {
			return fmt.Errorf("chunk at %d: status %d", off, resp.Status)
		}
		var c relaylink.UpdateChunk
		if err := json.Unmarshal(resp.Body, &c); err != nil {
			return err
		}
		if c.Offset != off || len(c.Data) == 0 {
			return fmt.Errorf("unexpected chunk (offset %d, want %d)", c.Offset, off)
		}
		if off+int64(len(c.Data)) > m.Size {
			return errors.New("hub sent more data than the manifest allows")
		}
		if _, err := f.Write(c.Data); err != nil {
			return err
		}
		h.Write(c.Data)
		off += int64(len(c.Data))
		if c.EOF || off == m.Size {
			break
		}
	}
	if off != m.Size {
		return fmt.Errorf("download ended at %d bytes, manifest says %d", off, m.Size)
	}
	if hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return errors.New("downloaded binary does not match the signed hash")
	}
	return f.Sync()
}

// Run checks periodically (with jitter, so a fleet does not update in lockstep)
// until ctx is cancelled. After an install it calls Exit.
func (u *Updater) Run(ctx context.Context, every time.Duration, firstDelay time.Duration) {
	exit := u.Exit
	if exit == nil {
		exit = func() { os.Exit(0) }
	}
	wait := firstDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		installed, err := u.CheckOnce(ctx)
		if err != nil {
			u.logf("update check failed: %v", err)
		}
		if installed {
			exit()
			if !u.KeepRunning {
				return
			}
		}
		wait = every + time.Duration(rand.Int63n(int64(every)/5+1)) // +0..20%
	}
}
