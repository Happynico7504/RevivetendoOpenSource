package relaylink

import (
	"net"
	"testing"
	"time"
)

func udpOn(t *testing.T, ip string) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Skipf("cannot bind %s: %v", ip, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func recv(t *testing.T, c *net.UDPConn) (string, *net.UDPAddr) {
	t.Helper()
	buf := make([]byte, 2048)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("no packet: %v", err)
	}
	return string(buf[:n]), from
}

func newTestTunnels(t *testing.T) *P2PTunnels {
	tun := NewP2PTunnels(P2PConfig{ListenIP: "127.0.0.1", PortMin: 41000, PortMax: 41999})
	stop := make(chan struct{})
	go tun.Run(stop)
	t.Cleanup(func() { close(stop) })
	return tun
}

func alias(port int) *net.UDPAddr { return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port} }

func TestP2PForwardsBothWaysWithAliasSources(t *testing.T) {
	tun := newTestTunnels(t)
	a, b := udpOn(t, "127.0.0.2"), udpOn(t, "127.0.0.3")
	ports, err := tun.Open("wsc:1", []P2PStation{
		{PID: 1, IP: "127.0.0.2", Port: a.LocalAddr().(*net.UDPAddr).Port},
		{PID: 2, IP: "127.0.0.3", Port: b.LocalAddr().(*net.UDPAddr).Port},
	})
	if err != nil || ports[1] == 0 || ports[2] == 0 || ports[1] == ports[2] {
		t.Fatalf("open: %v %v", ports, err)
	}
	// A talks to B's alias; B must see it coming from A's alias.
	a.WriteToUDP([]byte("hello"), alias(ports[2]))
	if msg, from := recv(t, b); msg != "hello" || from.Port != ports[1] {
		t.Fatalf("B got %q from %v, want hello from alias %d", msg, from, ports[1])
	}
	b.WriteToUDP([]byte("hi"), alias(ports[1]))
	if msg, from := recv(t, a); msg != "hi" || from.Port != ports[2] {
		t.Fatalf("A got %q from %v, want hi from alias %d", msg, from, ports[2])
	}
	// Opening again (a third player) keeps the existing aliases.
	again, err := tun.Open("wsc:1", []P2PStation{{PID: 1, IP: "127.0.0.2", Port: 1}, {PID: 3, IP: "127.0.0.4", Port: 9}})
	if err != nil || again[1] != ports[1] || again[2] != ports[2] || again[3] == 0 {
		t.Fatalf("reopen: %v %v", again, err)
	}
}

// A symmetric NAT: the console reaches the tunnel from another port than the one the game
// server saw, and answers must go to that new port.
func TestP2PLearnsTheRealSourcePort(t *testing.T) {
	tun := newTestTunnels(t)
	a, b := udpOn(t, "127.0.0.2"), udpOn(t, "127.0.0.3")
	ports, err := tun.Open("k", []P2PStation{{PID: 1, IP: "127.0.0.2", Port: 9}, {PID: 2, IP: "127.0.0.3", Port: 9}})
	if err != nil {
		t.Fatal(err)
	}
	// Both probe; the first packets go to the announced (wrong) ports and are lost, like
	// probes a NAT drops. Once each side was heard from, the retries get through.
	a.WriteToUDP([]byte("x"), alias(ports[2]))
	time.Sleep(50 * time.Millisecond)
	b.WriteToUDP([]byte("y"), alias(ports[1]))
	if msg, _ := recv(t, a); msg != "y" {
		t.Fatalf("A got %q", msg)
	}
	a.WriteToUDP([]byte("x2"), alias(ports[2]))
	if msg, _ := recv(t, b); msg != "x2" {
		t.Fatalf("B got %q", msg)
	}
}

// Before B was ever heard from, packets for it go to the address the game server announced.
func TestP2PUsesTheAnnouncedAddressFirst(t *testing.T) {
	tun := newTestTunnels(t)
	a, b := udpOn(t, "127.0.0.2"), udpOn(t, "127.0.0.3")
	ports, _ := tun.Open("k", []P2PStation{{PID: 1, IP: "127.0.0.2", Port: 9}, {PID: 2, IP: "127.0.0.3", Port: b.LocalAddr().(*net.UDPAddr).Port}})
	a.WriteToUDP([]byte("probe"), alias(ports[2]))
	if msg, from := recv(t, b); msg != "probe" || from.Port != ports[1] {
		t.Fatalf("B got %q from %v", msg, from)
	}
}

func TestP2PDropsStrangers(t *testing.T) {
	tun := newTestTunnels(t)
	a, b, x := udpOn(t, "127.0.0.2"), udpOn(t, "127.0.0.3"), udpOn(t, "127.0.0.9")
	ports, _ := tun.Open("k", []P2PStation{{PID: 1, IP: "127.0.0.2", Port: a.LocalAddr().(*net.UDPAddr).Port}, {PID: 2, IP: "127.0.0.3", Port: b.LocalAddr().(*net.UDPAddr).Port}})
	a.WriteToUDP([]byte("x"), alias(ports[2])) // A heard: no unheard station left except B itself
	recv(t, b)
	b.WriteToUDP([]byte("y"), alias(ports[1]))
	recv(t, a)
	x.WriteToUDP([]byte("evil"), alias(ports[2]))
	b.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := b.ReadFromUDP(make([]byte, 64)); err == nil {
		t.Fatal("a stranger's packet was forwarded")
	}
	if s := tun.Status(); len(s) != 1 || s[0].Dropped != 1 {
		t.Fatalf("status %+v", s)
	}
}

func TestP2PSweepClosesIdleSessions(t *testing.T) {
	tun := NewP2PTunnels(P2PConfig{ListenIP: "127.0.0.1", PortMin: 42000, PortMax: 42010, OpenGrace: time.Minute})
	now := time.Now()
	tun.now = func() time.Time { return now }
	ports, err := tun.Open("k", []P2PStation{{PID: 1, IP: "127.0.0.2", Port: 9}, {PID: 2, IP: "127.0.0.3", Port: 9}})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	tun.Sweep()
	if len(tun.Status()) != 0 {
		t.Fatal("unused session survived its grace period")
	}
	// Its ports are free again.
	c, err := net.ListenUDP("udp4", alias(ports[1]))
	if err != nil {
		t.Fatalf("port not released: %v", err)
	}
	c.Close()
}
