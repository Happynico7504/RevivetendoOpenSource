package wiiuchatedge

import (
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/nex-go/v2/types"

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

func TestICMPChecksumKnownVector(t *testing.T) {
	// RFC 1071 example data: 00 01 f2 03 f4 f5 f6 f7 -> checksum 0x220d.
	if got := icmpChecksum([]byte{0x00, 0x01, 0xf2, 0x03, 0xf4, 0xf5, 0xf6, 0xf7}); got != 0x220d {
		t.Fatalf("checksum %#04x, want 0x220d", got)
	}
	// An odd length is padded with a zero byte.
	if a, b := icmpChecksum([]byte{1, 2, 3}), icmpChecksum([]byte{1, 2, 3, 0}); a != b {
		t.Fatalf("odd length: %#04x vs %#04x", a, b)
	}
}

func TestBuiltInICMPPingsLoopbackAndParses(t *testing.T) {
	s, ok := icmpSummary("127.0.0.1", 3, 10*time.Millisecond, time.Second)
	if !ok {
		t.Skip("unprivileged ICMP sockets are not permitted here")
	}
	loss, avg := parsePingOutput(s)
	if loss != "0%" || avg == "?" {
		t.Fatalf("loopback: loss %q avg %q from %q", loss, avg, s)
	}
	if _, ok := icmpSummary("::1", 1, time.Millisecond, time.Millisecond); ok {
		t.Fatal("an IPv6 target must fall back to the ping binary")
	}
	// An address nobody answers for: full loss, and still a summary the parser understands.
	s, ok = icmpSummary("192.0.2.1", 1, time.Millisecond, 200*time.Millisecond) // TEST-NET-1
	if ok {
		if loss, avg := parsePingOutput(s); loss != "100%" || avg != "?" {
			t.Fatalf("unanswered: loss %q avg %q from %q", loss, avg, s)
		}
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

// recBackend records what the edge reports.
type recBackend struct {
	mu     sync.Mutex
	stats  []relaylink.WSCStats
	traces []relaylink.WSCTrace
}

func (b *recBackend) Open(uint32, string, int) error { return nil }
func (b *recBackend) Close(uint32)                   {}
func (b *recBackend) Handle(Call) error              { return nil }
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

func fakeConn(pid uint32, ip string) *nex.PRUDPConnection {
	c := nex.NewPRUDPConnection(nex.NewSocketConnection(nex.NewPRUDPServer(), &net.UDPAddr{IP: net.ParseIP(ip), Port: 1000}, nil))
	c.SetPID(types.NewPID(uint64(pid)))
	return c
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
	a, b := fakeConn(1, "8.8.8.8"), fakeConn(2, "9.9.9.9")
	a.RTT().Observe(40 * time.Millisecond) // one ack seen from player 1, none from player 2
	e.conns[1], e.conns[2] = a, b

	got := map[uint32]relaylink.WSCStats{}
	for _, s := range e.probeAll() {
		got[s.PID] = s
	}
	if len(got) != 2 {
		t.Fatalf("stats for %d players, want 2", len(got))
	}
	if s := got[1]; s.Loss != "0%" || s.AvgRTT != "2.5ms" || s.Samples != 1 || s.PRUDPRTTMs < 39 || s.PRUDPRTTMs > 41 || s.PRUDPMinMs < 39 || s.PRUDPMinMs > 41 || s.IP != "8.8.8.8" {
		t.Fatalf("player 1: %+v", s)
	}
	// The player whose router drops ICMP still gets a report, with no PRUDP sample yet.
	if s := got[2]; s.Loss != "100%" || s.AvgRTT != "?" || s.Samples != 0 {
		t.Fatalf("player 2: %+v", s)
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
	e.conns[5] = fakeConn(5, "8.8.4.4")

	e.probeOnce()
	waitTraces(t, b, 1)
	if b.traces[0].PID != 5 || b.traces[0].Reason != "loss=60%" || !strings.Contains(b.traces[0].Output, "traceroute to 8.8.4.4") {
		t.Fatalf("trace: %+v", b.traces[0])
	}
	e.probeOnce() // persistent loss inside the cooldown: measured again, not traced again
	time.Sleep(50 * time.Millisecond)
	if s, n := b.counts(); s != 2 || n != 1 {
		t.Fatalf("within the cooldown: %d stats, %d traces (want 2, 1)", s, n)
	}
	time.Sleep(150 * time.Millisecond)
	e.probeOnce()
	waitTraces(t, b, 2)
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

// v2's own RTT type is a retransmission timeout (three times the first sample) and is fed only by
// packets that were sent at least twice; the added Observe/Smoothed pair must report the plain round
// trip of first transmissions instead.
func TestRTTReportsPlainSmoothedAndMinimum(t *testing.T) {
	r := nex.NewRTT()
	if _, _, n := r.Smoothed(); n != 0 {
		t.Fatalf("samples before any ack: %d", n)
	}
	r.Observe(40 * time.Millisecond)
	sm, min, n := r.Smoothed()
	if n != 1 || sm != 40*time.Millisecond || min != 40*time.Millisecond {
		t.Fatalf("first sample: smoothed %v min %v n %d", sm, min, n)
	}
	r.Observe(80 * time.Millisecond) // alpha 1/8: 40 + (80-40)/8 = 45
	r.Observe(20 * time.Millisecond)
	sm, min, n = r.Smoothed()
	if n != 3 || min != 20*time.Millisecond || sm < 40*time.Millisecond || sm > 46*time.Millisecond {
		t.Fatalf("after three samples: smoothed %v min %v n %d", sm, min, n)
	}
	// Adjust is upstream's retransmission-timeout estimator (fed by resent packets): it must not
	// change the plain figures.
	r.Adjust(900 * time.Millisecond)
	if sm2, min2, n2 := r.Smoothed(); sm2 != sm || min2 != min || n2 != n {
		t.Fatalf("Adjust changed the plain round trip: %v/%v/%d -> %v/%v/%d", sm, min, n, sm2, min2, n2)
	}
}
