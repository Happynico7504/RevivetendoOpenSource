package relayhub

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/Happynico7504/relaylink"
)

// A streamed forward: the hub keeps the backend's response open and the relay
// pulls it in chunks by index (several at once), so an answer of any size flows
// through with bounded memory on both sides.
//
// Flow control is pull-driven: the hub reads ahead from the backend only a few
// chunks beyond what the relay has asked for, and drops chunks the relay has
// moved well past. A session that is idle for fwdIdleTimeout is cancelled (which
// also closes the backend connection); at most maxFwdSessions run at once.
const (
	fwdReadAhead   = 4
	fwdKeepBehind  = 8
	fwdIdleTimeout = 60 * time.Second
	maxFwdSessions = 32
)

var (
	errFwdGone    = errors.New("stream session unknown or expired")
	errFwdTooMany = errors.New("too many streamed forwards")
)

type fwdSession struct {
	id     string
	owner  string // the relay that opened it: no one else may read it
	cancel context.CancelFunc

	mu        sync.Mutex
	cond      *sync.Cond
	chunks    map[int][]byte
	produced  int // chunks read from the backend so far
	requested int // highest index the relay has asked for
	eof       bool
	err       error
	last      time.Time
	closed    bool
}

func newFwdID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// startFwdStream registers a session over rest (the not-yet-sent remainder of the
// backend body) and starts reading it. cancel closes the backend request.
func (f *Forwarder) startFwdStream(owner string, rest io.Reader, cancel context.CancelFunc) (string, error) {
	f.smu.Lock()
	if f.sessions == nil {
		f.sessions = map[string]*fwdSession{}
		go f.fwdJanitor()
	}
	if len(f.sessions) >= maxFwdSessions {
		f.smu.Unlock()
		return "", errFwdTooMany
	}
	s := &fwdSession{id: newFwdID(), owner: owner, cancel: cancel, chunks: map[int][]byte{}, requested: -1, last: time.Now()}
	s.cond = sync.NewCond(&s.mu)
	f.sessions[s.id] = s
	f.smu.Unlock()
	go s.produce(rest)
	return s.id, nil
}

func (s *fwdSession) produce(rest io.Reader) {
	br := bufio.NewReaderSize(rest, 64<<10)
	for {
		s.mu.Lock()
		for !s.closed && s.produced > s.requested+fwdReadAhead {
			s.cond.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		buf := make([]byte, relaylink.ForwardChunkSize)
		n, err := io.ReadFull(br, buf)
		eof := false
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			err, eof = nil, true
		} else if err == nil {
			if _, perr := br.Peek(1); perr == io.EOF {
				eof = true // the body ended exactly on a chunk boundary
			}
		}
		s.mu.Lock()
		if err != nil {
			s.err = err
		} else {
			s.chunks[s.produced] = buf[:n]
			s.produced++
			s.eof = eof
		}
		done := s.err != nil || s.eof
		s.cond.Broadcast()
		s.mu.Unlock()
		if done {
			return
		}
	}
}

func (s *fwdSession) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
	s.cancel()
}

// Chunk returns chunk `index` of a session, waiting for it if the backend has not
// produced it yet. A request beyond the end returns an empty final chunk, so a
// relay may fetch a few indices ahead without knowing where the body ends.
func (f *Forwarder) Chunk(ctx context.Context, relayID string, req relaylink.ForwardChunkRequest) (*relaylink.ForwardChunk, error) {
	f.smu.Lock()
	s := f.sessions[req.ID]
	f.smu.Unlock()
	if s == nil || s.owner != relayID || req.Index < 0 {
		return nil, errFwdGone
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = time.Now()
	if req.Index > s.requested {
		s.requested = req.Index
		s.cond.Broadcast() // lets the producer read ahead
	}
	// Drop chunks the relay has moved well past.
	for i := range s.chunks {
		if i < s.requested-fwdKeepBehind {
			delete(s.chunks, i)
		}
	}
	// A context that ends wakes the waiter.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-stop:
		}
	}()
	for {
		if data, ok := s.chunks[req.Index]; ok {
			return &relaylink.ForwardChunk{Data: data, EOF: s.eof && req.Index == s.produced-1}, nil
		}
		if s.err != nil {
			return nil, s.err
		}
		if s.eof && req.Index >= s.produced {
			return &relaylink.ForwardChunk{EOF: true}, nil // beyond the end
		}
		if s.closed || req.Index < s.produced-1-fwdKeepBehind && s.produced > 0 {
			return nil, errFwdGone
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.cond.Wait()
	}
}

// CloseStream ends a session early (the console went away, or the download is done).
func (f *Forwarder) CloseStream(relayID, id string) {
	f.smu.Lock()
	s := f.sessions[id]
	if s != nil && s.owner == relayID {
		delete(f.sessions, id)
	} else {
		s = nil
	}
	f.smu.Unlock()
	if s != nil {
		s.shutdown()
	}
}

// ActiveStreams reports how many streamed forwards are open (for tests and logs).
func (f *Forwarder) ActiveStreams() int {
	f.smu.Lock()
	defer f.smu.Unlock()
	return len(f.sessions)
}

func (f *Forwarder) fwdJanitor() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		f.smu.Lock()
		var dead []*fwdSession
		for id, s := range f.sessions {
			s.mu.Lock()
			idle := time.Since(s.last) > f.idleTimeout()
			s.mu.Unlock()
			if idle {
				dead = append(dead, s)
				delete(f.sessions, id)
			}
		}
		f.smu.Unlock()
		for _, s := range dead {
			s.shutdown()
		}
	}
}

func (f *Forwarder) idleTimeout() time.Duration {
	if f.IdleTimeout > 0 {
		return f.IdleTimeout
	}
	return fwdIdleTimeout
}
