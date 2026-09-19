package relaylink

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type streamRig struct {
	srv    *Server
	client *Client
	ln     net.Listener
	addr   string
	mu     sync.Mutex
	conns  []*StreamConn
	connCh chan *StreamConn
	mk     func(relayID string) StreamHandlers
	opt    StreamOptions
}

func newStreamRig(t *testing.T, serverH func(string) StreamHandlers, opt StreamOptions) *streamRig {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub, sk, _ := ed25519.GenerateKey(rand.Reader)
	r := &streamRig{connCh: make(chan *StreamConn, 8), opt: opt, mk: serverH}
	r.srv = &Server{Priv: priv, Replay: &MemoryReplay{},
		RelayKey: func(id string) (ed25519.PublicKey, bool) { return pub, id == "us-1" }}
	r.client = &Client{RelayID: "us-1", MainPub: &priv.PublicKey, Sign: sk}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.ln, r.addr = ln, ln.Addr().String()
	go r.srv.ServeStream(ln, opt, func(id string) StreamHandlers {
		if r.mk != nil {
			return r.mk(id)
		}
		return StreamHandlers{}
	}, func(c *StreamConn) {
		r.mu.Lock()
		r.conns = append(r.conns, c)
		r.mu.Unlock()
		r.connCh <- c
	})
	t.Cleanup(func() {
		ln.Close()
		r.mu.Lock()
		for _, c := range r.conns {
			c.Close()
		}
		r.mu.Unlock()
	})
	return r
}

func (r *streamRig) dial(t *testing.T, h StreamHandlers) (client, server *StreamConn) {
	t.Helper()
	c, err := r.client.DialStream(context.Background(), r.addr, h, r.opt)
	if err != nil {
		t.Fatalf("DialStream: %v", err)
	}
	t.Cleanup(c.Close)
	select {
	case s := <-r.connCh:
		return c, s
	case <-time.After(3 * time.Second):
		t.Fatal("server never saw the connection")
	}
	return
}

func echoHandlers() StreamHandlers {
	return StreamHandlers{Call: func(_ context.Context, _ *StreamConn, method string, body []byte) ([]byte, error) {
		switch method {
		case "echo":
			return body, nil
		case "fail":
			return nil, errors.New("nope: " + string(body))
		}
		return nil, errors.New("unknown method")
	}}
}

func TestStreamCallsBothWays(t *testing.T) {
	r := newStreamRig(t, func(string) StreamHandlers { return echoHandlers() }, StreamOptions{})
	c, s := r.dial(t, echoHandlers())
	if c.RelayID != "us-1" || s.RelayID != "us-1" {
		t.Fatalf("relay ids: %q %q", c.RelayID, s.RelayID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if got, err := c.Call(ctx, "echo", []byte("relay->main")); err != nil || string(got) != "relay->main" {
		t.Fatalf("client call: %q %v", got, err)
	}
	if got, err := s.Call(ctx, "echo", []byte("main->relay")); err != nil || string(got) != "main->relay" {
		t.Fatalf("server call: %q %v", got, err)
	}
	_, err := c.Call(ctx, "fail", []byte("x"))
	var re RemoteError
	if !errors.As(err, &re) || string(re) != "nope: x" {
		t.Fatalf("remote error: %v", err)
	}
	if _, err := c.Call(ctx, "missing", nil); err == nil {
		t.Fatal("unknown method succeeded")
	}
	if got, err := c.Call(ctx, "echo", nil); err != nil || len(got) != 0 {
		t.Fatalf("empty body: %q %v", got, err)
	}
	big := bytes.Repeat([]byte{7}, 700<<10)
	if got, err := c.Call(ctx, "echo", big); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("700KB body: %v", err)
	}
}

func TestStreamConcurrentCalls(t *testing.T) {
	r := newStreamRig(t, func(string) StreamHandlers { return echoHandlers() }, StreamOptions{})
	c, _ := r.dial(t, echoHandlers())
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			want := fmt.Sprintf("call-%d", i)
			if got, err := c.Call(ctx, "echo", []byte(want)); err != nil || string(got) != want {
				bad.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d of 300 concurrent calls got a wrong or missing answer", bad.Load())
	}
	c.pmu.Lock()
	n := len(c.pending)
	c.pmu.Unlock()
	if n != 0 {
		t.Fatalf("%d pending entries leaked", n)
	}
}

func TestStreamCallTimeoutAndCancelDoNotLeak(t *testing.T) {
	block := make(chan struct{})
	h := StreamHandlers{Call: func(ctx context.Context, _ *StreamConn, _ string, _ []byte) ([]byte, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil, nil
	}}
	r := newStreamRig(t, func(string) StreamHandlers { return h }, StreamOptions{})
	c, _ := r.dial(t, StreamHandlers{})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, "slow", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	c.pmu.Lock()
	n := len(c.pending)
	c.pmu.Unlock()
	if n != 0 {
		t.Fatalf("timed-out call left %d pending entries", n)
	}
	close(block)
}

func TestStreamEventsArriveInOrder(t *testing.T) {
	var mu sync.Mutex
	var got []int
	done := make(chan struct{})
	h := StreamHandlers{Event: func(_ *StreamConn, topic string, body []byte) {
		mu.Lock()
		got = append(got, int(binary.BigEndian.Uint32(body)))
		if len(got) == 4000 {
			close(done)
		}
		mu.Unlock()
	}}
	r := newStreamRig(t, func(string) StreamHandlers { return h }, StreamOptions{QueueNormal: 5000})
	c, _ := r.dial(t, StreamHandlers{})
	for i := 0; i < 4000; i++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(i))
		if err := c.Send(Normal, "seq", b[:]); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("only %d of 4000 events arrived", len(got))
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("event %d arrived as %d: out of order", i, v)
		}
	}
}

func TestStreamHighPriorityOvertakesBacklog(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	var first sync.Once
	h := StreamHandlers{Event: func(_ *StreamConn, topic string, body []byte) {
		first.Do(func() { <-release }) // a slow consumer: the sender's queue backs up
		mu.Lock()
		order = append(order, topic)
		mu.Unlock()
	}}
	r := newStreamRig(t, func(string) StreamHandlers { return h }, StreamOptions{QueueNormal: 4000})
	c, _ := r.dial(t, StreamHandlers{})
	payload := make([]byte, 128<<10) // large enough to fill the socket buffers and stall the writer
	for i := 0; i < 400; i++ {
		if err := c.Send(Normal, "bulk", payload); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // writer is now blocked with a backlog
	c.Send(High, "URGENT", []byte("probe"))
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	pos := -1
	for time.Now().Before(deadline) && pos < 0 {
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		for i, tp := range order {
			if tp == "URGENT" {
				pos = i
			}
		}
		mu.Unlock()
	}
	if pos < 0 {
		t.Fatal("urgent event never arrived")
	}
	if pos > 380 {
		t.Fatalf("urgent event arrived after %d bulk events: it did not overtake the backlog", pos)
	}
	t.Logf("urgent event overtook the backlog: delivered at position %d of ~400", pos)
}

// ---- tampering ---------------------------------------------------------------------

type hookConn struct {
	net.Conn
	mu     sync.Mutex
	writes int
	hook   func(n int, p []byte) (out [][]byte)
}

func (h *hookConn) Write(p []byte) (int, error) {
	h.mu.Lock()
	h.writes++
	n := h.writes
	hook := h.hook
	h.mu.Unlock()
	if hook == nil {
		return h.Conn.Write(p)
	}
	for _, chunk := range hook(n, p) {
		if _, err := h.Conn.Write(chunk); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (h *hookConn) setHook(f func(n int, p []byte) [][]byte) {
	h.mu.Lock()
	h.hook = f
	h.writes = 0 // count only the writes made after the hook is installed
	h.mu.Unlock()
}

// handshakeThrough completes a real handshake over a connection whose later
// writes (client->server) can be tampered with.
func handshakeThrough(t *testing.T, r *streamRig) (*StreamConn, *StreamConn, *hookConn) {
	t.Helper()
	raw, err := net.Dial("tcp", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	hc := &hookConn{Conn: raw}
	c, err := r.client.handshakeClient(hc, StreamHandlers{}, r.opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, <-r.connCh, hc
}

func TestStreamDetectsTamperedReplayedDroppedFrames(t *testing.T) {
	for name, hook := range map[string]func(n int, p []byte) [][]byte{
		"flipped byte": func(n int, p []byte) [][]byte {
			q := append([]byte(nil), p...)
			q[len(q)-1] ^= 1
			return [][]byte{q}
		},
		"replayed frame": func(n int, p []byte) [][]byte { return [][]byte{p, p} },
		"dropped frame": func(n int, p []byte) [][]byte {
			if n == 1 {
				return nil // first send vanishes, the next one arrives out of sequence
			}
			return [][]byte{p}
		},
		"truncated frame": func(n int, p []byte) [][]byte { return [][]byte{p[:len(p)/2]} },
	} {
		closed := make(chan error, 1)
		r := newStreamRig(t, func(string) StreamHandlers {
			return StreamHandlers{Closed: func(_ *StreamConn, err error) { closed <- err }, Event: func(*StreamConn, string, []byte) {}}
		}, StreamOptions{HeartbeatEvery: time.Hour, DeadAfter: time.Hour})
		c, _, hc := handshakeThrough(t, r)
		hc.setHook(hook)
		c.Send(High, "a", []byte("first"))
		time.Sleep(30 * time.Millisecond)
		c.Send(High, "b", []byte("second"))
		select {
		case err := <-closed:
			if name != "truncated frame" && !errors.Is(err, ErrBadFrame) {
				t.Errorf("%s: closed with %v, want ErrBadFrame", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s: the server kept the stream open", name)
		}
	}
}

func TestStreamHandshakeRejections(t *testing.T) {
	r := newStreamRig(t, nil, StreamOptions{})
	tryDial := func(c *Client) error {
		nc, err := net.Dial("tcp", r.addr)
		if err != nil {
			return err
		}
		defer nc.Close()
		sc, err := c.handshakeClient(nc, StreamHandlers{}, StreamOptions{})
		if err == nil {
			sc.Close()
		}
		return err
	}
	good := *r.client
	if err := tryDial(&good); err != nil {
		t.Fatalf("baseline handshake failed: %v", err)
	}
	<-r.connCh

	unknown := good
	unknown.RelayID = "jp-9"
	if tryDial(&unknown) == nil {
		t.Error("unknown relay accepted")
	}
	_, otherSK, _ := ed25519.GenerateKey(rand.Reader)
	forged := good
	forged.Sign = otherSK
	if tryDial(&forged) == nil {
		t.Error("wrong signing key accepted")
	}
	otherRSA, _ := rsa.GenerateKey(rand.Reader, 2048)
	wrongMain := good
	wrongMain.MainPub = &otherRSA.PublicKey // client thinks it talks to a different main
	if tryDial(&wrongMain) == nil {
		t.Error("handshake with the wrong main key succeeded")
	}
	stale := good
	stale.Now = func() time.Time { return time.Now().Add(-2 * MaxClockSkew) }
	if tryDial(&stale) == nil {
		t.Error("stale timestamp accepted")
	}
	future := good
	future.Now = func() time.Time { return time.Now().Add(2 * MaxClockSkew) }
	if tryDial(&future) == nil {
		t.Error("future timestamp accepted")
	}
	// Garbage, oversized hello and silence must all just end the connection.
	for name, payload := range map[string][]byte{
		"garbage":  []byte("GET / HTTP/1.1\r\n\r\n"),
		"oversize": append([]byte{0, 0xff, 0xff, 0xff}, make([]byte, 100)...),
		"bad json": append([]byte{0, 0, 0, 5}, []byte("{nope")...),
	} {
		nc, _ := net.Dial("tcp", r.addr)
		nc.Write(payload)
		nc.SetReadDeadline(time.Now().Add(2 * time.Second))
		if n, _ := nc.Read(make([]byte, 64)); n != 0 {
			t.Errorf("%s: server answered %d bytes to a bad hello", name, n)
		}
		nc.Close()
	}
}

func TestStreamReplayedHelloIsRejected(t *testing.T) {
	r := newStreamRig(t, nil, StreamOptions{})
	// Record a real ClientHello, then replay it on a fresh connection.
	rec, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	captured := make(chan []byte, 1)
	go func() {
		in, _ := rec.Accept()
		out, _ := net.Dial("tcp", r.addr)
		hello, _ := readBlob(in, maxHelloBytes)
		captured <- hello
		writeBlob(out, hello)
		go io.Copy(in, out)
		io.Copy(out, in)
	}()
	nc, _ := net.Dial("tcp", rec.Addr().String())
	sc, err := r.client.handshakeClient(nc, StreamHandlers{}, StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	<-r.connCh
	hello := <-captured
	nc2, _ := net.Dial("tcp", r.addr)
	defer nc2.Close()
	writeBlob(nc2, hello)
	nc2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _ := nc2.Read(make([]byte, 64)); n != 0 {
		t.Fatal("a replayed ClientHello was accepted")
	}
}

func TestStreamClientRejectsAFakeServer(t *testing.T) {
	// A server that does not hold the main's RSA key cannot produce the proof.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, _ := ln.Accept()
		readBlob(c, maxHelloBytes)
		junk := make([]byte, 16+len(serverProof)+16)
		rand.Read(junk)
		writeBlob(c, junk)
		io.Copy(io.Discard, c)
	}()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	_, sk, _ := ed25519.GenerateKey(rand.Reader)
	cl := &Client{RelayID: "us-1", MainPub: &priv.PublicKey, Sign: sk}
	nc, _ := net.Dial("tcp", ln.Addr().String())
	if _, err := cl.handshakeClient(nc, StreamHandlers{}, StreamOptions{}); !errors.Is(err, ErrBadHandshake) {
		t.Fatalf("client accepted an unauthenticated server: %v", err)
	}
}

func TestStreamFailsClosedWhenReplayStoreDown(t *testing.T) {
	r := newStreamRig(t, nil, StreamOptions{})
	r.srv.Replay = failingReplayStore{}
	nc, _ := net.Dial("tcp", r.addr)
	defer nc.Close()
	if _, err := r.client.handshakeClient(nc, StreamHandlers{}, StreamOptions{}); err == nil {
		t.Fatal("stream accepted while the replay store was unavailable")
	}
}

type failingReplayStore struct{}

func (failingReplayStore) Seen(context.Context, string, []byte, time.Duration) (bool, error) {
	return false, errors.New("down")
}

// ---- liveness ------------------------------------------------------------------------

func TestStreamHeartbeatMeasuresRTT(t *testing.T) {
	r := newStreamRig(t, nil, StreamOptions{HeartbeatEvery: 40 * time.Millisecond, DeadAfter: time.Second})
	c, s := r.dial(t, StreamHandlers{})
	deadline := time.Now().Add(2 * time.Second)
	for (c.RTT() == 0 || s.RTT() == 0) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c.RTT() == 0 || s.RTT() == 0 {
		t.Fatal("no RTT measured")
	}
	if c.RTT() > 50*time.Millisecond {
		t.Fatalf("loopback RTT %v is implausibly high", c.RTT())
	}
	t.Logf("loopback heartbeat RTT: client=%v server=%v (min %v)", c.RTT(), s.RTT(), c.RTTMin())
}

func TestStreamDetectsADeadPeer(t *testing.T) {
	r := newStreamRig(t, nil, StreamOptions{HeartbeatEvery: 40 * time.Millisecond, DeadAfter: 250 * time.Millisecond})
	raw, _ := net.Dial("tcp", r.addr)
	hc := &hookConn{Conn: raw}
	c, err := r.client.handshakeClient(hc, StreamHandlers{}, r.opt)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	srv := <-r.connCh
	// Blackhole the CLIENT's outgoing traffic: the server hears nothing more.
	start := time.Now()
	hc.setHook(func(int, []byte) [][]byte { return nil })
	select {
	case <-srv.Done():
		if !errors.Is(srv.err, ErrPeerSilent) {
			t.Fatalf("server closed with %v, want ErrPeerSilent", srv.err)
		}
		if d := time.Since(start); d > 800*time.Millisecond {
			t.Fatalf("took %v to notice a dead peer (limit 250ms + a heartbeat)", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dead peer never detected")
	}
}

func TestStreamCloseFailsPendingCalls(t *testing.T) {
	block := make(chan struct{})
	h := StreamHandlers{Call: func(ctx context.Context, _ *StreamConn, _ string, _ []byte) ([]byte, error) {
		<-block
		return nil, nil
	}}
	r := newStreamRig(t, func(string) StreamHandlers { return h }, StreamOptions{})
	c, s := r.dial(t, StreamHandlers{})
	res := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "x", nil); res <- err }()
	time.Sleep(100 * time.Millisecond)
	s.Close()
	select {
	case err := <-res:
		if !errors.Is(err, ErrStreamClosed) {
			t.Fatalf("pending call ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a call was left hanging after the stream closed")
	}
	close(block)
	if _, err := c.Call(context.Background(), "y", nil); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("call on a closed stream: %v", err)
	}
	if err := c.Send(Normal, "t", nil); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("send on a closed stream: %v", err)
	}
}

func TestStreamBackpressureIsBoundedAndNeverBlocks(t *testing.T) {
	stall := make(chan struct{})
	var once sync.Once
	h := StreamHandlers{Event: func(*StreamConn, string, []byte) { once.Do(func() { <-stall }) }}
	r := newStreamRig(t, func(string) StreamHandlers { return h }, StreamOptions{QueueNormal: 64})
	c, _ := r.dial(t, StreamHandlers{})
	payload := make([]byte, 256<<10)
	full := false
	start := time.Now()
	for i := 0; i < 5000 && !full; i++ {
		if err := c.Send(Normal, "bulk", payload); errors.Is(err, ErrQueueFull) {
			full = true
		}
	}
	if !full {
		t.Fatal("an unbounded queue: a stalled peer never produced ErrQueueFull")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("Send blocked on a stalled peer")
	}
	close(stall)
}

func TestStreamRejectsOversizedMessagesButStaysUsable(t *testing.T) {
	r := newStreamRig(t, func(string) StreamHandlers { return echoHandlers() }, StreamOptions{})
	c, _ := r.dial(t, echoHandlers())
	huge := make([]byte, maxFrameBytes+1)
	if _, err := c.Call(context.Background(), "echo", huge); err == nil {
		t.Fatal("oversized call accepted")
	}
	if err := c.Send(Normal, "t", huge); err == nil {
		t.Fatal("oversized event accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if got, err := c.Call(ctx, "echo", []byte("still fine")); err != nil || string(got) != "still fine" {
		t.Fatalf("stream broken after rejecting an oversized message: %v", err)
	}
}

// ---- performance ----------------------------------------------------------------------

func BenchmarkStreamCallLoopback(b *testing.B) {
	t := &testing.T{}
	r := newStreamRig(t, func(string) StreamHandlers { return echoHandlers() }, StreamOptions{})
	c, _ := r.dial(t, echoHandlers())
	ctx := context.Background()
	body := []byte("0123456789abcdef0123456789abcdef")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Call(ctx, "echo", body); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRawTCPEchoLoopback(b *testing.B) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, _ := ln.Accept()
		io.Copy(c, c)
	}()
	c, _ := net.Dial("tcp", ln.Addr().String())
	defer c.Close()
	body := make([]byte, 32)
	buf := make([]byte, 32)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Write(body)
		io.ReadFull(c, buf)
	}
}

var _ = json.Marshal
