package wscedge

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	nex "github.com/PretendoNetwork/nex-go"

	"github.com/Happynico7504/relaylink"
)

func TestParsePingOutput(t *testing.T) {
	ok := "5 packets transmitted, 5 received, 0% packet loss, time 4004ms\nrtt min/avg/max/mdev = 3.5/3.6/3.7/0.04 ms\n"
	if l, a := parsePingOutput(ok); l != "0%" || a != "3.6ms" {
		t.Fatalf("clean: %q %q", l, a)
	}
	lossy := "5 packets transmitted, 2 received, 60% packet loss, time 4004ms\nrtt min/avg/max/mdev = 100.5/110.25/120.0/9.7 ms\n"
	if l, a := parsePingOutput(lossy); l != "60%" || a != "110.25ms" {
		t.Fatalf("lossy: %q %q", l, a)
	}
	// A router that drops ICMP: 100% loss and no rtt line at all.
	if l, a := parsePingOutput("5 packets transmitted, 0 received, 100% packet loss, time 4083ms\n"); l != "100%" || a != "?" {
		t.Fatalf("dropped: %q %q", l, a)
	}
	if l, a := parsePingOutput("ping: unknown host"); l != "?" || a != "?" {
		t.Fatalf("garbage: %q %q", l, a)
	}
}

func TestAckRoundTripIsMeasuredAndIgnoresRetransmits(t *testing.T) {
	c := nex.NewClient(&net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 1}, nex.NewServer())
	if _, _, n := c.RTT(); n != 0 {
		t.Fatalf("samples before any ack: %d", n)
	}
	c.TrackPending(1, []byte("a"))
	time.Sleep(30 * time.Millisecond)
	c.AcknowledgePending(1)
	sm, min, n := c.RTT()
	if n != 1 || sm < 25*time.Millisecond || sm > 500*time.Millisecond || min != sm {
		t.Fatalf("first sample: smoothed %v min %v n %d", sm, min, n)
	}
	// An ack for a packet that was never tracked (a duplicate) adds nothing.
	c.AcknowledgePending(99)
	if _, _, n := c.RTT(); n != 1 {
		t.Fatalf("an unknown ack was counted: %d", n)
	}
	// A cumulative ack samples only the packet it names: the others may have waited for the
	// aggregate ack and would inflate the figure.
	c.TrackPending(10, []byte("x"))
	c.TrackPending(11, []byte("y"))
	time.Sleep(10 * time.Millisecond)
	c.AcknowledgePendingUpTo(11, nil)
	if _, _, n := c.RTT(); n != 2 {
		t.Fatalf("cumulative ack sampled %d packets, want 1 new sample (total 2)", n)
	}
}

func TestProbeAllMeasuresEveryConnectedPlayer(t *testing.T) {
	old := Pinger
	defer func() { Pinger = old }()
	Pinger = func(ip string) string {
		if ip == "9.9.9.9" {
			return "5 packets transmitted, 0 received, 100% packet loss\n"
		}
		return "5 packets transmitted, 5 received, 0% packet loss\nrtt min/avg/max/mdev = 1/2.5/4/0.5 ms\n"
	}
	e := New(Config{}, &EchoBackend{})
	a := nex.NewClient(&net.UDPAddr{IP: net.ParseIP("8.8.8.8"), Port: 1000}, nex.NewServer())
	a.SetPID(1)
	b := nex.NewClient(&net.UDPAddr{IP: net.ParseIP("9.9.9.9"), Port: 2000}, nex.NewServer())
	b.SetPID(2)
	a.TrackPending(1, nil)
	time.Sleep(15 * time.Millisecond)
	a.AcknowledgePending(1)
	e.clients[1], e.clients[2] = a, b

	got := map[uint32]relaylink.WSCStats{}
	for _, s := range e.probeAll() {
		got[s.PID] = s
	}
	if len(got) != 2 {
		t.Fatalf("stats for %d players, want 2", len(got))
	}
	if s := got[1]; s.Loss != "0%" || s.AvgRTT != "2.5ms" || s.Samples != 1 || s.PRUDPRTTMs < 10 || s.IP != "8.8.8.8" {
		t.Fatalf("player 1: %+v", s)
	}
	// The player whose router drops ICMP still gets a report, with no PRUDP samples yet.
	if s := got[2]; s.Loss != "100%" || s.AvgRTT != "?" || s.Samples != 0 {
		t.Fatalf("player 2: %+v", s)
	}
}

func TestFormatStatsMatchesTheMainsLogLines(t *testing.T) {
	ping, rtt := FormatStats(relaylink.WSCStats{PID: 7, IP: "1.2.3.4", Loss: "0%", AvgRTT: "12ms", PRUDPRTTMs: 14.26, PRUDPMinMs: 11.0, Samples: 9}, "edge:us-1")
	if ping != "PlayerPing: PID=7 ip=1.2.3.4 loss=0% avgRTT=12ms via=edge:us-1" {
		t.Fatalf("ping line: %q", ping)
	}
	if !strings.HasPrefix(rtt, "PlayerRTT: PID=7 ip=1.2.3.4 via=edge:us-1 prudp=14.3ms min=11.0ms samples=9") {
		t.Fatalf("rtt line: %q", rtt)
	}
}

// recBackend records what the edge reports.
type recBackend struct {
	mu     sync.Mutex
	stats  []relaylink.WSCStats
	traces []relaylink.WSCTrace
}

func (b *recBackend) Open(uint32, string, int) error { return nil }
func (b *recBackend) Close(uint32)                   {}
func (b *recBackend) Handle(Call) error              { return nil }
func (b *recBackend) Alive([]uint32)                 {}
func (b *recBackend) Stats(s relaylink.WSCStats) {
	b.mu.Lock()
	b.stats = append(b.stats, s)
	b.mu.Unlock()
}
func (b *recBackend) Trace(t relaylink.WSCTrace) {
	b.mu.Lock()
	b.traces = append(b.traces, t)
	b.mu.Unlock()
}
func (b *recBackend) counts() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.stats), len(b.traces)
}

func waitTraces(t *testing.T, b *recBackend, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, n := b.counts(); n >= want {
			return
		}
		if time.Now().After(deadline) {
			_, n := b.counts()
			t.Fatalf("%d traces, want %d", n, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLossTriggersOneTracerouteWithACooldown(t *testing.T) {
	oldP, oldT, oldC := Pinger, Tracer, TraceCooldown
	defer func() { Pinger, Tracer, TraceCooldown = oldP, oldT, oldC }()
	loss := "60%"
	Pinger = func(string) string {
		return "5 packets transmitted, 2 received, " + loss + " packet loss\nrtt min/avg/max/mdev = 1/2/3/0.1 ms\n"
	}
	Tracer = func(ip string) string { return "traceroute to " + ip + "\n 1  hop\n" }
	TraceCooldown = 150 * time.Millisecond

	b := &recBackend{}
	e := New(Config{}, b)
	c := nex.NewClient(&net.UDPAddr{IP: net.ParseIP("8.8.4.4"), Port: 1}, nex.NewServer())
	c.SetPID(5)
	e.clients[5] = c

	e.probeOnce()
	waitTraces(t, b, 1)
	if b.traces[0].PID != 5 || b.traces[0].Reason != "loss=60%" || !strings.Contains(b.traces[0].Output, "traceroute to 8.8.4.4") {
		t.Fatalf("trace: %+v", b.traces[0])
	}
	// Persistent loss inside the cooldown: measured again, not traced again.
	e.probeOnce()
	time.Sleep(50 * time.Millisecond)
	if s, n := b.counts(); s != 2 || n != 1 {
		t.Fatalf("within the cooldown: %d stats, %d traces (want 2, 1)", s, n)
	}
	// After the cooldown it may trace again.
	time.Sleep(150 * time.Millisecond)
	e.probeOnce()
	waitTraces(t, b, 2)
	// A clean path and an unparseable answer never trigger one.
	loss = "0%"
	time.Sleep(160 * time.Millisecond)
	e.probeOnce()
	Pinger = func(string) string { return "ping: something odd" }
	e.probeOnce()
	time.Sleep(50 * time.Millisecond)
	if _, n := b.counts(); n != 2 {
		t.Fatalf("clean or unparseable pings triggered a trace (%d traces)", n)
	}
}

func TestTraceAsyncReportsTheBaselineSnapshot(t *testing.T) {
	oldT := Tracer
	defer func() { Tracer = oldT }()
	Tracer = func(ip string) string { return "path to " + ip }
	b := &recBackend{}
	e := New(Config{}, b)
	e.traceAsync(9, "1.2.3.4", "connect")
	waitTraces(t, b, 1)
	if tr := b.traces[0]; tr.PID != 9 || tr.IP != "1.2.3.4" || tr.Reason != "connect" || tr.Output != "path to 1.2.3.4" {
		t.Fatalf("baseline: %+v", tr)
	}
}
