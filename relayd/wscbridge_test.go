package relayd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Happynico7504/relaylink"
)

type bridgeRig struct {
	b        *WSCBridge
	toEdge   *io.PipeWriter // the child's messages to the bridge
	fromEdge *bufio.Scanner // what the bridge writes to the child
	mu       sync.Mutex
	calls    []call
	fail     map[string]error
	done     chan struct{}
}

type call struct {
	method string
	body   []byte
}

func newBridgeRig(t *testing.T) *bridgeRig {
	t.Helper()
	r := &bridgeRig{fail: map[string]error{}, done: make(chan struct{})}
	childToParentR, childToParentW := io.Pipe()
	parentToChildR, parentToChildW := io.Pipe()
	r.toEdge = childToParentW
	r.fromEdge = bufio.NewScanner(parentToChildR)
	r.b = &WSCBridge{Logf: func(string, ...any) {}, CallTimeout: time.Second}
	r.b.Call = func(_ context.Context, method string, body []byte) ([]byte, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, call{method, body})
		return nil, r.fail[method]
	}
	go func() { r.b.Attach(childToParentR, parentToChildW); close(r.done) }()
	// Attach registers the child's writer on its own goroutine: wait for it, or an early
	// DeliverOut would be (correctly) dropped as "no edge running".
	for i := 0; ; i++ {
		r.b.mu.Lock()
		attached := r.b.enc != nil
		r.b.mu.Unlock()
		if attached {
			break
		}
		if i > 500 {
			t.Fatal("the bridge never attached")
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Cleanup(func() { childToParentW.Close(); parentToChildW.Close() })
	return r
}

func (r *bridgeRig) send(m relaylink.EdgePipeMsg) {
	b, _ := json.Marshal(m)
	r.toEdge.Write(append(b, '\n'))
}

// next reads the next line the bridge wrote to the child.
func (r *bridgeRig) next(t *testing.T) relaylink.EdgePipeMsg {
	t.Helper()
	ch := make(chan relaylink.EdgePipeMsg, 1)
	go func() {
		if r.fromEdge.Scan() {
			var m relaylink.EdgePipeMsg
			json.Unmarshal(r.fromEdge.Bytes(), &m)
			ch <- m
		}
	}()
	select {
	case m := <-ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("nothing arrived at the child")
		return relaylink.EdgePipeMsg{}
	}
}

func (r *bridgeRig) called(method string) *call {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.calls {
		if r.calls[i].method == method {
			return &r.calls[i]
		}
	}
	return nil
}

func TestBridgeTranslatesEdgeMessagesToStreamCalls(t *testing.T) {
	r := newBridgeRig(t)

	r.send(relaylink.EdgePipeMsg{T: relaylink.EdgeOpen, ID: 1, PID: 42, IP: "1.2.3.4", Port: 5555})
	if ack := r.next(t); ack.T != relaylink.EdgeAck || ack.ID != 1 || ack.Err != "" {
		t.Fatalf("open ack: %+v", ack)
	}
	var open relaylink.WSCOpen
	json.Unmarshal(r.called(relaylink.MethodWSCOpen).body, &open)
	if open != (relaylink.WSCOpen{PID: 42, IP: "1.2.3.4", Port: 5555}) {
		t.Fatalf("open call: %+v", open)
	}

	r.send(relaylink.EdgePipeMsg{T: relaylink.EdgeRMC, ID: 2, PID: 42, Call: 9, Proto: 0x7f, Custom: 0x83, Method: 3, Params: []byte{7, 8}})
	if ack := r.next(t); ack.ID != 2 || ack.Err != "" {
		t.Fatalf("rmc ack: %+v", ack)
	}
	var rmc relaylink.WSCRMC
	json.Unmarshal(r.called(relaylink.MethodWSCRMC).body, &rmc)
	if rmc.PID != 42 || rmc.Call != 9 || rmc.Proto != 0x7f || rmc.Custom != 0x83 || rmc.Method != 3 || string(rmc.Params) != "\x07\x08" {
		t.Fatalf("rmc call: %+v", rmc)
	}

	// Alive and close are fire-and-forget: they reach the main but nothing is acked.
	r.send(relaylink.EdgePipeMsg{T: relaylink.EdgeAlive, PIDs: []uint32{42, 43}})
	r.send(relaylink.EdgePipeMsg{T: relaylink.EdgeClose, PID: 42})
	waitFor(t, "alive and close calls", func() bool {
		return r.called(relaylink.MethodWSCAlive) != nil && r.called(relaylink.MethodWSCClose) != nil
	})
}

func TestBridgeAcksFailuresSoTheEdgeCanRefuseTheConnect(t *testing.T) {
	r := newBridgeRig(t)
	r.mu.Lock()
	r.fail[relaylink.MethodWSCOpen] = errors.New("main refused open: 409")
	r.mu.Unlock()
	r.send(relaylink.EdgePipeMsg{T: relaylink.EdgeOpen, ID: 5, PID: 1, IP: "1.1.1.1", Port: 1})
	ack := r.next(t)
	if ack.ID != 5 || !strings.Contains(ack.Err, "refused") {
		t.Fatalf("ack: %+v", ack)
	}
}

func TestBridgeAcksWhenTheStreamIsDown(t *testing.T) {
	r := newBridgeRig(t)
	r.b.Call = func(context.Context, string, []byte) ([]byte, error) { return nil, relaylink.ErrStreamClosed }
	r.send(relaylink.EdgePipeMsg{T: relaylink.EdgeRMC, ID: 8, PID: 1})
	if ack := r.next(t); ack.ID != 8 || ack.Err == "" {
		t.Fatalf("ack: %+v", ack)
	}
}

func TestBridgeDeliversMainMessagesToTheEdge(t *testing.T) {
	r := newBridgeRig(t)
	body, _ := json.Marshal(relaylink.WSCOut{PID: 42, Payload: []byte("rmc bytes")})
	go r.b.DeliverOut(body)
	m := r.next(t)
	if m.T != relaylink.EdgeOut || m.PID != 42 || string(m.Payload) != "rmc bytes" {
		t.Fatalf("out: %+v", m)
	}
	go r.b.StreamDown()
	if m := r.next(t); m.T != relaylink.EdgeReset {
		t.Fatalf("reset: %+v", m)
	}
}

func TestBridgeDropsMessagesWhenNoEdgeRuns(t *testing.T) {
	var logged []string
	b := &WSCBridge{Logf: func(f string, a ...any) { logged = append(logged, f) }}
	body, _ := json.Marshal(relaylink.WSCOut{PID: 1, Payload: []byte("x")})
	b.DeliverOut(body) // must not panic or block
	b.StreamDown()
	b.DeliverOut([]byte("not json"))
	if len(logged) == 0 {
		t.Fatal("dropped messages were not logged")
	}
}

func TestBridgeStopsWritingOnceTheChildIsGone(t *testing.T) {
	r := newBridgeRig(t)
	r.toEdge.Close() // the child's end closes
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("Attach did not return when the child went away")
	}
	body, _ := json.Marshal(relaylink.WSCOut{PID: 1, Payload: []byte("x")})
	r.b.DeliverOut(body) // no child: dropped, no panic, no block
}

// The supervisor gives the child descriptors 3 and 4 and hands the parent's ends to Pipe.
func TestSupervisorPipesReachTheChild(t *testing.T) {
	// A child that echoes descriptor 3 into descriptor 4.
	echo := []byte("#!/bin/sh\nif [ \"$1\" = \"-version\" ]; then echo \"wscedge 1\"; exit 0; fi\nexec cat <&3 >&4\n")
	r := newCompRig(t, 0, echo)
	got := make(chan string, 1)
	r.sup.Pipe = func(from io.Reader, to io.Writer) {
		io.WriteString(to, "hello child\n")
		line, _ := bufio.NewReader(from).ReadString('\n')
		got <- strings.TrimSpace(line)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.sup.Run(ctx)
	select {
	case line := <-got:
		if line != "hello child" {
			t.Fatalf("echoed %q", line)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the child never answered on the pipe")
	}
}

func TestBridgeForwardsStatsWithoutAnAck(t *testing.T) {
	r := newBridgeRig(t)
	r.send(relaylink.EdgePipeMsg{T: relaylink.EdgeStats, PID: 7, Stats: &relaylink.WSCStats{PID: 7, IP: "1.2.3.4", Loss: "0%", AvgRTT: "5ms", PRUDPRTTMs: 12.5, Samples: 3}})
	waitFor(t, "stats call", func() bool { return r.called(relaylink.MethodWSCStats) != nil })
	var s relaylink.WSCStats
	json.Unmarshal(r.called(relaylink.MethodWSCStats).body, &s)
	if s.PID != 7 || s.IP != "1.2.3.4" || s.PRUDPRTTMs != 12.5 || s.Samples != 3 || s.Loss != "0%" {
		t.Fatalf("stats call: %+v", s)
	}
	// A stats line without a payload is ignored, not forwarded.
	r.send(relaylink.EdgePipeMsg{T: relaylink.EdgeStats, PID: 8})
	time.Sleep(100 * time.Millisecond)
	r.mu.Lock()
	n := 0
	for _, c := range r.calls {
		if c.method == relaylink.MethodWSCStats {
			n++
		}
	}
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d stats calls, want 1", n)
	}
}

func TestBridgeForwardsTracerouteOutput(t *testing.T) {
	r := newBridgeRig(t)
	r.send(relaylink.EdgePipeMsg{T: relaylink.EdgeTrace, PID: 7, Trace: &relaylink.WSCTrace{PID: 7, IP: "1.2.3.4", Reason: "connect", Output: "traceroute to 1.2.3.4\n 1  a  1 ms\n"}})
	waitFor(t, "trace call", func() bool { return r.called(relaylink.MethodWSCTrace) != nil })
	var tr relaylink.WSCTrace
	json.Unmarshal(r.called(relaylink.MethodWSCTrace).body, &tr)
	if tr.PID != 7 || tr.Reason != "connect" || !strings.Contains(tr.Output, "1  a  1 ms") {
		t.Fatalf("trace call: %+v", tr)
	}
}

// Wii U Chat's bridge speaks the same pipe protocol but calls "wuc.*" methods, so both edges can share
// one relay and one hub without their calls or players mixing.
func TestChatBridgeCallsItsOwnMethods(t *testing.T) {
	r := newBridgeRig(t)
	r.b.Names = relaylink.WUCEdgeNames
	r.send(relaylink.EdgePipeMsg{T: relaylink.EdgeOpen, ID: 1, PID: 42, IP: "1.2.3.4", Port: 5})
	if ack := r.next(t); ack.Err != "" {
		t.Fatalf("ack: %+v", ack)
	}
	r.send(relaylink.EdgePipeMsg{T: relaylink.EdgeRMC, ID: 2, PID: 42, Call: 1, Proto: 0x1234, Method: 3})
	r.next(t)
	if r.called(relaylink.WUCEdgeNames.Open) == nil || r.called(relaylink.WUCEdgeNames.RMC) == nil {
		t.Fatal("the chat bridge did not call wuc.open / wuc.rmc")
	}
	if r.called(relaylink.MethodWSCOpen) != nil || r.called(relaylink.MethodWSCRMC) != nil {
		t.Fatal("the chat bridge called WSC's methods")
	}
	// A 16-bit protocol id (Wii U Chat's nex-go v2) survives the trip.
	var rmc relaylink.WSCRMC
	json.Unmarshal(r.called(relaylink.WUCEdgeNames.RMC).body, &rmc)
	if rmc.Proto != 0x1234 {
		t.Fatalf("protocol id %#x", rmc.Proto)
	}
}

func TestKnownEdgesAreDistinct(t *testing.T) {
	w, c := KnownEdges["wscedge"], KnownEdges["wiiuchatedge"]
	if w.Variant.Name == c.Variant.Name || w.Names.Open == c.Names.Open || w.SecretEnv == c.SecretEnv {
		t.Fatalf("edges overlap: %+v vs %+v", w, c)
	}
	if w.Variant.Base != "wsc" || c.Variant.Base != "wiiu-chat" || c.SecretEnv != "PN_WUC_KERBEROS_PASSWORD" {
		t.Fatalf("edge specs: %+v %+v", w, c)
	}
}
