package relayhub

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Happynico7504/relaylink"
)

var p2pGeo = fakeGeo{
	"203.0.113.1": {"DE", "EU"},
	"203.0.113.2": {"US", "NA"},
	"203.0.113.3": {"FR", "EU"},
	"203.0.113.4": {"JP", "AS"},
}

type fakeRelayP2P struct {
	calls []string
	fail  bool
}

func (f *fakeRelayP2P) call(_ context.Context, id, method string, body []byte) ([]byte, error) {
	f.calls = append(f.calls, id+" "+method)
	if f.fail {
		return nil, errors.New("remote: unknown method")
	}
	var req relaylink.P2POpen
	json.Unmarshal(body, &req)
	res := relaylink.P2POpenResult{Ports: map[uint32]int{}}
	for i, s := range req.Stations {
		res.Ports[s.PID] = 50000 + i
	}
	return json.Marshal(res)
}

func newP2PRouter(t *testing.T, policy string, fr *fakeRelayP2P) *P2PRouter {
	local := relaylink.NewP2PTunnels(relaylink.P2PConfig{ListenIP: "127.0.0.1", PortMin: 43000, PortMax: 43100})
	t.Cleanup(func() { stop := make(chan struct{}); close(stop); local.Run(stop) })
	return &P2PRouter{
		Local: local, LocalIP: "198.51.100.1", Geo: p2pGeo,
		Policy: func() P2PPolicy { return ParseP2PPolicy(policy) },
		Relays: func(context.Context) ([]P2PInstance, error) {
			return []P2PInstance{{ID: "us-1", Regions: []string{"na"}, IP: "198.51.100.2"}}, nil
		},
		CallRelay: fr.call,
	}
}

func st(pid uint32, ip string) relaylink.P2PStation {
	return relaylink.P2PStation{PID: pid, IP: ip, Port: 5000}
}

func TestParseP2PPolicy(t *testing.T) {
	p := ParseP2PPolicy("international # cross-region\n123, 456\n")
	if !p.International || p.All || !p.PIDs[123] || !p.PIDs[456] {
		t.Fatalf("%+v", p)
	}
	if !ParseP2PPolicy("").Off() || !ParseP2PPolicy("*").All {
		t.Fatal("empty/all")
	}
}

func TestP2PSameRegionStaysDirect(t *testing.T) {
	r := newP2PRouter(t, "international", &fakeRelayP2P{})
	if _, err := r.Open(context.Background(), P2PRequest{Key: "wsc:1", Stations: []relaylink.P2PStation{st(1, "203.0.113.1"), st(2, "203.0.113.3")}}); err != ErrNoTunnel {
		t.Fatalf("DE+FR got %v, want no tunnel", err)
	}
}

func TestP2PInternationalGoesToTheHostsRegion(t *testing.T) {
	fr := &fakeRelayP2P{}
	r := newP2PRouter(t, "international", fr)
	// US host, German joiner: the US relay.
	res, err := r.Open(context.Background(), P2PRequest{Key: "wsc:1", Stations: []relaylink.P2PStation{st(1, "203.0.113.2"), st(2, "203.0.113.1")}})
	if err != nil || res.Instance != "us-1" || res.IP != "198.51.100.2" || res.Ports[2] == 0 {
		t.Fatalf("%+v %v", res, err)
	}
	// German host, US joiner: the main.
	res, err = r.Open(context.Background(), P2PRequest{Key: "wsc:2", Stations: []relaylink.P2PStation{st(1, "203.0.113.1"), st(2, "203.0.113.2")}})
	if err != nil || res.Instance != MainInstance || res.IP != "198.51.100.1" {
		t.Fatalf("%+v %v", res, err)
	}
	// Japanese host (no relay there), US joiner: the US relay, in a console's region.
	res, err = r.Open(context.Background(), P2PRequest{Key: "wsc:3", Stations: []relaylink.P2PStation{st(1, "203.0.113.4"), st(2, "203.0.113.2")}})
	if err != nil || res.Instance != "us-1" {
		t.Fatalf("%+v %v", res, err)
	}
	// A third console later: the same instance, even though it alone would stay direct.
	res, err = r.Open(context.Background(), P2PRequest{Key: "wsc:2", Stations: []relaylink.P2PStation{st(3, "203.0.113.3")}})
	if err != nil || res.Instance != MainInstance || res.Ports[1] == 0 || res.Ports[3] == 0 {
		t.Fatalf("extend: %+v %v", res, err)
	}
}

func TestP2PFallsBackToTheMainWhenTheRelayCannot(t *testing.T) {
	fr := &fakeRelayP2P{fail: true}
	r := newP2PRouter(t, "international", fr)
	res, err := r.Open(context.Background(), P2PRequest{Key: "wsc:1", Stations: []relaylink.P2PStation{st(1, "203.0.113.2"), st(2, "203.0.113.1")}})
	if err != nil || res.Instance != MainInstance {
		t.Fatalf("%+v %v", res, err)
	}
	// The relay is skipped for a while instead of being asked every time.
	r.Open(context.Background(), P2PRequest{Key: "wsc:9", Stations: []relaylink.P2PStation{st(1, "203.0.113.2"), st(2, "203.0.113.1")}})
	if len(fr.calls) != 1 {
		t.Fatalf("relay asked %d times", len(fr.calls))
	}
}

func TestP2PListedPIDAndOff(t *testing.T) {
	r := newP2PRouter(t, "7", &fakeRelayP2P{})
	if _, err := r.Open(context.Background(), P2PRequest{Key: "a", Stations: []relaylink.P2PStation{st(7, "203.0.113.1"), st(8, "203.0.113.3")}}); err != nil {
		t.Fatalf("listed pid: %v", err)
	}
	r = newP2PRouter(t, "", &fakeRelayP2P{})
	if _, err := r.Open(context.Background(), P2PRequest{Key: "b", Stations: []relaylink.P2PStation{st(1, "203.0.113.2"), st(2, "203.0.113.1")}}); err != ErrNoTunnel {
		t.Fatalf("policy off: %v", err)
	}
}

func TestP2PSameAddressStaysDirect(t *testing.T) {
	r := newP2PRouter(t, "*", &fakeRelayP2P{})
	if _, err := r.Open(context.Background(), P2PRequest{Key: "a", Stations: []relaylink.P2PStation{st(1, "203.0.113.1"), st(2, "203.0.113.1")}}); err != ErrNoTunnel {
		t.Fatalf("same household: %v", err)
	}
}

func TestP2PHardToReachConsoleInOneRegion(t *testing.T) {
	stations := []relaylink.P2PStation{st(1, "203.0.113.1"), st(2, "203.0.113.3")}
	stations[1].Hard = "symmetric NAT (natm=2)"
	r := newP2PRouter(t, "international", &fakeRelayP2P{})
	if _, err := r.Open(context.Background(), P2PRequest{Key: "a", Stations: stations}); err != ErrNoTunnel {
		t.Fatalf("nat not in the policy: %v", err)
	}
	r = newP2PRouter(t, "international nat", &fakeRelayP2P{})
	res, err := r.Open(context.Background(), P2PRequest{Key: "b", Stations: stations})
	if err != nil || res.Instance != MainInstance {
		t.Fatalf("%+v %v", res, err)
	}
	if !ParseP2PPolicy("nat").NAT || ParseP2PPolicy("nat").Off() {
		t.Fatal("parse nat")
	}
}

func TestP2PUnknownRegionsPreferTheMain(t *testing.T) {
	fr := &fakeRelayP2P{}
	r := newP2PRouter(t, "*", fr)
	res, err := r.Open(context.Background(), P2PRequest{Key: "a", Stations: []relaylink.P2PStation{st(1, "192.0.2.1"), st(2, "192.0.2.2")}})
	if err != nil || res.Instance != MainInstance || len(fr.calls) != 0 {
		t.Fatalf("%+v %v calls=%v", res, err, fr.calls)
	}
}
