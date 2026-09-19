package relayhub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Happynico7504/relaylink"
)

// fakeMain stands in for wsc-secure's edge endpoint.
type fakeMain struct {
	mu     sync.Mutex
	posts  []fakePost
	status map[string]int // op -> forced status
}

type fakePost struct {
	Op   string
	Body map[string]any
}

func (f *fakeMain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	op := strings.TrimPrefix(r.URL.Path, "/edge/")
	f.mu.Lock()
	f.posts = append(f.posts, fakePost{op, m})
	code := f.status[op]
	f.mu.Unlock()
	if code == 0 {
		code = http.StatusNoContent
	}
	w.WriteHeader(code)
}

func (f *fakeMain) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.posts {
		out = append(out, p.Op)
	}
	return out
}

func (f *fakeMain) last(op string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.posts) - 1; i >= 0; i-- {
		if f.posts[i].Op == op {
			return f.posts[i].Body
		}
	}
	return nil
}

type edgeRig struct {
	st     *streamStack
	main   *fakeMain
	bridge *EdgeBridge
	out    *httptest.Server // the hub's /edge/out
	conn   *relaylink.StreamConn
	mu     sync.Mutex
	events []relaylink.WSCOut
}

func newEdgeRig(t *testing.T) *edgeRig {
	t.Helper()
	r := &edgeRig{st: newStreamStack(t, relaylink.StreamOptions{}), main: &fakeMain{status: map[string]int{}}}
	ms := httptest.NewServer(r.main)
	t.Cleanup(ms.Close)
	r.bridge = &EdgeBridge{Streams: r.st.hub, Main: ms.URL}
	r.bridge.Register()
	r.out = httptest.NewServer(http.HandlerFunc(r.bridge.Out))
	t.Cleanup(r.out.Close)
	r.conn = r.st.connect(t, relaylink.StreamHandlers{
		Event: func(_ *relaylink.StreamConn, topic string, body []byte) {
			if topic == relaylink.TopicWSCOut {
				var o relaylink.WSCOut
				json.Unmarshal(body, &o)
				r.mu.Lock()
				r.events = append(r.events, o)
				r.mu.Unlock()
			}
		},
	})
	return r
}

func (r *edgeRig) callErr(method string, v any) error {
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := r.conn.Call(ctx, method, b)
	return err
}

func (r *edgeRig) postOut(pid uint32, payload []byte) int {
	b, _ := json.Marshal(map[string]any{"relay": "us-1", "pid": pid, "payload": payload})
	resp, err := http.Post(r.out.URL, "application/json", bytes.NewReader(b))
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestEdgeBridgeSessionLifecycle(t *testing.T) {
	r := newEdgeRig(t)

	// A relay cannot claim another relay's identity: the hub overwrites "relay".
	if err := r.callErr(relaylink.MethodWSCOpen, map[string]any{"pid": 42, "ip": "1.2.3.4", "port": 5555, "relay": "evil"}); err != nil {
		t.Fatal(err)
	}
	open := r.main.last("open")
	if open["relay"] != "us-1" || open["pid"] != float64(42) || open["ip"] != "1.2.3.4" {
		t.Fatalf("open reached the main as %v", open)
	}
	if id, ok := r.bridge.RelayFor(42); !ok || id != "us-1" {
		t.Fatalf("presence: %q %v", id, ok)
	}

	// An RMC call is forwarded with its parameters intact (base64 passes through untouched).
	if err := r.callErr(relaylink.MethodWSCRMC, relaylink.WSCRMC{PID: 42, Call: 7, Proto: 0x6d, Method: 0x2b, Params: []byte{1, 2, 3, 250}}); err != nil {
		t.Fatal(err)
	}
	rmc := r.main.last("rmc")
	if rmc["relay"] != "us-1" || rmc["call"] != float64(7) || rmc["proto"] != float64(0x6d) || rmc["method"] != float64(0x2b) {
		t.Fatalf("rmc reached the main as %v", rmc)
	}
	if rmc["params"] != "AQID+g==" {
		t.Fatalf("params changed in transit: %v", rmc["params"])
	}

	// Liveness reports need no pid.
	if err := r.callErr(relaylink.MethodWSCAlive, relaylink.WSCAlive{PIDs: []uint32{42}}); err != nil {
		t.Fatal(err)
	}

	// The main sends something to the player: it arrives at that player's relay.
	if code := r.postOut(42, []byte("rmc-response")); code != http.StatusNoContent {
		t.Fatalf("out: %d", code)
	}
	waitFor(t, "event at the relay", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.events) == 1 })
	if r.events[0].PID != 42 || string(r.events[0].Payload) != "rmc-response" {
		t.Fatalf("event: %+v", r.events[0])
	}
	// ... and for a player nobody holds, the main is told at once.
	if code := r.postOut(99, []byte("x")); code != http.StatusNotFound {
		t.Fatalf("out for an unknown player: %d", code)
	}

	// Close removes presence.
	if err := r.callErr(relaylink.MethodWSCClose, relaylink.WSCClose{PID: 42}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.bridge.RelayFor(42); ok {
		t.Fatal("player still present after close")
	}
	if code := r.postOut(42, []byte("late")); code != http.StatusNotFound {
		t.Fatalf("out after close: %d", code)
	}
}

func TestEdgeBridgeRefusedOpenLeavesNoPresence(t *testing.T) {
	r := newEdgeRig(t)
	r.main.mu.Lock()
	r.main.status["open"] = http.StatusConflict
	r.main.mu.Unlock()
	if err := r.callErr(relaylink.MethodWSCOpen, relaylink.WSCOpen{PID: 5, IP: "1.1.1.1", Port: 1}); err == nil {
		t.Fatal("open succeeded although the main refused it")
	}
	if _, ok := r.bridge.RelayFor(5); ok {
		t.Fatal("presence recorded for a refused session")
	}
}

func TestEdgeBridgeRejectsMalformedCalls(t *testing.T) {
	r := newEdgeRig(t)
	for name, v := range map[string]any{"no pid": map[string]any{"ip": "1.1.1.1"}, "zero pid": map[string]any{"pid": 0}} {
		if err := r.callErr(relaylink.MethodWSCRMC, v); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if len(r.main.ops()) != 0 {
		t.Fatalf("malformed calls reached the main: %v", r.main.ops())
	}
}

func TestEdgeBridgeClosesAllPlayersWhenTheRelayDrops(t *testing.T) {
	r := newEdgeRig(t)
	for _, pid := range []uint32{10, 11, 12} {
		if err := r.callErr(relaylink.MethodWSCOpen, relaylink.WSCOpen{PID: pid, IP: "9.9.9.9", Port: int(pid)}); err != nil {
			t.Fatal(err)
		}
	}
	r.conn.Close()
	waitFor(t, "close for every player", func() bool {
		n := 0
		for _, op := range r.main.ops() {
			if op == "close" {
				n++
			}
		}
		return n == 3
	})
	for _, pid := range []uint32{10, 11, 12} {
		if _, ok := r.bridge.RelayFor(pid); ok {
			t.Fatalf("pid %d still present after the relay dropped", pid)
		}
	}
}

func TestEdgeBridgeReportsAnUnreachableMain(t *testing.T) {
	st := newStreamStack(t, relaylink.StreamOptions{})
	(&EdgeBridge{Streams: st.hub, Main: "http://127.0.0.1:1"}).Register() // nothing listens there
	conn := st.connect(t, relaylink.StreamHandlers{})
	b, _ := json.Marshal(relaylink.WSCOpen{PID: 3, IP: "1.1.1.1", Port: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := conn.Call(ctx, relaylink.MethodWSCOpen, b); err == nil {
		t.Fatal("open succeeded with no main")
	}
	if _, ok := st.hub.RelayFor(3); ok {
		t.Fatal("presence recorded without a main")
	}
}

func TestEdgeBridgeForwardsStatsToTheMain(t *testing.T) {
	r := newEdgeRig(t)
	if err := r.callErr(relaylink.MethodWSCStats, relaylink.WSCStats{PID: 42, IP: "1.2.3.4", Loss: "0%", AvgRTT: "9ms", PRUDPRTTMs: 20.5, PRUDPMinMs: 18, Samples: 4}); err != nil {
		t.Fatal(err)
	}
	m := r.main.last("stats")
	if m["relay"] != "us-1" || m["pid"] != float64(42) || m["prudp_rtt_ms"] != 20.5 || m["samples"] != float64(4) || m["loss"] != "0%" {
		t.Fatalf("stats reached the main as %v", m)
	}
	if err := r.callErr(relaylink.MethodWSCStats, map[string]any{"loss": "0%"}); err == nil {
		t.Fatal("stats without a pid accepted")
	}
}

func TestEdgeBridgeForwardsTracerouteToTheMain(t *testing.T) {
	r := newEdgeRig(t)
	if err := r.callErr(relaylink.MethodWSCTrace, relaylink.WSCTrace{PID: 42, IP: "1.2.3.4", Reason: "loss=40%", Output: "traceroute to 1.2.3.4\n"}); err != nil {
		t.Fatal(err)
	}
	m := r.main.last("trace")
	if m["relay"] != "us-1" || m["pid"] != float64(42) || m["reason"] != "loss=40%" || m["output"] != "traceroute to 1.2.3.4\n" {
		t.Fatalf("trace reached the main as %v", m)
	}
}

// One player can be in WSC and Wii U Chat at the same time (through different relays even): each
// edge's bridge keeps its own session table, so closing one session never removes the other.
func TestTwoEdgeBridgesKeepIndependentSessionsForTheSamePlayer(t *testing.T) {
	st := newStreamStack(t, relaylink.StreamOptions{})
	wsc, chat := &fakeMain{status: map[string]int{}}, &fakeMain{status: map[string]int{}}
	wscSrv, chatSrv := httptest.NewServer(wsc), httptest.NewServer(chat)
	t.Cleanup(wscSrv.Close)
	t.Cleanup(chatSrv.Close)
	wb := &EdgeBridge{Streams: st.hub, Main: wscSrv.URL}
	cb := &EdgeBridge{Streams: st.hub, Main: chatSrv.URL, Names: relaylink.WUCEdgeNames}
	wb.Register()
	cb.Register()
	conn := st.connect(t, relaylink.StreamHandlers{})
	call := func(method string, v any) error {
		b, _ := json.Marshal(v)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := conn.Call(ctx, method, b)
		return err
	}

	if err := call(relaylink.WSCEdgeNames.Open, relaylink.WSCOpen{PID: 42, IP: "1.1.1.1", Port: 1}); err != nil {
		t.Fatal(err)
	}
	if err := call(relaylink.WUCEdgeNames.Open, relaylink.WSCOpen{PID: 42, IP: "1.1.1.1", Port: 2}); err != nil {
		t.Fatal(err)
	}
	// Each open reached only its own game server.
	if got := wsc.last("open")["port"]; got != float64(1) {
		t.Fatalf("wsc server saw %v", got)
	}
	if got := chat.last("open")["port"]; got != float64(2) {
		t.Fatalf("chat server saw %v", got)
	}
	if err := call(relaylink.WUCEdgeNames.Close, relaylink.WSCClose{PID: 42}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cb.RelayFor(42); ok {
		t.Fatal("chat session still recorded after its close")
	}
	if id, ok := wb.RelayFor(42); !ok || id != "us-1" {
		t.Fatalf("closing the chat session removed the WSC one (%q %v)", id, ok)
	}
}
