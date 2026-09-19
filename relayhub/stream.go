package relayhub

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/Happynico7504/relaylink"
)

// ErrNotConnected means no relay currently holds the target (or is connected).
var ErrNotConnected = errors.New("relayhub: target is not connected through any relay")

// Topics and methods of the real-time stream.
const (
	TopicInvalidate = "invalidate"      // hub -> relay: {"epoch","seq","tags"}
	MethodEcho      = "echo"            // either way: returns the body (latency probes)
	MethodPresAdd   = "presence.add"    // relay -> hub: {"pids":[...]}
	MethodPresDel   = "presence.remove" // relay -> hub: {"pids":[...]}
	MethodPresSet   = "presence.set"    // relay -> hub: {"pids":[...]} replaces the relay's whole set (send after every (re)connect)
)

// StreamHub is the main's side of the real-time streams: one authenticated
// stream per relay, a table of which relay each player is connected through,
// and routing of events to the right relay.
type StreamHub struct {
	// OnRelayDown is called when a relay's stream ends, with the players that
	// were connected through it. The game logic uses it to disconnect them.
	OnRelayDown func(relayID string, pids []uint32)
	// OnRelayUp is called when a relay (re)connects.
	OnRelayUp func(relayID string)
	// OnPlayersGone is called when a relay's presence set shrinks without the
	// relay going away (presence.set after a reconnect, or presence.remove is NOT
	// reported here: the relay itself said so).
	OnPlayersGone func(relayID string, pids []uint32)

	// Methods lets other components (e.g. the NEX assigner) serve extra calls from
	// relays. The relay id is the authenticated caller.
	Methods map[string]func(ctx context.Context, relayID string, body []byte) ([]byte, error)

	mu       sync.Mutex
	conns    map[string]*relaylink.StreamConn
	since    map[string]time.Time
	presence map[uint32]string
	byRelay  map[string]map[uint32]struct{}
	subs     map[string][]func(relayID string, body []byte)
}

func NewStreamHub() *StreamHub {
	return &StreamHub{
		conns: map[string]*relaylink.StreamConn{}, since: map[string]time.Time{},
		presence: map[uint32]string{}, byRelay: map[string]map[uint32]struct{}{},
		subs: map[string][]func(string, []byte){},
	}
}

// Handlers returns the per-relay handlers passed to Server.ServeStream.
func (h *StreamHub) Handlers(relayID string) relaylink.StreamHandlers {
	return relaylink.StreamHandlers{
		Call: func(ctx context.Context, c *relaylink.StreamConn, method string, body []byte) ([]byte, error) {
			switch method {
			case MethodEcho:
				return body, nil
			case MethodPresAdd, MethodPresDel, MethodPresSet:
				var req struct {
					PIDs []uint32 `json:"pids"`
				}
				if err := json.Unmarshal(body, &req); err != nil || len(req.PIDs) > 65536 {
					return nil, errors.New("bad request")
				}
				if method == MethodPresSet {
					h.replacePresence(c, req.PIDs)
				} else {
					h.setPresence(c, req.PIDs, method == MethodPresAdd)
				}
				return []byte("ok"), nil
			}
			h.mu.Lock()
			fn := h.Methods[method]
			h.mu.Unlock()
			if fn != nil {
				return fn(ctx, c.RelayID, body)
			}
			return nil, errors.New("unknown method")
		},
		Event: func(c *relaylink.StreamConn, topic string, body []byte) {
			h.mu.Lock()
			fns := append([]func(string, []byte){}, h.subs[topic]...)
			h.mu.Unlock()
			for _, fn := range fns {
				fn(c.RelayID, body)
			}
		},
		Closed: func(c *relaylink.StreamConn, _ error) { h.dropConn(c) },
	}
}

// OnConn registers a freshly handshaken stream. A relay has exactly one: a new
// connection replaces (and closes) the old one, whose players are reported down
// unless the relay re-registers them.
func (h *StreamHub) OnConn(c *relaylink.StreamConn) {
	h.mu.Lock()
	old := h.conns[c.RelayID]
	h.conns[c.RelayID] = c
	h.since[c.RelayID] = time.Now()
	up := h.OnRelayUp
	h.mu.Unlock()
	if old != nil {
		old.Close() // its Closed handler sees it is no longer current
	}
	if up != nil {
		up(c.RelayID)
	}
}

func (h *StreamHub) setPresence(c *relaylink.StreamConn, pids []uint32, add bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[c.RelayID] != c {
		return // a stale connection must not touch the table
	}
	set := h.byRelay[c.RelayID]
	if set == nil {
		set = map[uint32]struct{}{}
		h.byRelay[c.RelayID] = set
	}
	for _, pid := range pids {
		if add {
			h.presence[pid] = c.RelayID
			set[pid] = struct{}{}
		} else if h.presence[pid] == c.RelayID {
			delete(h.presence, pid)
			delete(set, pid)
		}
	}
}

func (h *StreamHub) replacePresence(c *relaylink.StreamConn, pids []uint32) {
	h.mu.Lock()
	if h.conns[c.RelayID] != c {
		h.mu.Unlock()
		return
	}
	want := make(map[uint32]struct{}, len(pids))
	for _, p := range pids {
		want[p] = struct{}{}
	}
	var gone []uint32
	for pid := range h.byRelay[c.RelayID] {
		if _, keep := want[pid]; !keep {
			if h.presence[pid] == c.RelayID {
				delete(h.presence, pid)
			}
			gone = append(gone, pid)
		}
	}
	for pid := range want {
		h.presence[pid] = c.RelayID
	}
	h.byRelay[c.RelayID] = want
	cb := h.OnPlayersGone
	h.mu.Unlock()
	if cb != nil && len(gone) > 0 {
		sort.Slice(gone, func(i, j int) bool { return gone[i] < gone[j] })
		cb(c.RelayID, gone)
	}
}

func (h *StreamHub) dropConn(c *relaylink.StreamConn) {
	h.mu.Lock()
	if h.conns[c.RelayID] != c { // already replaced by a newer connection
		h.mu.Unlock()
		return
	}
	delete(h.conns, c.RelayID)
	delete(h.since, c.RelayID)
	var pids []uint32
	for pid := range h.byRelay[c.RelayID] {
		if h.presence[pid] == c.RelayID {
			delete(h.presence, pid)
			pids = append(pids, pid)
		}
	}
	delete(h.byRelay, c.RelayID)
	down := h.OnRelayDown
	h.mu.Unlock()
	if down != nil {
		sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
		down(c.RelayID, pids)
	}
}

// RelayFor returns the relay a player is connected through.
func (h *StreamHub) RelayFor(pid uint32) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id, ok := h.presence[pid]
	return id, ok
}

// Route delivers an event to the relay holding pid's connection. It returns
// ErrNotConnected immediately if there is none, so the sender can react at once.
func (h *StreamHub) Route(pid uint32, p relaylink.Priority, topic string, body []byte) error {
	h.mu.Lock()
	id, ok := h.presence[pid]
	c := h.conns[id]
	h.mu.Unlock()
	if !ok || c == nil {
		return ErrNotConnected
	}
	return c.Send(p, topic, body)
}

// SendToRelay delivers an event to one relay.
func (h *StreamHub) SendToRelay(relayID string, p relaylink.Priority, topic string, body []byte) error {
	h.mu.Lock()
	c := h.conns[relayID]
	h.mu.Unlock()
	if c == nil {
		return ErrNotConnected
	}
	return c.Send(p, topic, body)
}

// CallRelay makes a request to a relay and waits for its answer.
func (h *StreamHub) CallRelay(ctx context.Context, relayID, method string, body []byte) ([]byte, error) {
	h.mu.Lock()
	c := h.conns[relayID]
	h.mu.Unlock()
	if c == nil {
		return nil, ErrNotConnected
	}
	return c.Call(ctx, method, body)
}

// Broadcast sends an event to every connected relay; it returns how many accepted it.
func (h *StreamHub) Broadcast(p relaylink.Priority, topic string, body []byte) int {
	h.mu.Lock()
	conns := make([]*relaylink.StreamConn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	n := 0
	for _, c := range conns {
		if c.Send(p, topic, body) == nil {
			n++
		}
	}
	return n
}

// HandleMethod registers a call handler that relays can invoke over the stream.
func (h *StreamHub) HandleMethod(name string, fn func(ctx context.Context, relayID string, body []byte) ([]byte, error)) {
	h.mu.Lock()
	if h.Methods == nil {
		h.Methods = map[string]func(context.Context, string, []byte) ([]byte, error){}
	}
	h.Methods[name] = fn
	h.mu.Unlock()
}

// Subscribe registers a handler for events relays send on a topic.
func (h *StreamHub) Subscribe(topic string, fn func(relayID string, body []byte)) {
	h.mu.Lock()
	h.subs[topic] = append(h.subs[topic], fn)
	h.mu.Unlock()
}

// RelayStatus describes one connected relay (for the dashboard and logs).
type RelayStatus struct {
	ID      string
	Since   time.Time
	RTT     time.Duration
	Players int
}

func (h *StreamHub) Status() []RelayStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []RelayStatus
	for id, c := range h.conns {
		out = append(out, RelayStatus{ID: id, Since: h.since[id], RTT: c.RTT(), Players: len(h.byRelay[id])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// PushInvalidation is wired to InvalidationLog.OnAppend: relays learn about a
// change in one network hop instead of at their next poll.
func (h *StreamHub) PushInvalidation(epoch string, e relaylink.Event) {
	body, _ := json.Marshal(map[string]any{"epoch": epoch, "seq": e.Seq, "tags": e.Tags})
	h.Broadcast(relaylink.High, TopicInvalidate, body)
}
