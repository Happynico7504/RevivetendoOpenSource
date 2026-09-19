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

	MinBackoff time.Duration // default 1s
	MaxBackoff time.Duration // default 15s
	ReadyWait  time.Duration // default 5s
	AckWait    time.Duration // default 800ms

	mu      sync.Mutex
	games   []relaylink.NexGame
	child   *nexChild
	wake    chan struct{}
	started atomic.Bool
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

func (s *NexSupervisor) notify() {
	s.mu.Lock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	w := s.wake
	s.mu.Unlock()
	select {
	case w <- struct{}{}:
	default:
	}
}

// SetGames installs (or changes) the configuration. An identical configuration
// is a no-op; a different one restarts the child.
func (s *NexSupervisor) SetGames(games []relaylink.NexGame) {
	s.mu.Lock()
	same := reflect.DeepEqual(s.games, games)
	s.games = append([]relaylink.NexGame(nil), games...)
	kid := s.child
	s.mu.Unlock()
	if same {
		return
	}
	if kid != nil {
		kid.kill() // Run notices, and starts a fresh child with the new configuration
	}
	s.notify()
}

// Ready reports whether the child is running and serving.
func (s *NexSupervisor) Ready() bool {
	s.mu.Lock()
	kid := s.child
	s.mu.Unlock()
	return kid != nil && kid.isUp.Load()
}

// PutCred stores a password in the child and waits for its acknowledgement, so
// the caller knows the console can authenticate here before it is sent here.
func (s *NexSupervisor) PutCred(ctx context.Context, p relaylink.NexCredPut) error {
	s.mu.Lock()
	kid := s.child
	s.mu.Unlock()
	if kid == nil || !kid.isUp.Load() {
		return errors.New("nexauth child is not running")
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

// Run keeps a child running for the current configuration until ctx ends.
func (s *NexSupervisor) Run(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	minB, maxB := s.MinBackoff, s.MaxBackoff
	if minB <= 0 {
		minB = time.Second
	}
	if maxB <= 0 {
		maxB = 15 * time.Second
	}
	backoff := minB
	s.notify() // in case games were set before Run started
	for ctx.Err() == nil {
		s.mu.Lock()
		games := append([]relaylink.NexGame(nil), s.games...)
		wake := s.wake
		if wake == nil {
			s.wake = make(chan struct{}, 1)
			wake = s.wake
		}
		s.mu.Unlock()
		if len(games) == 0 { // nothing to host yet: wait for the main's configuration
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			continue
		}
		kid, err := s.start(games)
		if err != nil {
			wait := backoff + time.Duration(rand.Int63n(int64(backoff)/2+1))
			s.logf("nexauth: could not start (%v); retrying in %v", err, wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			case <-wake:
			}
			if backoff *= 2; backoff > maxB {
				backoff = maxB
			}
			continue
		}
		s.mu.Lock()
		s.child = kid
		s.mu.Unlock()
		s.logf("nexauth: child running (%d games)", len(games))
		started := time.Now()
		select {
		case <-kid.done:
			s.logf("nexauth: child exited; restarting")
		case <-ctx.Done():
			kid.kill()
			<-kid.done
			return
		}
		s.mu.Lock()
		s.child = nil
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
