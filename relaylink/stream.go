package relaylink

// A persistent, authenticated, encrypted, two-way stream between a relay and
// the main, for time-critical traffic (events routed to a player's connection,
// calls that need an answer, instant cache invalidations, credential pushes).
//
// Handshake (reuses the relay keys, no new trust):
//
//	relay -> main  ClientHello: relay id, timestamp, random nonce, a random 32-byte
//	               secret K wrapped to the main's RSA key (RSA-OAEP), and an Ed25519
//	               signature over all of it (which relay is calling; revocable).
//	main -> relay  ServerHello: its own nonce and a proof frame encrypted under keys
//	               derived from K. Only the holder of the main's RSA private key can
//	               unwrap K, so a valid proof authenticates the MAIN.
//
// Both directions then use their own AES-256-GCM key (HKDF from K and both
// nonces) with a strictly increasing frame counter as the nonce: a replayed,
// reordered, dropped or forged frame fails authentication and closes the
// stream. There is no forward secrecy (see the package doc in envelope.go).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	streamVersion  = 1
	maxFrameBytes  = 1 << 20
	maxHelloBytes  = 8 << 10
	streamKDFLabel = "relaylink-stream-v1"
)

var (
	ErrStreamClosed = errors.New("relaylink: stream closed")
	ErrQueueFull    = errors.New("relaylink: send queue full")
	ErrPeerSilent   = errors.New("relaylink: peer stopped responding")
	ErrBadFrame     = errors.New("relaylink: bad stream frame")
	ErrBadHandshake = errors.New("relaylink: stream handshake failed")
)

// Priority orders outgoing frames: High always goes before Normal.
type Priority int

const (
	Normal Priority = iota
	High
)

// RemoteError is an error returned by the other side's handler.
type RemoteError string

func (e RemoteError) Error() string { return "remote: " + string(e) }

// StreamHandlers receives what the peer sends. Any may be nil.
type StreamHandlers struct {
	Call   func(ctx context.Context, c *StreamConn, method string, body []byte) ([]byte, error)
	Event  func(c *StreamConn, topic string, body []byte)
	Closed func(c *StreamConn, err error)
}

// StreamOptions tunes timing. Zero values give the production defaults.
type StreamOptions struct {
	HeartbeatEvery time.Duration // default 1s
	DeadAfter      time.Duration // default 3.5s without any frame
	QueueNormal    int           // default 1024
	QueueHigh      int           // default 256
}

func (o StreamOptions) withDefaults() StreamOptions {
	if o.HeartbeatEvery <= 0 {
		o.HeartbeatEvery = time.Second
	}
	if o.DeadAfter <= 0 {
		o.DeadAfter = 3500 * time.Millisecond
	}
	if o.QueueNormal <= 0 {
		o.QueueNormal = 1024
	}
	if o.QueueHigh <= 0 {
		o.QueueHigh = 256
	}
	return o
}

// message types inside a frame
const (
	msgPing byte = iota + 1
	msgPong
	msgRequest
	msgResponse
	msgEvent
	msgBye
)

// StreamConn is one established stream (either side).
type StreamConn struct {
	RelayID string // the relay this stream belongs to (both sides know it)
	conn    net.Conn
	opt     StreamOptions
	h       StreamHandlers

	sendAEAD, recvAEAD cipher.AEAD
	sendDir, recvDir   byte
	recvCounter        uint64

	hi, lo chan []byte
	kick   chan struct{}

	nextID  atomic.Uint64
	pmu     sync.Mutex
	pending map[uint64]chan streamReply

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
	err    error
	done   chan struct{}

	lastRecv atomic.Int64 // unix nanos
	rttNanos atomic.Int64 // smoothed
	rttMin   atomic.Int64
}

type streamReply struct {
	ok   bool
	body []byte
}

// ---- key schedule -----------------------------------------------------------

func hkdf(secret, salt []byte, info string, n int) []byte {
	mac := hmac.New(sha256.New, salt)
	mac.Write(secret)
	prk := mac.Sum(nil)
	var out, t []byte
	for i := byte(1); len(out) < n; i++ {
		m := hmac.New(sha256.New, prk)
		m.Write(t)
		m.Write([]byte(info))
		m.Write([]byte{i})
		t = m.Sum(nil)
		out = append(out, t...)
	}
	return out[:n]
}

func deriveStreamKeys(secret, cNonce, sNonce []byte) (c2s, s2c cipher.AEAD, err error) {
	salt := append(append([]byte{}, cNonce...), sNonce...)
	k := hkdf(secret, salt, streamKDFLabel, 64)
	mk := func(b []byte) (cipher.AEAD, error) {
		blk, err := aes.NewCipher(b)
		if err != nil {
			return nil, err
		}
		return cipher.NewGCM(blk)
	}
	if c2s, err = mk(k[:32]); err != nil {
		return
	}
	s2c, err = mk(k[32:])
	return
}

func frameNonce(dir byte, counter uint64) []byte {
	n := make([]byte, 12)
	n[0] = dir
	binary.BigEndian.PutUint64(n[4:], counter)
	return n
}

// ---- hello messages -----------------------------------------------------------

type clientHello struct {
	V          int    `json:"v"`
	RelayID    string `json:"relay_id"`
	TS         int64  `json:"ts"` // unix ms
	Nonce      []byte `json:"nonce"`
	WrappedKey []byte `json:"k"`
	Sig        []byte `json:"sig"`
}

func helloSigBytes(h *clientHello) []byte {
	var b bytes.Buffer
	b.WriteString("relaylink-stream-hello-v1\x00")
	b.WriteString(h.RelayID)
	b.WriteByte(0)
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(h.TS))
	b.Write(t[:])
	b.Write(h.Nonce)
	sum := sha256.Sum256(h.WrappedKey)
	b.Write(sum[:])
	return b.Bytes()
}

func writeBlob(w io.Writer, b []byte) error {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(b)))
	if _, err := w.Write(l[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readBlob(r io.Reader, max int) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(l[:])
	if n > uint32(max) {
		return nil, ErrBadFrame
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}

const serverProof = "relaylink-stream-ok"

// ---- client side ----------------------------------------------------------------

// DialStream connects to the main's stream listener and completes the handshake.
func (c *Client) DialStream(ctx context.Context, addr string, h StreamHandlers, opt StreamOptions) (*StreamConn, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	sc, err := c.handshakeClient(nc, h, opt)
	if err != nil {
		nc.Close()
		return nil, err
	}
	return sc, nil
}

func (c *Client) handshakeClient(nc net.Conn, h StreamHandlers, opt StreamOptions) (*StreamConn, error) {
	nc.SetDeadline(time.Now().Add(10 * time.Second))
	secret := make([]byte, 32)
	cNonce := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rand.Reader, cNonce); err != nil {
		return nil, err
	}
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, c.MainPub, secret, []byte(oaepLabel+"-stream"))
	if err != nil {
		return nil, err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	hello := &clientHello{V: streamVersion, RelayID: c.RelayID, TS: now().UnixMilli(), Nonce: cNonce, WrappedKey: wrapped}
	hello.Sig = ed25519.Sign(c.Sign, helloSigBytes(hello))
	raw, _ := json.Marshal(hello)
	if err := writeBlob(nc, raw); err != nil {
		return nil, err
	}
	sHello, err := readBlob(nc, 256)
	if err != nil || len(sHello) < 16+16 { // server nonce + at least a GCM tag
		return nil, ErrBadHandshake
	}
	sNonce := sHello[:16]
	proofCT := sHello[16:]
	c2s, s2c, err := deriveStreamKeys(secret, cNonce, sNonce)
	if err != nil {
		return nil, err
	}
	// The proof is server->client frame #1: only the holder of the main's RSA
	// private key could have derived the key that opens it.
	pt, err := s2c.Open(nil, frameNonce(1, 1), proofCT, nil)
	if err != nil || string(pt) != serverProof {
		return nil, ErrBadHandshake
	}
	nc.SetDeadline(time.Time{})
	sc := newStreamConn(nc, c.RelayID, c2s, s2c, 0, 1, h, opt)
	sc.recvCounter = 1 // the proof consumed s2c counter 1
	sc.start()
	return sc, nil
}

// ---- server side ----------------------------------------------------------------

// ServeStream accepts stream connections until ln is closed. onConn is called
// for every relay that completes the handshake, in its own goroutine.
func (s *Server) ServeStream(ln net.Listener, opt StreamOptions, mk func(relayID string) StreamHandlers, onConn func(*StreamConn)) error {
	for {
		nc, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			sc, err := s.handshakeServer(nc, mk, opt)
			if err != nil {
				nc.Close() // one opaque outcome for every failure
				return
			}
			onConn(sc)
		}()
	}
}

func (s *Server) handshakeServer(nc net.Conn, mk func(string) StreamHandlers, opt StreamOptions) (*StreamConn, error) {
	nc.SetDeadline(time.Now().Add(10 * time.Second))
	raw, err := readBlob(nc, maxHelloBytes)
	if err != nil {
		return nil, err
	}
	var h clientHello
	if json.Unmarshal(raw, &h) != nil || h.V != streamVersion || h.RelayID == "" || len(h.Nonce) != 16 || len(h.WrappedKey) == 0 {
		return nil, ErrBadHandshake
	}
	pub, ok := s.RelayKey(h.RelayID)
	if !ok || !ed25519.Verify(pub, helloSigBytes(&h), h.Sig) {
		return nil, ErrBadHandshake
	}
	skew := s.now().Sub(time.UnixMilli(h.TS))
	if skew > MaxClockSkew || skew < -MaxClockSkew {
		return nil, ErrBadHandshake
	}
	secret, err := rsa.DecryptOAEP(sha256.New(), nil, s.Priv, h.WrappedKey, []byte(oaepLabel+"-stream"))
	if err != nil || len(secret) != 32 {
		return nil, ErrBadHandshake
	}
	seen, err := s.Replay.Seen(context.Background(), h.RelayID, h.Nonce, 2*MaxClockSkew)
	if err != nil || seen {
		return nil, ErrBadHandshake
	}
	sNonce := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, sNonce); err != nil {
		return nil, err
	}
	c2s, s2c, err := deriveStreamKeys(secret, h.Nonce, sNonce)
	if err != nil {
		return nil, err
	}
	proof := s2c.Seal(nil, frameNonce(1, 1), []byte(serverProof), nil)
	if err := writeBlob(nc, append(sNonce, proof...)); err != nil {
		return nil, err
	}
	nc.SetDeadline(time.Time{})
	sc := newStreamConn(nc, h.RelayID, s2c, c2s, 1, 0, mk(h.RelayID), opt)
	sc.start()
	return sc, nil
}

// ---- the stream itself -----------------------------------------------------------

func newStreamConn(nc net.Conn, relayID string, send, recv cipher.AEAD, sendDir, recvDir byte, h StreamHandlers, opt StreamOptions) *StreamConn {
	opt = opt.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	sc := &StreamConn{
		RelayID: relayID, conn: nc, opt: opt, h: h,
		sendAEAD: send, recvAEAD: recv, sendDir: sendDir, recvDir: recvDir,
		hi: make(chan []byte, opt.QueueHigh), lo: make(chan []byte, opt.QueueNormal),
		kick: make(chan struct{}, 1), pending: map[uint64]chan streamReply{},
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	sc.lastRecv.Store(time.Now().UnixNano())
	return sc
}

func (sc *StreamConn) start() {
	go sc.writeLoop()
	go sc.readLoop()
	go sc.heartbeatLoop()
}

// Done is closed when the stream ends; Err then says why.
func (sc *StreamConn) Done() <-chan struct{} { return sc.done }
func (sc *StreamConn) Err() error            { <-sc.done; return sc.err }

// RTT is the smoothed heartbeat round-trip time (0 until the first pong).
func (sc *StreamConn) RTT() time.Duration    { return time.Duration(sc.rttNanos.Load()) }
func (sc *StreamConn) RTTMin() time.Duration { return time.Duration(sc.rttMin.Load()) }

func (sc *StreamConn) Close() { sc.fail(ErrStreamClosed) }

func (sc *StreamConn) fail(err error) {
	sc.once.Do(func() {
		sc.err = err
		sc.cancel()
		sc.conn.Close()
		close(sc.done)
		sc.pmu.Lock()
		for id, ch := range sc.pending {
			close(ch)
			delete(sc.pending, id)
		}
		sc.pmu.Unlock()
		if sc.h.Closed != nil {
			go sc.h.Closed(sc, err)
		}
	})
}

// enqueue puts one encoded message on a queue. The frame counter is assigned by
// the writer when the frame is actually sent, so ordering on the wire always
// matches the counter.
func (sc *StreamConn) enqueue(p Priority, msg []byte) error {
	select {
	case <-sc.done:
		return ErrStreamClosed
	default:
	}
	q := sc.lo
	if p == High {
		q = sc.hi
	}
	select {
	case q <- msg:
	default:
		return ErrQueueFull
	}
	select {
	case sc.kick <- struct{}{}:
	default:
	}
	return nil
}

func (sc *StreamConn) writeLoop() {
	bw := bufio.NewWriterSize(sc.conn, 32<<10)
	var counter uint64
	if sc.sendDir == 1 {
		counter = 1 // server->client counter 1 was the handshake proof
	}
	send := func(msg []byte) bool {
		counter++
		ct := sc.sendAEAD.Seal(nil, frameNonce(sc.sendDir, counter), msg, nil)
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
		if _, err := bw.Write(l[:]); err != nil {
			sc.fail(err)
			return false
		}
		if _, err := bw.Write(ct); err != nil {
			sc.fail(err)
			return false
		}
		return true
	}
	for {
		var msg []byte
		select {
		case msg = <-sc.hi:
		default:
			select {
			case msg = <-sc.hi:
			case msg = <-sc.lo:
			case <-sc.kick:
				continue
			case <-sc.done:
				return
			}
		}
		if !send(msg) {
			return
		}
		// Drain what is already queued (high first) before flushing once.
		for more := true; more; {
			select {
			case m := <-sc.hi:
				if !send(m) {
					return
				}
			default:
				select {
				case m := <-sc.hi:
					if !send(m) {
						return
					}
				case m := <-sc.lo:
					if !send(m) {
						return
					}
				default:
					more = false
				}
			}
		}
		if err := bw.Flush(); err != nil {
			sc.fail(err)
			return
		}
	}
}

func (sc *StreamConn) readLoop() {
	br := bufio.NewReaderSize(sc.conn, 32<<10)
	for {
		ct, err := readBlob(br, maxFrameBytes+64)
		if err != nil {
			sc.fail(err)
			return
		}
		sc.recvCounter++
		pt, err := sc.recvAEAD.Open(nil, frameNonce(sc.recvDir, sc.recvCounter), ct, nil)
		if err != nil {
			sc.fail(ErrBadFrame) // forged, replayed, reordered or dropped frame
			return
		}
		sc.lastRecv.Store(time.Now().UnixNano())
		if len(pt) == 0 || !sc.dispatch(pt) {
			sc.fail(ErrBadFrame)
			return
		}
	}
}

func (sc *StreamConn) heartbeatLoop() {
	t := time.NewTicker(sc.opt.HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-sc.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, sc.lastRecv.Load())) > sc.opt.DeadAfter {
				sc.fail(ErrPeerSilent)
				return
			}
			var b [9]byte
			b[0] = msgPing
			binary.BigEndian.PutUint64(b[1:], uint64(time.Now().UnixNano()))
			sc.enqueue(High, b[:]) // a full queue just skips one beat
		}
	}
}

// ---- messages ----------------------------------------------------------------------

func appendBytes(dst, b []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

func readBytes(p []byte) (b, rest []byte, ok bool) {
	n, k := binary.Uvarint(p)
	if k <= 0 || uint64(len(p)-k) < n {
		return nil, nil, false
	}
	return p[k : k+int(n)], p[k+int(n):], true
}

func (sc *StreamConn) dispatch(pt []byte) bool {
	typ, p := pt[0], pt[1:]
	switch typ {
	case msgPing:
		if len(p) != 8 {
			return false
		}
		pong := append([]byte{msgPong}, p...)
		sc.enqueue(High, pong)
		return true
	case msgPong:
		if len(p) != 8 {
			return false
		}
		rtt := time.Now().UnixNano() - int64(binary.BigEndian.Uint64(p))
		if rtt < 0 {
			rtt = 0
		}
		if old := sc.rttNanos.Load(); old == 0 {
			sc.rttNanos.Store(rtt)
		} else {
			sc.rttNanos.Store((old*7 + rtt) / 8)
		}
		if m := sc.rttMin.Load(); m == 0 || rtt < m {
			sc.rttMin.Store(rtt)
		}
		return true
	case msgRequest:
		if len(p) < 8 {
			return false
		}
		id := binary.BigEndian.Uint64(p[:8])
		method, rest, ok := readBytes(p[8:])
		if !ok {
			return false
		}
		body, _, ok := readBytes(rest)
		if !ok {
			return false
		}
		go sc.serveCall(id, string(method), append([]byte(nil), body...))
		return true
	case msgResponse:
		if len(p) < 9 {
			return false
		}
		id, okb := binary.BigEndian.Uint64(p[:8]), p[8] == 1
		body, _, ok := readBytes(p[9:])
		if !ok {
			return false
		}
		sc.pmu.Lock()
		ch := sc.pending[id]
		delete(sc.pending, id)
		sc.pmu.Unlock()
		if ch != nil {
			ch <- streamReply{ok: okb, body: append([]byte(nil), body...)}
		}
		return true
	case msgEvent:
		topic, rest, ok := readBytes(p)
		if !ok {
			return false
		}
		body, _, ok := readBytes(rest)
		if !ok {
			return false
		}
		if sc.h.Event != nil {
			sc.h.Event(sc, string(topic), append([]byte(nil), body...))
		}
		return true
	case msgBye:
		sc.fail(ErrStreamClosed)
		return true
	}
	return false
}

func (sc *StreamConn) serveCall(id uint64, method string, body []byte) {
	var out []byte
	var err error
	if sc.h.Call == nil {
		err = errors.New("no handler")
	} else {
		out, err = sc.h.Call(sc.ctx, sc, method, body)
	}
	msg := make([]byte, 0, 10+len(out))
	msg = append(msg, msgResponse)
	msg = binary.BigEndian.AppendUint64(msg, id)
	if err != nil {
		msg = append(msg, 0)
		msg = appendBytes(msg, []byte(err.Error()))
	} else {
		msg = append(msg, 1)
		msg = appendBytes(msg, out)
	}
	sc.enqueue(High, msg)
}

// Call sends a request and waits for the peer's answer.
func (sc *StreamConn) Call(ctx context.Context, method string, body []byte) ([]byte, error) {
	id := sc.nextID.Add(1)
	ch := make(chan streamReply, 1)
	sc.pmu.Lock()
	select {
	case <-sc.done:
		sc.pmu.Unlock()
		return nil, ErrStreamClosed
	default:
	}
	sc.pending[id] = ch
	sc.pmu.Unlock()
	msg := []byte{msgRequest}
	msg = binary.BigEndian.AppendUint64(msg, id)
	msg = appendBytes(msg, []byte(method))
	msg = appendBytes(msg, body)
	if len(msg) > maxFrameBytes {
		sc.dropPending(id)
		return nil, fmt.Errorf("relaylink: request of %d bytes exceeds the frame limit", len(msg))
	}
	if err := sc.enqueue(High, msg); err != nil {
		sc.dropPending(id)
		return nil, err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return nil, ErrStreamClosed
		}
		if !r.ok {
			return nil, RemoteError(r.body)
		}
		return r.body, nil
	case <-ctx.Done():
		sc.dropPending(id)
		return nil, ctx.Err()
	}
}

func (sc *StreamConn) dropPending(id uint64) {
	sc.pmu.Lock()
	delete(sc.pending, id)
	sc.pmu.Unlock()
}

// Send delivers a one-way event. It never blocks: if the queue is full it
// returns ErrQueueFull and the caller decides what to do.
func (sc *StreamConn) Send(p Priority, topic string, body []byte) error {
	msg := []byte{msgEvent}
	msg = appendBytes(msg, []byte(topic))
	msg = appendBytes(msg, body)
	if len(msg) > maxFrameBytes {
		return fmt.Errorf("relaylink: event of %d bytes exceeds the frame limit", len(msg))
	}
	return sc.enqueue(p, msg)
}
