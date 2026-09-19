package relayd

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/Happynico7504/relaylink"
)

// DefaultStreamPort is where the main's hub listens for real-time streams.
const DefaultStreamPort = "7778"

// StreamAddrFromURL derives the stream address from the relay API URL in the
// bundle (same host, stream port), e.g. http://main:7777 -> main:7778.
func StreamAddrFromURL(mainURL string) (string, error) {
	u, err := url.Parse(mainURL)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("relayd: cannot derive the stream address from " + mainURL)
	}
	return net.JoinHostPort(u.Hostname(), DefaultStreamPort), nil
}

// StreamClient keeps the relay's real-time stream to the main connected: it
// reconnects with jittered exponential backoff and calls OnUp for every new
// connection (the relay must re-announce its players there, with presence.set).
type StreamClient struct {
	Client   *relaylink.Client
	Addr     string
	Handlers relaylink.StreamHandlers // Call/Event from the main; Closed is managed here
	Opt      relaylink.StreamOptions
	OnUp     func(*relaylink.StreamConn)
	OnDown   func(error)
	Logf     func(string, ...any)

	MinBackoff time.Duration // default 500ms
	MaxBackoff time.Duration // default 15s

	mu  sync.Mutex
	cur *relaylink.StreamConn
}

func (s *StreamClient) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	}
}

// Conn returns the current stream, or nil while disconnected.
func (s *StreamClient) Conn() *relaylink.StreamConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil {
		select {
		case <-s.cur.Done():
			return nil
		default:
		}
	}
	return s.cur
}

// Send fails fast (ErrStreamClosed) while the stream is down.
func (s *StreamClient) Send(p relaylink.Priority, topic string, body []byte) error {
	if c := s.Conn(); c != nil {
		return c.Send(p, topic, body)
	}
	return relaylink.ErrStreamClosed
}

// Call fails fast (ErrStreamClosed) while the stream is down.
func (s *StreamClient) Call(ctx context.Context, method string, body []byte) ([]byte, error) {
	if c := s.Conn(); c != nil {
		return c.Call(ctx, method, body)
	}
	return nil, relaylink.ErrStreamClosed
}

// Run connects and reconnects until ctx is cancelled.
func (s *StreamClient) Run(ctx context.Context) {
	minB, maxB := s.MinBackoff, s.MaxBackoff
	if minB <= 0 {
		minB = 500 * time.Millisecond
	}
	if maxB <= 0 {
		maxB = 15 * time.Second
	}
	backoff := minB
	for ctx.Err() == nil {
		h := s.Handlers
		down := make(chan error, 1)
		userClosed := h.Closed
		h.Closed = func(c *relaylink.StreamConn, err error) {
			if userClosed != nil {
				userClosed(c, err)
			}
			down <- err
		}
		conn, err := s.Client.DialStream(ctx, s.Addr, h, s.Opt)
		if err != nil {
			s.logf("stream: connect failed: %v (retrying in %v)", err, backoff)
		} else {
			s.mu.Lock()
			s.cur = conn
			s.mu.Unlock()
			s.logf("stream: connected to the main")
			started := time.Now()
			if s.OnUp != nil {
				s.OnUp(conn)
			}
			var derr error
			select {
			case derr = <-down:
			case <-ctx.Done():
				conn.Close()
				return
			}
			s.logf("stream: lost (%v)", derr)
			if s.OnDown != nil {
				s.OnDown(derr)
			}
			if time.Since(started) > 10*time.Second {
				backoff = minB // it was a healthy connection: start over quickly
			}
		}
		wait := backoff + time.Duration(rand.Int63n(int64(backoff)/2+1))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if backoff *= 2; backoff > maxB {
			backoff = maxB
		}
	}
}
