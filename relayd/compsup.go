package relayd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ComponentSupervisor runs a separately shipped component (for example the WSC edge) as
// a child process, keeps it current through its own Updater, and restarts only that child
// when an update lands: the relay and its other services stay up.
//
// Rollback is this supervisor's job (relayd's own is done by the launcher script): every
// launch of a freshly installed version counts as a boot, and a version that does not stay
// up for GoodAfter within MaxBoots launches is replaced by the previous binary and never
// installed again. A binary that cannot even print its version is rolled back at once.
type ComponentSupervisor struct {
	Updater  *Updater
	Fallback string   // binary to run when nothing has been installed over the air; "" = wait for one
	Args     []string // the child's arguments
	Env      []string // extra environment ("KEY=value")
	Logf     func(string, ...any)
	// Pipe, if set, is called on every launch with a line channel to the child: it reads the
	// child's messages from fromChild and writes to toChild. The child sees them as inherited
	// descriptors 3 (reads) and 4 (writes). Pipe blocks until the child's end closes.
	Pipe func(fromChild io.Reader, toChild io.Writer)

	GoodAfter time.Duration // uptime that proves a version healthy (default 30s)
	MinDelay  time.Duration // restart backoff start (default 1s)
	MaxDelay  time.Duration // restart backoff cap (default 30s)

	mu      sync.Mutex
	running *exec.Cmd
	restart chan struct{}
	started int // launches so far (for tests and status)
}

func (s *ComponentSupervisor) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	}
}

func (s *ComponentSupervisor) defaults() {
	if s.GoodAfter <= 0 {
		s.GoodAfter = 30 * time.Second
	}
	if s.MinDelay <= 0 {
		s.MinDelay = time.Second
	}
	if s.MaxDelay <= 0 {
		s.MaxDelay = 30 * time.Second
	}
	s.mu.Lock()
	if s.restart == nil {
		s.restart = make(chan struct{}, 1)
	}
	s.mu.Unlock()
}

// Restart asks the run loop to relaunch the child (the Updater calls this after an
// install). Requests coalesce: several before the loop reacts cause one restart.
func (s *ComponentSupervisor) Restart() {
	s.defaults()
	select {
	case s.restart <- struct{}{}:
	default:
	}
}

// Launches reports how many times the child has been started.
func (s *ComponentSupervisor) Launches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *ComponentSupervisor) active() string {
	return filepath.Join(s.Updater.binDir(), s.Updater.Name())
}

// pick chooses what to run: the over-the-air binary if there is one, else the fallback.
func (s *ComponentSupervisor) pick() string {
	if _, err := os.Stat(s.active()); err == nil {
		return s.active()
	}
	if s.Fallback != "" {
		if _, err := os.Stat(s.Fallback); err == nil {
			return s.Fallback
		}
	}
	return ""
}

// binaryVersion runs `<path> -version` and parses "<name> <n>".
func (s *ComponentSupervisor) binaryVersion(path string) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[0] != s.Updater.Name() {
		return 0, fmt.Errorf("unexpected -version output %q", strings.TrimSpace(string(out)))
	}
	return strconv.ParseUint(fields[1], 10, 64)
}

// Run supervises until ctx is cancelled.
func (s *ComponentSupervisor) Run(ctx context.Context) {
	s.defaults()
	delay := s.MinDelay
	for ctx.Err() == nil {
		path := s.pick()
		if path == "" {
			// Nothing installed yet: the updater will fetch it and call Restart.
			select {
			case <-ctx.Done():
				return
			case <-s.restart:
			case <-time.After(5 * time.Second):
			}
			continue
		}
		ver, err := s.binaryVersion(path)
		if err != nil {
			s.logf("%s: %s cannot report its version: %v", s.Updater.Name(), path, err)
			if path == s.active() {
				if rolled, rerr := s.Updater.ForceRollback("cannot report its version"); rolled {
					if rerr != nil {
						s.logf("%s: rollback: %v", s.Updater.Name(), rerr)
					}
					continue // pick again: the previous binary or the fallback
				}
			}
			s.wait(ctx, &delay)
			continue
		}
		s.Updater.SetCurrent(ver)
		if rolled, err := s.Updater.OnStart(); err != nil {
			s.logf("%s: update state: %v", s.Updater.Name(), err)
		} else if rolled {
			continue // the failing version is gone: pick again
		}

		ranFor := s.runChild(ctx, path, ver)
		if ctx.Err() != nil {
			return
		}
		if ranFor >= s.GoodAfter {
			delay = s.MinDelay
		}
		select {
		case <-s.restart:
			delay = s.MinDelay // asked to restart: no penalty
			continue
		default:
		}
		s.wait(ctx, &delay)
	}
}

// wait sleeps the current backoff (or less, if a restart is requested) and doubles it.
func (s *ComponentSupervisor) wait(ctx context.Context, delay *time.Duration) {
	select {
	case <-ctx.Done():
	case <-s.restart:
	case <-time.After(*delay):
	}
	if *delay *= 2; *delay > s.MaxDelay {
		*delay = s.MaxDelay
	}
}

// runChild starts the binary and blocks until it exits, a restart is requested or ctx is
// cancelled. It returns how long the child ran.
func (s *ComponentSupervisor) runChild(ctx context.Context, path string, ver uint64) time.Duration {
	name := s.Updater.Name()
	runDir := filepath.Join(s.Updater.Dir, "run") // the child's cwd: nex-go writes log/ there
	os.MkdirAll(runDir, 0o755)
	cmd := exec.Command(path, s.Args...)
	cmd.Dir = runDir
	cmd.Env = append(os.Environ(), s.Env...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	var toChild, fromChild *os.File // the parent's ends of the message pipes
	var childEnds []*os.File
	if s.Pipe != nil {
		cr, pwr, err := os.Pipe() // parent -> child (descriptor 3)
		if err != nil {
			s.logf("%s: pipe: %v", name, err)
			return 0
		}
		prd, cw, err := os.Pipe() // child -> parent (descriptor 4)
		if err != nil {
			cr.Close()
			pwr.Close()
			s.logf("%s: pipe: %v", name, err)
			return 0
		}
		toChild, fromChild = pwr, prd
		childEnds = []*os.File{cr, cw}
		cmd.ExtraFiles = childEnds
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} // never outlive relayd
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				s.logf("%s: %s", name, line)
			}
		}
	}()

	start := time.Now()
	closePipes := func() {
		for _, f := range []*os.File{toChild, fromChild} {
			if f != nil {
				f.Close()
			}
		}
	}
	if err := cmd.Start(); err != nil {
		pw.Close()
		for _, f := range childEnds {
			f.Close()
		}
		closePipes()
		s.logf("%s: start failed: %v", name, err)
		return 0
	}
	for _, f := range childEnds {
		f.Close() // the child has its own copies now
	}
	if s.Pipe != nil {
		go s.Pipe(fromChild, toChild)
	}
	s.mu.Lock()
	s.running = cmd
	s.started++
	s.mu.Unlock()
	s.logf("%s: version %d started (pid %d)", name, ver, cmd.Process.Pid)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); pw.Close(); closePipes() }()
	proven := time.NewTimer(s.GoodAfter)
	defer proven.Stop()

	stop := func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
	for {
		select {
		case err := <-done:
			s.mu.Lock()
			s.running = nil
			s.mu.Unlock()
			s.logf("%s: exited after %v: %v", name, time.Since(start).Round(time.Millisecond), err)
			return time.Since(start)
		case <-proven.C:
			s.Updater.Commit()
		case <-s.restart:
			s.logf("%s: restarting", name)
			stop()
			s.mu.Lock()
			s.running = nil
			s.mu.Unlock()
			// leave a token so Run does not add a backoff for a requested restart
			select {
			case s.restart <- struct{}{}:
			default:
			}
			return time.Since(start)
		case <-ctx.Done():
			stop()
			s.mu.Lock()
			s.running = nil
			s.mu.Unlock()
			s.logf("%s: stopped", name)
			return time.Since(start)
		}
	}
}
