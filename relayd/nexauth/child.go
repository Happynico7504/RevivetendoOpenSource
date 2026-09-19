package nexauth

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Happynico7504/relaylink"
)

// ---- protocol between relayd (parent) and its nexauth child ---------------------
//
// JSON lines over two dedicated pipes, inherited as file descriptors 3 (parent ->
// child) and 4 (child -> parent). Not stdin/stdout: nex-go logs to stdout.

type Msg struct {
	T        string              `json:"t"`
	ID       uint64              `json:"id,omitempty"`
	Games    []relaylink.NexGame `json:"games,omitempty"`
	Game     string              `json:"game,omitempty"`
	PID      uint32              `json:"pid,omitempty"`
	Password string              `json:"pw,omitempty"`
	TTL      int                 `json:"ttl,omitempty"`
	Err      string              `json:"err,omitempty"`
}

// message types
const (
	TConfig   = "config"   // parent -> child: the games to run (first message)
	TCred     = "cred"     // parent -> child: store a password (acknowledged)
	TCredAck  = "credack"  // child -> parent
	TReady    = "ready"    // child -> parent: servers started
	TCredReq  = "credreq"  // child -> parent: please fetch a password from the main
	TCredResp = "credresp" // parent -> child: the answer
)

// PullTimeout bounds how long a console's login waits for the main.
const PullTimeout = 1500 * time.Millisecond

// ServeChild is the child's main loop: read the parent's messages from in, write
// to out. It returns when the parent closes the pipe (the child must then exit,
// so a dead relayd never leaves an orphaned auth server running).
func ServeChild(in io.Reader, out io.Writer, run Runner, logf func(string, ...any)) error {
	var wmu sync.Mutex
	enc := json.NewEncoder(out)
	send := func(m Msg) error {
		wmu.Lock()
		defer wmu.Unlock()
		return enc.Encode(m)
	}

	store := &Store{}
	var nextID atomic.Uint64
	var pmu sync.Mutex
	pending := map[uint64]chan Msg{}

	pull := func(game string, pid uint32) (string, error) {
		id := nextID.Add(1)
		ch := make(chan Msg, 1)
		pmu.Lock()
		pending[id] = ch
		pmu.Unlock()
		defer func() { pmu.Lock(); delete(pending, id); pmu.Unlock() }()
		if err := send(Msg{T: TCredReq, ID: id, Game: game, PID: pid}); err != nil {
			return "", err
		}
		select {
		case r := <-ch:
			if r.Err != "" {
				return "", errors.New(r.Err)
			}
			return r.Password, nil
		case <-time.After(PullTimeout):
			return "", errors.New("timed out waiting for the main")
		}
	}

	started := false
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m Msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.T {
		case TConfig:
			if started {
				continue // one configuration per process; the parent restarts us to change it
			}
			started = true
			if err := run.Start(m.Games, store, pull); err != nil {
				send(Msg{T: TReady, Err: err.Error()})
				return err
			}
			send(Msg{T: TReady})
		case TCred:
			store.Put(m.Game, m.PID, m.Password, time.Duration(m.TTL)*time.Second)
			send(Msg{T: TCredAck, ID: m.ID})
		case TCredResp:
			pmu.Lock()
			ch := pending[m.ID]
			pmu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
	return sc.Err()
}

// RunChild is what `relayd nexauth` executes: it uses the inherited descriptors.
func RunChild() {
	in, out := os.NewFile(3, "from-parent"), os.NewFile(4, "to-parent")
	if in == nil || out == nil {
		os.Stderr.WriteString("nexauth: must be started by relayd (missing pipes)\n")
		os.Exit(2)
	}
	logf := func(f string, a ...any) {}
	err := ServeChild(in, out, &Engine{Logf: func(f string, a ...any) {
		os.Stderr.WriteString("nexauth: " + sprintf(f, a...) + "\n")
	}}, logf)
	if err != nil {
		os.Stderr.WriteString("nexauth: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(0) // the parent went away
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }
