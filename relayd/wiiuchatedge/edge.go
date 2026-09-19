// Package wiiuchatedge terminates Wii U Chat PRUDP sessions on a relay, the counterpart of
// wscedge for the second secure server. It is its own module because Wii U Chat runs on
// nex-go v2 (a different major version and API from the patched v1 fork wscedge uses), and
// components ship separately with their own dependencies and version line.
//
// Milestone 1: the relay answers the Kerberos handshake, acknowledgements and keepalives
// itself (stock nex-go v2 does all of that) and hands each RMC request to a Backend. See
// relayd/deploy/WSC-EDGE.md for the design this repeats.
package wiiuchatedge

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/nex-go/v2/constants"
	"github.com/PretendoNetwork/nex-go/v2/types"

	"github.com/Happynico7504/relaylink"
)

// Config mirrors wiiu-chat-secure's secure server settings (nex/secure.go).
type Config struct {
	Port             int
	KerberosPassword string
	AccessKey        string // default "e7a47214"
	Logf             func(string, ...any)
}

// Call is one RMC request from a player.
type Call struct {
	PID      uint32
	IP       string
	Port     int
	CallID   uint32
	Protocol uint16 // v2 protocol ids can exceed 8 bits
	Method   uint32
	Params   []byte
}

// Backend is where the edge sends what it terminates.
type Backend interface {
	// Open reports a player the first time their session is heard from. An error refuses it.
	Open(pid uint32, ip string, port int) error
	Close(pid uint32)
	Handle(c Call) error
	// Stats reports what the edge measured toward a player (see probe.go).
	Stats(s relaylink.WSCStats)
	// Trace reports a traceroute run toward a player.
	Trace(t relaylink.WSCTrace)
}

// Edge is one running terminator.
type Edge struct {
	cfg     Config
	backend Backend
	server  *nex.PRUDPServer
	ep      *nex.PRUDPEndPoint

	mu        sync.Mutex
	conns     map[uint32]*nex.PRUDPConnection // pid -> live connection
	opened    map[*nex.PRUDPConnection]bool
	lastTrace map[uint32]time.Time // last loss-triggered traceroute per player
	resetting bool                 // Reset is dropping sessions the main has already closed
}

func New(cfg Config, b Backend) *Edge {
	if cfg.AccessKey == "" {
		cfg.AccessKey = "e7a47214"
	}
	return &Edge{cfg: cfg, backend: b, conns: map[uint32]*nex.PRUDPConnection{}, opened: map[*nex.PRUDPConnection]bool{}}
}

func (e *Edge) logf(f string, a ...any) {
	if e.cfg.Logf != nil {
		e.cfg.Logf(f, a...)
	}
}

// Serve configures the server exactly like wiiu-chat-secure and blocks in Listen.
func (e *Edge) Serve() {
	serverAccount := nex.NewAccount(types.NewPID(2), "Quazal Rendez-Vous", e.cfg.KerberosPassword, false)

	e.server = nex.NewPRUDPServer()
	e.ep = nex.NewPRUDPEndPoint(1)
	e.ep.IsSecureEndPoint = true
	e.ep.ServerAccount = serverAccount
	// Connecting only needs the server account (it decrypts the ticket the auth server issued);
	// player accounts do not exist on a relay, so any other PID gets a placeholder.
	e.ep.AccountDetailsByPID = func(pid types.PID) (*nex.Account, *nex.Error) {
		if pid.Equals(serverAccount.PID) {
			return serverAccount, nil
		}
		return nex.NewAccount(pid, strconv.FormatUint(uint64(pid), 10), "", false), nil
	}
	e.ep.AccountDetailsByUsername = func(username string) (*nex.Account, *nex.Error) {
		if username == serverAccount.Username {
			return serverAccount, nil
		}
		n, err := strconv.ParseUint(username, 10, 64)
		if err != nil {
			return nil, nex.NewError(nex.ResultCodes.RendezVous.InvalidUsername, "Invalid username")
		}
		return nex.NewAccount(types.NewPID(n), username, "", false), nil
	}
	e.server.BindPRUDPEndPoint(e.ep)
	e.server.ByteStreamSettings.UseStructureHeader = false
	// Technically 3.4.2, but it uses the older-style structures, so wiiu-chat-secure defines 3.3.2.
	e.server.LibraryVersions.SetDefault(nex.NewLibraryVersion(3, 3, 2))
	e.server.AccessKey = e.cfg.AccessKey

	e.ep.OnData(e.onData)
	e.ep.OnConnectionEnded(e.onEnded)
	go e.probeLoop()

	e.logf("wiiuchatedge: listening on :%d", e.cfg.Port)
	e.server.Listen(e.cfg.Port)
}

func (e *Edge) onData(packet nex.PacketInterface) {
	defer func() {
		if r := recover(); r != nil {
			e.logf("wiiuchatedge: bad data packet: %v", r)
		}
	}()
	conn, ok := packet.Sender().(*nex.PRUDPConnection)
	if !ok {
		return
	}
	msg := packet.RMCMessage()
	if msg == nil || !msg.IsRequest {
		return
	}
	pid := uint32(conn.PID())
	if pid == 0 {
		return // not past the Kerberos handshake
	}
	ip, port := addrOf(conn)

	e.mu.Lock()
	first := !e.opened[conn]
	if first {
		e.opened[conn] = true
		e.conns[pid] = conn
	}
	e.mu.Unlock()
	if first {
		if err := e.backend.Open(pid, ip, port); err != nil {
			e.logf("wiiuchatedge: PID=%d refused by the main: %v", pid, err)
			e.mu.Lock()
			delete(e.opened, conn)
			delete(e.conns, pid)
			e.mu.Unlock()
			return
		}
		e.logf("wiiuchatedge: PID=%d connected from %s:%d", pid, ip, port)
		// A path snapshot from the moment they connect, to compare a later degradation with.
		if ip != "" {
			e.traceAsync(pid, ip, "connect")
		}
	}

	call := Call{PID: pid, IP: ip, Port: port, CallID: msg.CallID, Protocol: uint16(msg.ProtocolID), Method: msg.MethodID, Params: msg.Parameters}
	go func() {
		if err := e.backend.Handle(call); err != nil {
			e.logf("wiiuchatedge: PID=%d proto=%#x method=%#x failed: %v", pid, call.Protocol, call.Method, err)
		}
	}()
}

func (e *Edge) onEnded(conn *nex.PRUDPConnection) {
	pid := uint32(conn.PID())
	e.mu.Lock()
	was := e.opened[conn]
	delete(e.opened, conn)
	if e.conns[pid] == conn {
		delete(e.conns, pid)
	}
	quiet := e.resetting // the main already closed these sessions
	e.mu.Unlock()
	if was && pid != 0 && !quiet {
		e.backend.Close(pid)
		e.logf("wiiuchatedge: PID=%d disconnected", pid)
	}
}

func addrOf(conn *nex.PRUDPConnection) (string, int) {
	if conn.Socket != nil && conn.Socket.Address != nil {
		if u, ok := conn.Socket.Address.(*net.UDPAddr); ok {
			return u.IP.String(), u.Port
		}
		if h, p, err := net.SplitHostPort(conn.Socket.Address.String()); err == nil {
			n, _ := strconv.Atoi(p)
			return h, n
		}
	}
	return "", 0
}

// Out delivers an RMC message from the main (a response or a notification) to a player.
func (e *Edge) Out(pid uint32, payload []byte) {
	e.mu.Lock()
	conn := e.conns[pid]
	e.mu.Unlock()
	if conn == nil {
		e.logf("wiiuchatedge: message for PID=%d, who is not connected here", pid)
		return
	}
	if err := e.send(conn, payload); err != nil {
		e.logf("wiiuchatedge: PID=%d: %v", pid, err)
	}
}

// Reset drops every session without telling the backend: the main has already closed them (its
// stream to this relay was lost), and the consoles reconnect and handshake again.
func (e *Edge) Reset() {
	e.mu.Lock()
	old := make([]*nex.PRUDPConnection, 0, len(e.conns))
	for _, c := range e.conns {
		old = append(old, c)
	}
	e.resetting = true
	e.mu.Unlock()
	for _, c := range old {
		e.ep.CleanupConnection(c)
	}
	e.mu.Lock()
	e.resetting = false
	e.mu.Unlock()
	e.logf("wiiuchatedge: reset: dropped %d sessions", len(old))
}

// Send puts a complete RMC message into a reliable data packet for the connection, the way the
// protocol library does it for notifications.
func (e *Edge) send(conn *nex.PRUDPConnection, rmcBytes []byte) error {
	var pkt nex.PRUDPPacketInterface
	var err error
	switch conn.DefaultPRUDPVersion {
	case 0:
		pkt, err = nex.NewPRUDPPacketV0(e.server, conn, nil)
	case 1:
		pkt, err = nex.NewPRUDPPacketV1(e.server, conn, nil)
	case 2:
		pkt, err = nex.NewPRUDPPacketLite(e.server, conn, nil)
	default:
		return fmt.Errorf("PRUDP version %d is not supported", conn.DefaultPRUDPVersion)
	}
	if err != nil {
		return err
	}
	pkt.SetType(constants.DataPacket)
	pkt.AddFlag(constants.PacketFlagNeedsAck)
	pkt.AddFlag(constants.PacketFlagReliable)
	pkt.SetSourceVirtualPortStreamType(conn.StreamType)
	pkt.SetSourceVirtualPortStreamID(e.ep.StreamID)
	pkt.SetDestinationVirtualPortStreamType(conn.StreamType)
	pkt.SetDestinationVirtualPortStreamID(conn.StreamID)
	pkt.SetPayload(rmcBytes)
	e.server.Send(pkt)
	return nil
}

// Respond sends an RMC success response to a player.
func (e *Edge) Respond(conn *nex.PRUDPConnection, protocolID uint16, callID uint32, methodID uint32, payload []byte) error {
	msg := nex.NewRMCSuccess(e.ep, payload)
	msg.ProtocolID = uint16(protocolID)
	msg.CallID = callID
	msg.MethodID = methodID
	return e.send(conn, msg.Bytes())
}

// EchoBackend answers every call with an empty success, itself: the handshake, acks and
// keepalives are still terminated locally. Set Edge after New.
type EchoBackend struct{ Edge *Edge }

func (b *EchoBackend) Open(pid uint32, ip string, port int) error { return nil }
func (b *EchoBackend) Close(pid uint32)                           {}
func (b *EchoBackend) Stats(s relaylink.WSCStats)                 {}
func (b *EchoBackend) Trace(t relaylink.WSCTrace)                 {}
func (b *EchoBackend) Handle(c Call) error {
	b.Edge.mu.Lock()
	conn := b.Edge.conns[c.PID]
	b.Edge.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("no connection for pid %d", c.PID)
	}
	return b.Edge.Respond(conn, c.Protocol, c.CallID, c.Method, nil)
}
