package relayhub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Happynico7504/relaylink"
)

// EdgeBridge connects relays that terminate WSC player sessions to wsc-secure. Inbound, a
// relay's stream calls are forwarded to wsc-secure's loopback endpoint with the relay's
// authenticated id filled in (a relay can never claim to be another). Outbound, wsc-secure
// posts what the game wants to send to a player to Out, and it is routed to the relay that
// holds that player's session. The hub's presence table records who is where, and a relay
// whose stream drops has all its players closed on the main.
type EdgeBridge struct {
	Streams *StreamHub
	Main    string // the game server's edge endpoint, e.g. "http://127.0.0.1:9451"
	HTTP    *http.Client
	Logf    func(string, ...any)
	// Names are the stream methods and topic this bridge serves (default: WSC's). Wii U Chat's
	// bridge uses relaylink.WUCEdgeNames, so both edges share one hub.
	Names relaylink.EdgeNames

	mu       sync.Mutex
	sessions map[uint32]string // pid -> relay holding the session. Per bridge, not the hub's shared presence table: the same player can be in WSC and Wii U Chat at once.
}

func (b *EdgeBridge) names() relaylink.EdgeNames {
	if b.Names.Open == "" {
		return relaylink.WSCEdgeNames
	}
	return b.Names
}

// RelayFor returns the relay holding a player's session on this bridge.
func (b *EdgeBridge) RelayFor(pid uint32) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.sessions[pid]
	return id, ok
}

func (b *EdgeBridge) setSession(pid uint32, relay string, present bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sessions == nil {
		b.sessions = map[uint32]string{}
	}
	if present {
		b.sessions[pid] = relay
	} else if b.sessions[pid] == relay {
		delete(b.sessions, pid)
	}
}

func (b *EdgeBridge) logf(f string, a ...any) {
	if b.Logf != nil {
		b.Logf(f, a...)
	}
}

func (b *EdgeBridge) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// Register installs the stream methods and the relay-down cleanup.
func (b *EdgeBridge) Register() {
	n := b.names()
	b.Streams.HandleMethod(n.Open, b.method("open", true))
	b.Streams.HandleMethod(n.RMC, b.method("rmc", false))
	b.Streams.HandleMethod(n.Alive, b.method("alive", false))
	b.Streams.HandleMethod(n.Close, b.method("close", false))
	b.Streams.HandleMethod(n.Stats, b.method("stats", false))
	b.Streams.HandleMethod(n.Trace, b.method("trace", false))

	prev := b.Streams.OnRelayDown
	b.Streams.OnRelayDown = func(relayID string, pids []uint32) {
		if prev != nil {
			prev(relayID, pids)
		}
		// Best effort: the relay is gone, so its players are too. Only this bridge's own players.
		b.mu.Lock()
		var mine []uint32
		for pid, id := range b.sessions {
			if id == relayID {
				mine = append(mine, pid)
				delete(b.sessions, pid)
			}
		}
		b.mu.Unlock()
		for _, pid := range mine {
			b.post(context.Background(), "close", relayID, map[string]any{"pid": pid})
		}
	}
}

// method builds the handler of one stream call. open marks a new player as present on the
// relay; close removes them.
func (b *EdgeBridge) method(op string, open bool) func(context.Context, string, []byte) ([]byte, error) {
	return func(ctx context.Context, relayID string, body []byte) ([]byte, error) {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, fmt.Errorf("bad %s body", op)
		}
		if op != "alive" {
			pid, _ := m["pid"].(float64)
			if pid <= 0 {
				return nil, fmt.Errorf("%s: missing pid", op)
			}
		}
		if err := b.post(ctx, op, relayID, m); err != nil {
			return nil, err
		}
		switch {
		case open:
			b.setSession(uint32(m["pid"].(float64)), relayID, true)
		case op == "close":
			b.setSession(uint32(m["pid"].(float64)), relayID, false)
		}
		return nil, nil
	}
}

// post forwards one message to wsc-secure. The relay id always comes from the authenticated
// stream, whatever the body said.
func (b *EdgeBridge) post(ctx context.Context, op, relayID string, m map[string]any) error {
	m["relay"] = relayID
	raw, _ := json.Marshal(m)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Main+"/edge/"+op, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client().Do(req)
	if err != nil {
		return fmt.Errorf("main unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("main refused %s: %s %s", op, resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// Out is the local endpoint wsc-secure posts outbound messages to: {relay,pid,payload}.
// It answers 404 when that player's relay is not connected, so wsc-secure can log it.
func (b *EdgeBridge) Out(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var m struct {
		Relay   string `json:"relay"`
		PID     uint32 `json:"pid"`
		Payload []byte `json:"payload"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&m); err != nil || m.PID == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	body, _ := json.Marshal(relaylink.WSCOut{PID: m.PID, Payload: m.Payload})
	// Route to the relay this bridge recorded for the player: it is what the hub trusts about who
	// holds the session (the game server's own idea of the relay is not).
	relay, ok := b.RelayFor(m.PID)
	if !ok {
		http.Error(w, "no relay holds that player", http.StatusNotFound)
		return
	}
	if err := b.Streams.SendToRelay(relay, relaylink.High, b.names().Out, body); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
