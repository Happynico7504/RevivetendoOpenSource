package relayd

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/Happynico7504/relaylink"
)

// WSCBridge connects the edge child process to the main. The child speaks
// relaylink.EdgePipeMsg lines over its pipe; the bridge turns them into stream calls to the
// hub (wsc.open / wsc.rmc / wsc.alive / wsc.close) and turns the hub's wsc.out events back
// into pipe lines. It knows nothing about PRUDP or the game.
type WSCBridge struct {
	// Call makes a stream call to the main; it fails fast while the stream is down.
	Call func(ctx context.Context, method string, body []byte) ([]byte, error)
	Logf func(string, ...any)

	// CallTimeout bounds an open or rmc call (default 5s).
	CallTimeout time.Duration

	mu  sync.Mutex
	enc *json.Encoder // the current child's input; nil while no child runs
}

func (b *WSCBridge) logf(f string, a ...any) {
	if b.Logf != nil {
		b.Logf(f, a...)
	}
}

func (b *WSCBridge) timeout() time.Duration {
	if b.CallTimeout > 0 {
		return b.CallTimeout
	}
	return 5 * time.Second
}

func (b *WSCBridge) write(m relaylink.EdgePipeMsg) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.enc == nil {
		return false
	}
	return b.enc.Encode(m) == nil
}

// Attach serves one child until its end of the pipe closes. It is the supervisor's Pipe
// callback, so it runs once per launch.
func (b *WSCBridge) Attach(fromChild io.Reader, toChild io.Writer) {
	b.mu.Lock()
	b.enc = json.NewEncoder(toChild)
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.enc = nil
		b.mu.Unlock()
	}()

	sc := bufio.NewScanner(fromChild)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		var m relaylink.EdgePipeMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			b.logf("wsc bridge: bad line from the edge: %v", err)
			continue
		}
		go b.handle(m)
	}
}

func (b *WSCBridge) handle(m relaylink.EdgePipeMsg) {
	var method string
	var body any
	switch m.T {
	case relaylink.EdgeOpen:
		method, body = relaylink.MethodWSCOpen, relaylink.WSCOpen{PID: m.PID, IP: m.IP, Port: m.Port}
	case relaylink.EdgeRMC:
		method, body = relaylink.MethodWSCRMC, relaylink.WSCRMC{PID: m.PID, Call: m.Call, Proto: m.Proto, Custom: m.Custom, Method: m.Method, Params: m.Params}
	case relaylink.EdgeAlive:
		method, body = relaylink.MethodWSCAlive, relaylink.WSCAlive{PIDs: m.PIDs}
	case relaylink.EdgeClose:
		method, body = relaylink.MethodWSCClose, relaylink.WSCClose{PID: m.PID}
	default:
		b.logf("wsc bridge: unknown message %q from the edge", m.T)
		return
	}
	needsAck := m.T == relaylink.EdgeOpen || m.T == relaylink.EdgeRMC

	var err error
	if b.Call == nil {
		err = context.Canceled
	} else {
		raw, _ := json.Marshal(body)
		ctx, cancel := context.WithTimeout(context.Background(), b.timeout())
		_, err = b.Call(ctx, method, raw)
		cancel()
	}
	if err != nil && !needsAck {
		b.logf("wsc bridge: %s for PID=%d failed: %v", m.T, m.PID, err)
	}
	if needsAck {
		ack := relaylink.EdgePipeMsg{T: relaylink.EdgeAck, ID: m.ID}
		if err != nil {
			ack.Err = err.Error()
		}
		b.write(ack)
	}
}

// DeliverOut passes a wsc.out event from the main to the child.
func (b *WSCBridge) DeliverOut(body []byte) {
	var o relaylink.WSCOut
	if err := json.Unmarshal(body, &o); err != nil || o.PID == 0 {
		b.logf("wsc bridge: malformed wsc.out event")
		return
	}
	if !b.write(relaylink.EdgePipeMsg{T: relaylink.EdgeOut, PID: o.PID, Payload: o.Payload}) {
		b.logf("wsc bridge: message for PID=%d dropped: no edge running", o.PID)
	}
}

// StreamDown tells the child the stream to the main was lost. The main closes every one of
// this relay's sessions when that happens, so the child must drop them too.
func (b *WSCBridge) StreamDown() {
	b.write(relaylink.EdgePipeMsg{T: relaylink.EdgeReset})
}
