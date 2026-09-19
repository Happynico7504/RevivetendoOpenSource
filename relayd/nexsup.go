package relayd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Happynico7504/relayd/nexauth"
	"github.com/Happynico7504/relaylink"
)

// NexSupervisor runs the NEX authentication servers in a child process (a copy
// of this same binary started as `relayd nexauth`, so OTA updates cover it) and
// keeps it alive: it restarts the child if it crashes, and when the main sends a
// changed configuration (its Kerberos secrets change every time the bridge
// restarts).
type NexSupervisor struct {
	Exe  string   // default: this executable
	Args []string // default: {"nexauth"}
	Env  []string // extra environment for the child (tests)
	// Pull fetches a password from the main for a console the local store does not
	// know yet (a stream cred.get). Called from the child's login path.
	Pull func(ctx context.Context, game string, pid uint32) (string, error)
	Logf func(string, ...any)
	// OnGames, if set, is called after the configuration changed (not for an identical one).
	OnGames func(games []relaylink.NexGame)

	MinBackoff time.Duration // default 1s
	MaxBackoff time.Duration // default 15s
	ReadyWait  time.Duration // default 5s
	AckWait    time.Duration // default 800ms

	mu      sync.Mutex
	games   []relaylink.NexGame
	runs    map[string]*gameRun // one supervised child process per game
	ctx     context.Context     // set by Run; runs created before it are started then
	started atomic.Bool
}

// gameRun supervises the child process of ONE game.
//
// Why one process per game: the auth library (nex-protocols-common-go) keeps its configuration in
// package-level globals, so a second NewCommonAuthenticationProtocol in the same process silently
// overwrites the first: every auth server in the process would use the LAST game's password lookup,
// secure-server address and build name. The main avoids this by running one auth process per game
// (wsc-authentication, mk8-authentication, ...), and so must a relay. A side effect worth having:
// a crash, a rotated Kerberos secret or a game coming or going only restarts that game's process,
// and only wipes that game's credential store.
type gameRun struct {
	name   string
	cfg    relaylink.NexGame // guarded by NexSupervisor.mu
	child  *nexChild         // guarded by NexSupervisor.mu
	wake   chan struct{}
	cancel context.CancelFunc // ends this run (the game was removed)
	run    atomic.Bool
}

type nexChild struct {
	cmd    *exec.Cmd
	toKid  io.WriteCloser
	wmu    sync.Mutex
	pmu    sync.Mutex
	acks   map[uint64]chan struct{}
	nextID atomic.Uint64
	ready  chan error
	done   chan struct{}
	isUp   atomic.Bool
}

func (s *NexSupervisor) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	}
}

func (g *gameRun) notify() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// SetGames installs (or changes) the configuration. An identical configuration is a no-op. For a
// different one only the games that changed are restarted, new games are started, and games no
// longer listed are stopped.
func (s *NexSupervisor) SetGames(games []relaylink.NexGame) {
	s.mu.Lock()
	same := reflect.DeepEqual(s.games, games)
	s.games = append([]relaylink.NexGame(nil), games...)
	if same {
		s.mu.Unlock()
		return
	}
	if s.runs == nil {
		s.runs = map[string]*gameRun{}
	}
	want := map[string]relaylink.NexGame{}
	for _, g := range games {
		want[g.Name] = g
	}
	var kill []*nexChild
	for name, r := range s.runs {
		g, ok := want[name]
		switch {
		case !ok: // no longer hosted
			if r.child != nil {
				kill = append(kill, r.child)
			}
			if r.cancel != nil { // nil until its loop has started
				r.cancel()
			}
			delete(s.runs, name)
		case !reflect.DeepEqual(r.cfg, g): // changed: restart just this game
			r.cfg = g
			if r.child != nil {
				kill = append(kill, r.child)
			}
			r.notify()
		}
	}
	var started []*gameRun
	for name, g := range want {
		if _, ok := s.runs[name]; !ok {
			r := &gameRun{name: name, cfg: g, wake: make(chan struct{}, 1)}
			s.runs[name] = r
			started = append(started, r)
		}
	}
	ctx := s.ctx
	s.mu.Unlock()
	for _, k := range kill {
		k.kill() // each game's loop notices and starts a fresh child with the new configuration
	}
	if ctx != nil {
		for _, r := range started {
			go s.superviseGame(ctx, r)
		}
	}
	if s.OnGames != nil {
		s.OnGames(games)
	}
}

// Game returns the current configuration of one game (ok is false until the main has sent it).
func (s *NexSupervisor) Game(name string) (relaylink.NexGame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.games {
		if g.Name == name {
			return g, true
		}
	}
	return relaylink.NexGame{}, false
}

// Ready reports whether every configured game's child is running and serving.
func (s *NexSupervisor) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runs) == 0 {
		return false
	}
	for _, r := range s.runs {
		if r.child == nil || !r.child.isUp.Load() {
			return false
		}
	}
	return true
}

// PutCred stores a password in the child and waits for its acknowledgement, so
// the caller knows the console can authenticate here before it is sent here.
func (s *NexSupervisor) PutCred(ctx context.Context, p relaylink.NexCredPut) error {
	s.mu.Lock()
	var kid *nexChild
	if r := s.runs[p.Game]; r != nil {
		kid = r.child
	}
	s.mu.Unlock()
	if kid == nil || !kid.isUp.Load() {
		return errors.New("nexauth child for " + p.Game + " is not running")
	}
	id := kid.nextID.Add(1)
	ack := make(chan struct{}, 1)
	kid.pmu.Lock()
	kid.acks[id] = ack
	kid.pmu.Unlock()
	defer func() { kid.pmu.Lock(); delete(kid.acks, id); kid.pmu.Unlock() }()
	if err := kid.send(nexauth.Msg{T: nexauth.TCred, ID: id, Game: p.Game, PID: p.PID, Password: p.Password, TTL: p.TTLSeconds}); err != nil {
		return err
	}
	wait := s.AckWait
	if wait <= 0 {
		wait = 800 * time.Millisecond
	}
	select {
	case <-ack:
		return nil
	case <-time.After(wait):
		return errors.New("nexauth child did not acknowledge in time")
	case <-kid.done:
		return errors.New("nexauth child exited")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (k *nexChild) send(m nexauth.Msg) error {
	k.wmu.Lock()
	defer k.wmu.Unlock()
	b, _ := json.Marshal(m)
	_, err := k.toKid.Write(append(b, '\n'))
	return err
}

func (k *nexChild) kill() {
	k.toKid.Close() // the child exits on EOF
	if k.cmd.Process != nil {
		k.cmd.Process.Kill()
	}
}

func (s *NexSupervisor) start(games []relaylink.NexGame) (*nexChild, error) {
	exe := s.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, err
		}
	}
	args := s.Args
	if args == nil {
		args = []string{"nexauth"}
	}
	toKidR, toKidW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromKidR, fromKidW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{toKidR, fromKidW} // descriptors 3 and 4 in the child
	cmd.Env = append(os.Environ(), s.Env...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	toKidR.Close()
	fromKidW.Close()
	k := &nexChild{cmd: cmd, toKid: toKidW, acks: map[uint64]chan struct{}{}, ready: make(chan error, 1), done: make(chan struct{})}

	go func() { // parent's reader
		sc := bufio.NewScanner(fromKidR)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var m nexauth.Msg
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				continue
			}
			switch m.T {
			case nexauth.TReady:
				var err error
				if m.Err != "" {
					err = errors.New(m.Err)
				}
				select {
				case k.ready <- err:
				default:
				}
			case nexauth.TCredAck:
				k.pmu.Lock()
				ch := k.acks[m.ID]
				k.pmu.Unlock()
				if ch != nil {
					ch <- struct{}{}
				}
			case nexauth.TCredReq:
				go s.answerPull(k, m)
			}
		}
	}()
	go func() {
		cmd.Wait()
		k.isUp.Store(false)
		fromKidR.Close()
		close(k.done)
	}()

	if err := k.send(nexauth.Msg{T: nexauth.TConfig, Games: games}); err != nil {
		k.kill()
		return nil, err
	}
	wait := s.ReadyWait
	if wait <= 0 {
		wait = 5 * time.Second
	}
	select {
	case err := <-k.ready:
		if err != nil {
			k.kill()
			return nil, err
		}
	case <-k.done:
		return nil, errors.New("nexauth child exited during startup")
	case <-time.After(wait):
		k.kill()
		return nil, errors.New("nexauth child did not become ready")
	}
	k.isUp.Store(true)
	return k, nil
}

func (s *NexSupervisor) answerPull(k *nexChild, req nexauth.Msg) {
	ctx, cancel := context.WithTimeout(context.Background(), nexauth.PullTimeout)
	defer cancel()
	resp := nexauth.Msg{T: nexauth.TCredResp, ID: req.ID}
	if s.Pull == nil {
		resp.Err = "no upstream"
	} else if pw, err := s.Pull(ctx, req.Game, req.PID); err != nil {
		resp.Err = err.Error()
	} else {
		resp.Password = pw
	}
	k.send(resp)
}

// Run supervises one child per configured game until ctx ends.
func (s *NexSupervisor) Run(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	s.mu.Lock()
	s.ctx = ctx
	var pending []*gameRun
	for _, r := range s.runs {
		pending = append(pending, r)
	}
	s.mu.Unlock()
	for _, r := range pending { // games configured before Run started
		go s.superviseGame(ctx, r)
	}
	<-ctx.Done()
	// Stopping: every child ends with its loop; give them a moment so none outlives us.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		alive := false
		for _, r := range s.runs {
			if r.child != nil {
				alive = true
			}
		}
		s.mu.Unlock()
		if !alive {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// superviseGame keeps one game's child running, restarting it with backoff if it fails or exits
// and immediately when its configuration changes, until the game is removed or ctx ends.
func (s *NexSupervisor) superviseGame(parent context.Context, r *gameRun) {
	if !r.run.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	r.cancel = func() { cancel() }
	s.mu.Unlock()
	defer cancel()

	minB, maxB := s.MinBackoff, s.MaxBackoff
	if minB <= 0 {
		minB = time.Second
	}
	if maxB <= 0 {
		maxB = 15 * time.Second
	}
	backoff := minB
	for ctx.Err() == nil {
		s.mu.Lock()
		cfg := r.cfg
		s.mu.Unlock()
		kid, err := s.start([]relaylink.NexGame{cfg})
		if err != nil {
			wait := backoff + time.Duration(rand.Int63n(int64(backoff)/2+1))
			s.logf("nexauth: %s could not start (%v); retrying in %v", r.name, err, wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			case <-r.wake:
			}
			if backoff *= 2; backoff > maxB {
				backoff = maxB
			}
			continue
		}
		s.mu.Lock()
		r.child = kid
		s.mu.Unlock()
		s.logf("nexauth: %s child running", r.name)
		started := time.Now()
		select {
		case <-kid.done:
			s.logf("nexauth: %s child exited; restarting", r.name)
		case <-ctx.Done():
			kid.kill()
			<-kid.done
			s.mu.Lock()
			r.child = nil
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		r.child = nil
		s.mu.Unlock()
		if time.Since(started) > 30*time.Second {
			backoff = minB
		} else {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > maxB {
				backoff = maxB
			}
		}
	}
}
