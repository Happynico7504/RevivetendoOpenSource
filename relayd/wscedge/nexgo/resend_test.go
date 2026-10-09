package nex

import (
	"net"
	"testing"
)

// consoleData encodes reliable data packets carrying one RMC request each, the way the console
// holding the session would: its own RC4 keystream, sequence IDs counting up.
type consoleSide struct {
	t      *testing.T
	client *Client
}

func newConsoleSide(t *testing.T, server *Server, key, serverSig []byte) *consoleSide {
	c := NewClient(&net.UDPAddr{IP: net.IPv4(203, 0, 113, 5), Port: 50000}, server)
	c.SetSessionKey(key)
	c.SetClientConnectionSignature(serverSig)
	c.UpdateRC4Key(key)
	return &consoleSide{t: t, client: c}
}

func (cs *consoleSide) request(seq uint16, method uint32) []byte {
	req := NewRMCRequest()
	req.SetProtocolID(0x6d)
	req.SetCallID(uint32(seq))
	req.SetMethodID(method)
	req.SetParameters([]byte{1, 2, 3, 4})
	p, _ := NewPacketV1(cs.client, nil)
	p.SetVersion(1)
	p.SetSource(0xAF)
	p.SetDestination(0xA1)
	p.SetType(DataPacket)
	p.AddFlag(FlagReliable)
	p.AddFlag(FlagNeedsAck)
	p.SetSessionID(1)
	p.SetSequenceID(seq)
	p.SetPayload(req.Bytes())
	return p.Bytes() // encrypts with the console's keystream
}

func serverSide(t *testing.T) (*Server, *Client, *consoleSide) {
	server, c, key, sig := rebindServer(t)
	c.UpdateRC4Key(key)
	return server, c, newConsoleSide(t, server, key, sig)
}

func TestResentRequestDoesNotDesyncTheKeystream(t *testing.T) {
	_, c, console := serverSide(t)
	first := console.request(10, 0x1)
	second := console.request(11, 0x2)
	third := console.request(12, 0x3)

	for i, data := range [][]byte{first, second, second, third} { // 11 resent: its ack was late
		p, err := NewPacketV1(c, data)
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if dup := p.Duplicate(); dup != (i == 2) {
			t.Fatalf("packet %d: duplicate=%v", i, dup)
		}
		if i == 2 {
			continue
		}
		want := map[int]uint32{0: 1, 1: 2, 3: 3}[i]
		req := p.RMCRequest()
		if m := req.MethodID(); m != want {
			t.Fatalf("packet %d deciphered to method %#x - keystream out of step", i, m)
		}
	}
}

func TestRequestFromBeyondAGapWaits(t *testing.T) {
	_, c, console := serverSide(t)
	first, second, third := console.request(10, 0x1), console.request(11, 0x2), console.request(12, 0x3)
	if _, err := NewPacketV1(c, first); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPacketV1(c, third); err == nil {
		t.Fatal("request 12 processed before 11 arrived")
	}
	for _, data := range [][]byte{second, third} { // 11 arrives, the client resends 12
		p, err := NewPacketV1(c, data)
		if err != nil || p.Duplicate() {
			t.Fatalf("in-order packet: err=%v dup=%v", err, p.Duplicate())
		}
	}
}
