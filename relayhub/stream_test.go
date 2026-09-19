package relayhub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Happynico7504/relayd"
	"github.com/Happynico7504/relaylink"
)

type streamStack struct {
	hub    *StreamHub
	log    *InvalidationLog
	client *relaylink.Client
	addr   string
	ln     net.Listener
	opt    relaylink.StreamOptions
}

func newStreamStack(t *testing.T, opt relaylink.StreamOptions) *streamStack {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub, sk, _ := ed25519.GenerateKey(rand.Reader)
	srv := &relaylink.Server{Priv: priv, Replay: &relaylink.MemoryReplay{},
		RelayKey: func(id string) (ed25519.PublicKey, bool) { return pub, id == "us-1" || id == "jp-1" || id == "us-2" }}
	hub := NewStreamHub()
	log := NewInvalidationLog(100)
	log.OnAppend = hub.PushInvalidation
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeStream(ln, opt, hub.Handlers, hub.OnConn)
	t.Cleanup(func() { ln.Close() })
	return &streamStack{hub: hub, log: log, ln: ln, addr: ln.Addr().String(), opt: opt,
		client: &relaylink.Client{RelayID: "us-1", MainPub: &priv.PublicKey, Sign: sk}}
}

func (s *streamStack) asRelay(id string) *relaylink.Client {
	c := *s.client
	c.RelayID = id
	return &c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *streamStack) connect(t *testing.T, h relaylink.StreamHandlers) *relaylink.StreamConn {
	t.Helper()
	c, err := s.client.DialStream(context.Background(), s.addr, h, s.opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	waitFor(t, "hub to register the relay", func() bool { return len(s.hub.Status()) > 0 })
	return c
}

func call(t *testing.T, c *relaylink.StreamConn, method string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := c.Call(ctx, method, b); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

func TestPresenceAndRoutingDeliversToTheRightRelay(t *testing.T) {
	s := newStreamStack(t, relaylink.StreamOptions{})
	got := make(chan string, 4)
	relay := s.connect(t, relaylink.StreamHandlers{Event: func(_ *relaylink.StreamConn, topic string, body []byte) { got <- topic + ":" + string(body) }})
	call(t, relay, MethodPresAdd, map[string]any{"pids": []uint32{1435853600, 42}})

	if id, ok := s.hub.RelayFor(42); !ok || id != "us-1" {
		t.Fatalf("presence: %v %v", id, ok)
	}
	start := time.Now()
	if err := s.hub.Route(1435853600, relaylink.High, "nat.probe", []byte("target=42")); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m != "nat.probe:target=42" {
			t.Fatalf("got %q", m)
		}
		t.Logf("routed event delivered in %v", time.Since(start))
	case <-time.After(2 * time.Second):
		t.Fatal("event never arrived")
	}
	// Unknown player, and a player who left: an immediate, explicit error.
	if err := s.hub.Route(999, relaylink.High, "x", nil); err != ErrNotConnected {
		t.Fatalf("unknown pid: %v", err)
	}
	call(t, relay, MethodPresDel, map[string]any{"pids": []uint32{42}})
	if err := s.hub.Route(42, relaylink.High, "x", nil); err != ErrNotConnected {
		t.Fatalf("removed pid: %v", err)
	}
	if err := s.hub.Route(1435853600, relaylink.High, "x", []byte("y")); err != nil {
		t.Fatalf("remaining pid: %v", err)
	}
}

func TestRelayLossReportsItsPlayersAndClearsThem(t *testing.T) {
	s := newStreamStack(t, relaylink.StreamOptions{})
	type down struct {
		relay string
		pids  []uint32
	}
	ch := make(chan down, 2)
	s.hub.OnRelayDown = func(id string, pids []uint32) { ch <- down{id, pids} }
	relay := s.connect(t, relaylink.StreamHandlers{})
	call(t, relay, MethodPresAdd, map[string]any{"pids": []uint32{7, 3, 5}})
	relay.Close()
	select {
	case d := <-ch:
		if d.relay != "us-1" || len(d.pids) != 3 || d.pids[0] != 3 || d.pids[2] != 7 {
			t.Fatalf("down report: %+v", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay loss not reported")
	}
	if _, ok := s.hub.RelayFor(3); ok {
		t.Fatal("presence survived the loss of the relay")
	}
	if err := s.hub.Route(3, relaylink.High, "x", nil); err != ErrNotConnected {
		t.Fatalf("route after loss: %v", err)
	}
}

func TestReconnectReplacesTheOldStreamAndReconcilesPresence(t *testing.T) {
	s := newStreamStack(t, relaylink.StreamOptions{})
	gone := make(chan []uint32, 2)
	downs := make(chan string, 2)
	s.hub.OnPlayersGone = func(_ string, pids []uint32) { gone <- pids }
	s.hub.OnRelayDown = func(id string, _ []uint32) { downs <- id }

	first := s.connect(t, relaylink.StreamHandlers{})
	call(t, first, MethodPresSet, map[string]any{"pids": []uint32{1, 2, 3}})

	// The relay reconnects (its old TCP stream may still look alive to the hub).
	second, err := s.client.DialStream(context.Background(), s.addr, relaylink.StreamHandlers{}, s.opt)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	waitFor(t, "old stream to be closed by the hub", func() bool {
		select {
		case <-first.Done():
			return true
		default:
			return false
		}
	})
	select {
	case id := <-downs:
		t.Fatalf("the relay was reported DOWN although a new stream replaced the old one (%s)", id)
	case <-time.After(150 * time.Millisecond):
	}
	// Players survive the swap until the relay reconciles: 1 and 2 are still
	// there, 3 disconnected while the stream was down.
	if _, ok := s.hub.RelayFor(3); !ok {
		t.Fatal("presence was dropped by the swap itself")
	}
	call(t, second, MethodPresSet, map[string]any{"pids": []uint32{1, 2}})
	select {
	case pids := <-gone:
		if len(pids) != 1 || pids[0] != 3 {
			t.Fatalf("gone: %v", pids)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("players missing after the reconnect were not reported")
	}
	if _, ok := s.hub.RelayFor(3); ok {
		t.Fatal("player 3 still present")
	}
	if err := s.hub.Route(1, relaylink.High, "x", nil); err != nil {
		t.Fatalf("route via the new stream: %v", err)
	}
}

func TestStaleStreamCannotChangePresence(t *testing.T) {
	s := newStreamStack(t, relaylink.StreamOptions{})
	old := s.connect(t, relaylink.StreamHandlers{})
	fresh, _ := s.client.DialStream(context.Background(), s.addr, relaylink.StreamHandlers{}, s.opt)
	defer fresh.Close()
	waitFor(t, "swap", func() bool {
		select {
		case <-old.Done():
			return true
		default:
			return false
		}
	})
	call(t, fresh, MethodPresSet, map[string]any{"pids": []uint32{10}})
	// The old stream is closed; even a late message from it changes nothing.
	s.hub.setPresence(old, []uint32{99}, true)
	if _, ok := s.hub.RelayFor(99); ok {
		t.Fatal("a superseded stream modified the presence table")
	}
}

func TestSubscribeBroadcastAndCallRelay(t *testing.T) {
	s := newStreamStack(t, relaylink.StreamOptions{})
	events := make(chan string, 4)
	s.hub.Subscribe("match.update", func(relayID string, body []byte) { events <- relayID + "/" + string(body) })
	callbacks := make(chan string, 4)
	relay := s.connect(t, relaylink.StreamHandlers{
		Event: func(_ *relaylink.StreamConn, topic string, body []byte) { callbacks <- topic },
		Call: func(_ context.Context, _ *relaylink.StreamConn, method string, body []byte) ([]byte, error) {
			return []byte("relay says " + method), nil
		},
	})
	relay.Send(relaylink.High, "match.update", []byte("gid=7"))
	select {
	case e := <-events:
		if e != "us-1/gid=7" {
			t.Fatalf("subscriber got %q", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber never called")
	}
	if n := s.hub.Broadcast(relaylink.Normal, "hello", nil); n != 1 {
		t.Fatalf("broadcast reached %d relays", n)
	}
	if got := <-callbacks; got != "hello" {
		t.Fatalf("broadcast delivered %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if out, err := s.hub.CallRelay(ctx, "us-1", "ping", nil); err != nil || string(out) != "relay says ping" {
		t.Fatalf("CallRelay: %q %v", out, err)
	}
	if _, err := s.hub.CallRelay(ctx, "jp-1", "ping", nil); err != ErrNotConnected {
		t.Fatalf("call to an unconnected relay: %v", err)
	}
	if err := s.hub.SendToRelay("jp-1", relaylink.Normal, "x", nil); err != ErrNotConnected {
		t.Fatalf("send to an unconnected relay: %v", err)
	}
}

func TestStatusShowsRTTAndPlayers(t *testing.T) {
	s := newStreamStack(t, relaylink.StreamOptions{HeartbeatEvery: 30 * time.Millisecond, DeadAfter: time.Second})
	relay := s.connect(t, relaylink.StreamHandlers{})
	call(t, relay, MethodPresAdd, map[string]any{"pids": []uint32{1, 2}})
	waitFor(t, "an RTT measurement", func() bool { st := s.hub.Status(); return len(st) == 1 && st[0].RTT > 0 })
	st := s.hub.Status()[0]
	if st.ID != "us-1" || st.Players != 2 || st.RTT > 50*time.Millisecond || st.Since.IsZero() {
		t.Fatalf("status: %+v", st)
	}
}

func TestPresenceRequestsAreValidated(t *testing.T) {
	s := newStreamStack(t, relaylink.StreamOptions{})
	relay := s.connect(t, relaylink.StreamHandlers{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := relay.Call(ctx, MethodPresAdd, []byte("not json")); err == nil {
		t.Fatal("garbage presence request accepted")
	}
	if _, err := relay.Call(ctx, "no.such.method", nil); err == nil {
		t.Fatal("unknown method accepted")
	}
	huge := make([]uint32, 70000)
	b, _ := json.Marshal(map[string]any{"pids": huge})
	if _, err := relay.Call(ctx, MethodPresSet, b); err == nil {
		t.Fatal("absurdly large presence set accepted")
	}
	if out, err := relay.Call(ctx, MethodEcho, []byte("hi")); err != nil || string(out) != "hi" {
		t.Fatalf("echo: %q %v", out, err)
	}
}

// The point of the whole exercise: a change on the main reaches a relay's cache
// in about one network hop, with the slow poll switched off.
func TestInvalidationIsPushedInstantlyToTheRelayCache(t *testing.T) {
	s := newStreamStack(t, relaylink.StreamOptions{})
	store := &relaylink.MemoryStore{}
	var fetcher *relaylink.Fetcher
	fetcher = &relaylink.Fetcher{Client: s.client, Store: store, MaxStale: time.Hour}
	// Prime the fetcher's epoch/sequence the way the first poll would.
	batch := s.log.Batch(0, "")
	fetcher.ApplyBatchForTest(batch)
	store.Set("GET /x", &relaylink.Response{Status: 200, Tags: []string{"pid:1"}}, time.Hour)

	applied := make(chan bool, 1)
	relay := s.connect(t, relaylink.StreamHandlers{Event: func(_ *relaylink.StreamConn, topic string, body []byte) {
		if topic != TopicInvalidate {
			return
		}
		var m struct {
			Epoch string   `json:"epoch"`
			Seq   int64    `json:"seq"`
			Tags  []string `json:"tags"`
		}
		json.Unmarshal(body, &m)
		applied <- fetcher.ApplyPushed(m.Epoch, relaylink.Event{Seq: m.Seq, Tags: m.Tags})
	}})
	_ = relay

	start := time.Now()
	s.log.Append([]string{"pid:1"})
	select {
	case ok := <-applied:
		if !ok {
			t.Fatal("the pushed invalidation was not applied")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no push arrived")
	}
	if _, cached := store.Get("GET /x"); cached {
		t.Fatal("the cached entry survived the pushed invalidation")
	}
	t.Logf("cache entry invalidated %v after the change (no polling)", time.Since(start))
}

func TestStreamClientReconnectsAndReannouncesPlayers(t *testing.T) {
	s := newStreamStack(t, relaylink.StreamOptions{})
	var mu sync.Mutex
	ups := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc := &relayd.StreamClient{
		Client: s.client, Addr: s.addr, MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
		OnUp: func(c *relaylink.StreamConn) {
			mu.Lock()
			ups++
			mu.Unlock()
			b, _ := json.Marshal(map[string]any{"pids": []uint32{5, 6}})
			go c.Call(context.Background(), MethodPresSet, b) // what a relay does on every (re)connect
		},
	}
	go sc.Run(ctx)
	waitFor(t, "first connection with presence", func() bool { _, ok := s.hub.RelayFor(5); return ok })

	// Kill the stream from the hub's side: the relay must come back by itself.
	s.hub.mu.Lock()
	victim := s.hub.conns["us-1"]
	s.hub.mu.Unlock()
	victim.Close()
	waitFor(t, "presence to be cleared by the loss", func() bool { _, ok := s.hub.RelayFor(5); return !ok })
	waitFor(t, "automatic reconnect and re-announce", func() bool { _, ok := s.hub.RelayFor(5); return ok })
	mu.Lock()
	n := ups
	mu.Unlock()
	if n < 2 {
		t.Fatalf("OnUp called %d times", n)
	}
	// While down, sends fail fast instead of blocking the relay's game logic.
	sc2 := &relayd.StreamClient{Client: s.client, Addr: "127.0.0.1:1"}
	start := time.Now()
	if err := sc2.Send(relaylink.High, "x", nil); err == nil || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("send while down: %v after %v", err, time.Since(start))
	}
	if _, err := sc2.Call(context.Background(), "x", nil); err == nil {
		t.Fatal("call while down succeeded")
	}
}

func TestStreamAddrDerivedFromTheBundleURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://netcup-server.nicochristmann.net:7777": "netcup-server.nicochristmann.net:7778",
		"http://127.0.0.1:9490":                        "127.0.0.1:7778",
		"https://main.example":                         "main.example:7778",
	} {
		if got, err := relayd.StreamAddrFromURL(in); err != nil || got != want {
			t.Errorf("%s -> %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := relayd.StreamAddrFromURL("::not a url"); err == nil {
		t.Error("garbage URL accepted")
	}
}
