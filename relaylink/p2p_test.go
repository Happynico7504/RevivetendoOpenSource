package relaylink

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"encoding/binary"
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
	ports, err := tun.Open("wsc:1", "", []P2PStation{
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
	again, err := tun.Open("wsc:1", "", []P2PStation{{PID: 1, IP: "127.0.0.2", Port: 1}, {PID: 3, IP: "127.0.0.4", Port: 9}})
	if err != nil || again[1] != ports[1] || again[2] != ports[2] || again[3] == 0 {
		t.Fatalf("reopen: %v %v", again, err)
	}
}

// A symmetric NAT: the console reaches the tunnel from another port than the one the game
// server saw, and answers must go to that new port.
func TestP2PLearnsTheRealSourcePort(t *testing.T) {
	tun := newTestTunnels(t)
	a, b := udpOn(t, "127.0.0.2"), udpOn(t, "127.0.0.3")
	ports, err := tun.Open("k", "", []P2PStation{{PID: 1, IP: "127.0.0.2", Port: 9}, {PID: 2, IP: "127.0.0.3", Port: 9}})
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
	ports, _ := tun.Open("k", "", []P2PStation{{PID: 1, IP: "127.0.0.2", Port: 9}, {PID: 2, IP: "127.0.0.3", Port: b.LocalAddr().(*net.UDPAddr).Port}})
	a.WriteToUDP([]byte("probe"), alias(ports[2]))
	if msg, from := recv(t, b); msg != "probe" || from.Port != ports[1] {
		t.Fatalf("B got %q from %v", msg, from)
	}
}

func TestP2PDropsStrangers(t *testing.T) {
	tun := newTestTunnels(t)
	a, b, x := udpOn(t, "127.0.0.2"), udpOn(t, "127.0.0.3"), udpOn(t, "127.0.0.9")
	ports, _ := tun.Open("k", "", []P2PStation{{PID: 1, IP: "127.0.0.2", Port: a.LocalAddr().(*net.UDPAddr).Port}, {PID: 2, IP: "127.0.0.3", Port: b.LocalAddr().(*net.UDPAddr).Port}})
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
	ports, err := tun.Open("k", "", []P2PStation{{PID: 1, IP: "127.0.0.2", Port: 9}, {PID: 2, IP: "127.0.0.3", Port: 9}})
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

// piaPacket builds a signed PIA packet holding a station list like the one a WSC host sends.
func piaPacket(locs ...[]byte) []byte {
	p := append([]byte{}, piaMagic...)
	p = append(p, 0x01, 0xb1, 0x00, 0x01, 0x51, 0xfa, 0x1e, 0xe6, 0, 0, 1, 4)
	for _, l := range locs {
		p = append(p, l...)
	}
	mac := hmac.New(md5.New, nil)
	mac.Write(p)
	return mac.Sum(p)
}

func piaLoc(ip [4]byte, port uint16, pid uint32) []byte {
	b := make([]byte, 0, 27)
	b = append(b, ip[:]...)
	b = binary.BigEndian.AppendUint16(b, port)
	b = append(b, 0, 0)
	b = binary.BigEndian.AppendUint32(b, pid)
	b = append(b, 0, 0, 0, 0, 0, 0, 0, 0x14, 0x01, 0x0f, 0x00, 0x01, 0x02)
	return b
}

func TestRewritePIAStationList(t *testing.T) {
	pub := [4]byte{203, 0, 113, 5}
	lan := [4]byte{192, 168, 8, 99}
	other := [4]byte{198, 51, 100, 7}
	pkt := piaPacket(piaLoc(pub, 51765, 1001), piaLoc(lan, 51765, 1001), piaLoc(other, 62080, 1002), piaLoc(other, 4000, 9999))
	aliases := map[uint32]piaAlias{1001: {ip: [4]byte{192, 0, 2, 1}, port: 61113}, 1002: {ip: [4]byte{192, 0, 2, 1}, port: 61314}}
	if n := rewritePIA(pkt, aliases, 0); n != 2 {
		t.Fatalf("rewrote %d locations, want 2 (public of 1001 and 1002; never LAN, never a stranger)", n)
	}
	want := piaPacket(piaLoc([4]byte{192, 0, 2, 1}, 61113, 1001), piaLoc(lan, 51765, 1001), piaLoc([4]byte{192, 0, 2, 1}, 61314, 1002), piaLoc(other, 4000, 9999))
	if !bytes.Equal(pkt, want) {
		t.Fatalf("got  %x\nwant %x", pkt, want)
	}
	if !piaSigned(pkt) {
		t.Fatal("rewritten packet is not signed correctly")
	}
	if n := rewritePIA(pkt, aliases, 0); n != 0 {
		t.Fatalf("second pass rewrote %d", n)
	}
}

func TestRewritePIALeavesForeignPackets(t *testing.T) {
	aliases := map[uint32]piaAlias{1001: {ip: [4]byte{192, 0, 2, 1}, port: 61113}}
	pkt := piaPacket(piaLoc([4]byte{203, 0, 113, 5}, 1, 1001))
	pkt[len(pkt)-1] ^= 1 // signed with some other key
	orig := append([]byte(nil), pkt...)
	if rewritePIA(pkt, aliases, 0) != 0 || !bytes.Equal(pkt, orig) {
		t.Fatal("touched a packet that is not signed with the empty key")
	}
	notPIA := append([]byte{1, 2, 3, 4}, piaLoc([4]byte{203, 0, 113, 5}, 1, 1001)...)
	if rewritePIA(notPIA, aliases, 0) != 0 {
		t.Fatal("touched a non-PIA packet")
	}
}

// End to end: a station list from A to B arrives with C's location replaced by C's alias.
func TestP2PRewritesInBandLocations(t *testing.T) {
	tun := newTestTunnels(t)
	a, b := udpOn(t, "127.0.0.2"), udpOn(t, "127.0.0.3")
	ports, err := tun.Open("k", "127.0.0.1", []P2PStation{
		{PID: 1, IP: "127.0.0.2", Port: a.LocalAddr().(*net.UDPAddr).Port},
		{PID: 2, IP: "127.0.0.3", Port: b.LocalAddr().(*net.UDPAddr).Port},
		{PID: 3, IP: "203.0.113.9", Port: 5000},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.WriteToUDP(piaPacket(piaLoc([4]byte{203, 0, 113, 9}, 5000, 3)), alias(ports[2]))
	got, _ := recv(t, b)
	want := piaPacket(piaLoc([4]byte{127, 0, 0, 1}, uint16(ports[3]), 3))
	if got != string(want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestRewritePIAKeepsSenderAndReceiver(t *testing.T) {
	a, b, c := [4]byte{203, 0, 113, 5}, [4]byte{198, 51, 100, 7}, [4]byte{198, 51, 100, 9}
	pkt := piaPacket(piaLoc(a, 1, 1001), piaLoc(b, 2, 1002), piaLoc(c, 3, 1003))
	al := [4]byte{192, 0, 2, 1}
	aliases := map[uint32]piaAlias{1001: {ip: al, port: 61001}, 1002: {ip: al, port: 61002}, 1003: {ip: al, port: 61003}}
	if n := rewritePIA(pkt, aliases, 1001, 1002); n != 1 {
		t.Fatalf("rewrote %d, want only the third party", n)
	}
	if want := piaPacket(piaLoc(a, 1, 1001), piaLoc(b, 2, 1002), piaLoc(al, 61003, 1003)); !bytes.Equal(pkt, want) {
		t.Fatalf("got  %x\nwant %x", pkt, want)
	}
}

func TestRewritePIAKeepsTheReceiversOwnLocation(t *testing.T) {
	me, other := [4]byte{203, 0, 113, 5}, [4]byte{198, 51, 100, 7}
	pkt := piaPacket(piaLoc(me, 51765, 1001), piaLoc(other, 62080, 1002))
	aliases := map[uint32]piaAlias{1001: {ip: [4]byte{192, 0, 2, 1}, port: 61113}, 1002: {ip: [4]byte{192, 0, 2, 1}, port: 61314}}
	if n := rewritePIA(pkt, aliases, 1001); n != 1 {
		t.Fatalf("rewrote %d, want only the other console's location", n)
	}
	want := piaPacket(piaLoc(me, 51765, 1001), piaLoc([4]byte{192, 0, 2, 1}, 61314, 1002))
	if !bytes.Equal(pkt, want) {
		t.Fatalf("got  %x\nwant %x", pkt, want)
	}
}
