package wscedge

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Happynico7504/relaylink"
)

// PipeTimeout bounds how long the edge waits for relayd (and through it the main) to
// accept an open or a call.
const PipeTimeout = 5 * time.Second

// PipeBackend is the real backend: it speaks relaylink.EdgePipeMsg lines with relayd, which
// carries them to the main over the real-time stream. Set Edge after New.
type PipeBackend struct {
	Edge *Edge
	Logf func(string, ...any)

	wmu sync.Mutex
	enc *json.Encoder

	nextID  atomic.Uint64
	pmu     sync.Mutex
	pending map[uint64]chan string
}

func (b *PipeBackend) logf(f string, a ...any) {
	if b.Logf != nil {
		b.Logf(f, a...)
	}
}

// Run reads relayd's messages from r and writes ours to w. It returns when r ends (relayd
// is gone, so the edge must exit too).
func (b *PipeBackend) Run(r io.Reader, w io.Writer) error {
	b.wmu.Lock()
	b.enc = json.NewEncoder(w)
	b.wmu.Unlock()
	b.pmu.Lock()
	if b.pending == nil {
		b.pending = map[uint64]chan string{}
	}
	b.pmu.Unlock()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		var m relaylink.EdgePipeMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			b.logf("wscedge: bad line from relayd: %v", err)
			continue
		}
		switch m.T {
		case relaylink.EdgeAck:
			b.pmu.Lock()
			ch := b.pending[m.ID]
			b.pmu.Unlock()
			if ch != nil {
				ch <- m.Err
			}
		case relaylink.EdgeOut:
			b.Edge.Out(m.PID, m.Payload)
		case relaylink.EdgeReset:
			b.Edge.Reset()
		}
	}
	return sc.Err()
}

func (b *PipeBackend) send(m relaylink.EdgePipeMsg) error {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	if b.enc == nil {
		return errors.New("pipe not running")
	}
	return b.enc.Encode(m)
}

// request sends a message that relayd answers with an ack, and waits for it.
func (b *PipeBackend) request(m relaylink.EdgePipeMsg) error {
	id := b.nextID.Add(1)
	m.ID = id
	ch := make(chan string, 1)
	b.pmu.Lock()
	b.pending[id] = ch
	b.pmu.Unlock()
	defer func() { b.pmu.Lock(); delete(b.pending, id); b.pmu.Unlock() }()
	if err := b.send(m); err != nil {
		return err
	}
	select {
	case e := <-ch:
		if e != "" {
			return errors.New(e)
		}
		return nil
	case <-time.After(PipeTimeout):
		return errors.New("timed out waiting for the main")
	}
}

func (b *PipeBackend) Open(pid uint32, ip string, port int) error {
	return b.request(relaylink.EdgePipeMsg{T: relaylink.EdgeOpen, PID: pid, IP: ip, Port: port})
}

func (b *PipeBackend) Handle(c Call) error {
	return b.request(relaylink.EdgePipeMsg{T: relaylink.EdgeRMC, PID: c.PID, Call: c.CallID, Proto: c.Protocol, Custom: c.Custom, Method: c.Method, Params: c.Params})
}

func (b *PipeBackend) Close(pid uint32) {
	b.send(relaylink.EdgePipeMsg{T: relaylink.EdgeClose, PID: pid})
}

func (b *PipeBackend) Stats(s relaylink.WSCStats) {
	b.send(relaylink.EdgePipeMsg{T: relaylink.EdgeStats, PID: s.PID, Stats: &s})
}

func (b *PipeBackend) Trace(t relaylink.WSCTrace) {
	b.send(relaylink.EdgePipeMsg{T: relaylink.EdgeTrace, PID: t.PID, Trace: &t})
}

func (b *PipeBackend) Alive(pids []uint32) {
	b.send(relaylink.EdgePipeMsg{T: relaylink.EdgeAlive, PIDs: pids})
}
