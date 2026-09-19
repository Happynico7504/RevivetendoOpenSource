package wiiuchatedge

import (
	"net"
	"testing"

	nex "github.com/PretendoNetwork/nex-go/v2"
)

func TestDefaultsMirrorWiiUChatSecure(t *testing.T) {
	e := New(Config{Port: 1}, &EchoBackend{})
	// wiiu-chat-secure/nex/secure.go: access key e7a47214 (auth and secure alike).
	if e.cfg.AccessKey != "e7a47214" {
		t.Fatalf("access key %q", e.cfg.AccessKey)
	}
	if custom := New(Config{AccessKey: "abcd1234"}, nil); custom.cfg.AccessKey != "abcd1234" {
		t.Fatal("an explicit access key was overridden")
	}
}

func TestAddrOfReadsTheSocketAddress(t *testing.T) {
	conn := &nex.PRUDPConnection{Socket: &nex.SocketConnection{Address: &net.UDPAddr{IP: net.ParseIP("203.0.113.7"), Port: 51234}}}
	if ip, port := addrOf(conn); ip != "203.0.113.7" || port != 51234 {
		t.Fatalf("got %s:%d", ip, port)
	}
	// No socket (a connection that is not established): no panic, empty answer.
	if ip, port := addrOf(&nex.PRUDPConnection{}); ip != "" || port != 0 {
		t.Fatalf("empty connection: %s:%d", ip, port)
	}
}
