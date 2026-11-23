package nex

import (
	"bytes"
	"net"
	"testing"
)

// consolePacket signs a ping the way the console holding the given session would.
func consolePacket(t *testing.T, server *Server, sessionKey, serverSig []byte, from *net.UDPAddr) []byte {
	t.Helper()
	fake := NewClient(from, server)
	fake.SetSessionKey(sessionKey)
	fake.SetClientConnectionSignature(serverSig) // the console signs with the server's connection signature
	p, _ := NewPacketV1(fake, nil)
	p.SetVersion(1)
	p.SetSource(0xAF)
	p.SetDestination(0xA1)
	p.SetType(PingPacket)
	p.AddFlag(FlagNeedsAck)
	p.SetSessionID(1)
	return p.Bytes()
}

func rebindServer(t *testing.T) (*Server, *Client, []byte, []byte) {
	server := NewServer()
	server.SetPRUDPVersion(1)
	server.SetAccessKey("6f599f81")
	old := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 5), Port: 50000}
	c := NewClient(old, server)
	key := bytes.Repeat([]byte{7}, 32)
	sig := bytes.Repeat([]byte{9}, 16)
	c.SetSessionKey(key)
	c.SetServerConnectionSignature(sig)
	c.SetPID(1001)
	server.clients[old.String()] = c
	return server, c, key, sig
}

func TestRebindMovesTheSessionToTheNewPort(t *testing.T) {
	server, c, key, sig := rebindServer(t)
	moved := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 5), Port: 50777}
	var hooked bool
	server.OnRebind(func(cl *Client, from, to *net.UDPAddr) { hooked = cl == c && from.Port == 50000 && to.Port == 50777 })
	got, pkt := server.rebind(moved, consolePacket(t, server, key, sig, moved))
	if got != c || pkt == nil || !hooked {
		t.Fatalf("rebind = %v, %v, hook %v", got, pkt, hooked)
	}
	if c.Address().String() != moved.String() || server.clients[moved.String()] != c || server.clients["203.0.113.5:50000"] != nil {
		t.Fatalf("client not moved: addr %v, map %v", c.Address(), server.clients)
	}
}

func TestRebindRefusesOtherSessionsAndOtherIPs(t *testing.T) {
	server, c, key, sig := rebindServer(t)
	moved := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 5), Port: 50777}
	if got, _ := server.rebind(moved, consolePacket(t, server, bytes.Repeat([]byte{8}, 32), sig, moved)); got != nil {
		t.Fatal("a packet signed with another session key took over the session")
	}
	other := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 9), Port: 50777}
	if got, _ := server.rebind(other, consolePacket(t, server, key, sig, other)); got != nil {
		t.Fatal("a client was moved to another IP")
	}
	if c.Address().Port != 50000 {
		t.Fatal("client address changed")
	}
}
