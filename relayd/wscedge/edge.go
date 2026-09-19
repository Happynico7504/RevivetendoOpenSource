// Package wscedge terminates Wii Sports Club PRUDP sessions on a relay so the
// handshake, acknowledgements and keepalives are answered locally, and only complete
// RMC calls have to cross to the main. Prototype: see relayd/deploy/WSC-EDGE.md.
//
// nex-go panics inside its own goroutines, so like nexauth this only ever runs in a
// child process (`relayd wscedge`).
package wscedge

import (
	"fmt"
	"net"
	"sync"
	"time"

	nex "github.com/PretendoNetwork/nex-go"
)

// Config mirrors wsc-secure's server settings exactly.
type Config struct {
	Port             int
	KerberosPassword string
	AccessKey        string // default "4d324052"
	Logf             func(string, ...any)
}

// Call is one RMC request from a player.
type Call struct {
	PID      uint32
	IP       string
	Port     int
	CallID   uint32
	Protocol uint8
	Method   uint32
	Params   []byte
}

// Reply is what goes back to the player. Empty Payload with OK is a bare success.
type Reply struct {
	Payload []byte
}

// Backend answers calls. The prototype's default echoes an empty success; the real one
// forwards to the main.
type Backend interface {
	Open(pid uint32, ip string, port int)
	Close(pid uint32)
	Handle(c Call) (Reply, error)
}

// Edge is one running terminator.
type Edge struct {
	cfg     Config
	backend Backend
	srv     *nex.Server

	mu      sync.Mutex
	clients map[uint32]*nex.Client // pid -> live client
}

func New(cfg Config, b Backend) *Edge {
	if cfg.AccessKey == "" {
		cfg.AccessKey = "4d324052"
	}
	return &Edge{cfg: cfg, backend: b, clients: map[uint32]*nex.Client{}}
}

func (e *Edge) logf(f string, a ...any) {
	if e.cfg.Logf != nil {
		e.cfg.Logf(f, a...)
	}
}

// Clients returns the PIDs currently connected (for the probes and tests).
func (e *Edge) Clients() map[uint32]*net.UDPAddr {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[uint32]*net.UDPAddr, len(e.clients))
	for pid, c := range e.clients {
		out[pid] = c.Address()
	}
	return out
}

// Serve configures the nex-go server and blocks in Listen (which panics on failure).
func (e *Edge) Serve() {
	srv := nex.NewServer()
	e.srv = srv
	srv.SetPRUDPVersion(1)
	srv.SetPRUDPProtocolMinorVersion(3)
	srv.SetDefaultNEXVersion(&nex.NEXVersion{Major: 3, Minor: 4, Patch: 0})
	srv.SetMatchMakingProtocolVersion(&nex.NEXVersion{Major: 3, Minor: 4, Patch: 0})
	srv.SetKerberosPassword(e.cfg.KerberosPassword)
	srv.SetAccessKey(e.cfg.AccessKey)
	// Same value and reasoning as wsc-secure: real WSC clients never answer a
	// server-initiated PRUDP ping, so this is only a loose safety net.
	srv.SetPingTimeout(3600)

	srv.On("Connect", e.onConnect)
	srv.On("Disconnect", e.onDisconnect)
	srv.On("Data", e.onData)

	e.logf("wscedge: listening on :%d", e.cfg.Port)
	srv.Listen(fmt.Sprintf(":%d", e.cfg.Port))
}

// onConnect is wsc-secure's Connect handler: decrypt the Kerberos ticket, learn the
// session key, verify the check value, acknowledge, switch the session to RC4.
func (e *Edge) onConnect(packet *nex.PacketV1) {
	// A malformed or hostile ticket must never take the edge down: nex-go's stream
	// readers panic on short data.
	defer func() {
		if r := recover(); r != nil {
			e.logf("wscedge: connect from %v rejected: %v", packet.Sender().Address(), r)
		}
	}()
	srv := e.srv
	stream := nex.NewStreamIn(packet.Payload(), srv)

	ticketBytes, err := stream.ReadBuffer()
	if err != nil {
		e.logf("wscedge: connect from %v: bad ticket buffer: %v", packet.Sender().Address(), err)
		return
	}
	serverKey := nex.DeriveKerberosKey(2, []byte(srv.KerberosPassword()))
	decryptedInternal := nex.NewKerberosEncryption(serverKey).Decrypt(ticketBytes)

	internal := nex.NewStreamIn(decryptedInternal, srv)
	_ = internal.ReadDateTime()
	_ = internal.ReadUInt32LE()
	sessionKey := internal.ReadBytesNext(int64(srv.KerberosKeySize()))

	checkData, err := stream.ReadBuffer()
	if err != nil {
		e.logf("wscedge: connect from %v: bad check buffer: %v", packet.Sender().Address(), err)
		return
	}
	check := nex.NewStreamIn(nex.NewKerberosEncryption(sessionKey).Decrypt(checkData), srv)
	pid := check.ReadUInt32LE()
	_ = check.ReadUInt32LE()
	responseCheck := check.ReadUInt32LE()

	client := packet.Sender()
	client.SetPID(pid)

	e.mu.Lock()
	e.clients[pid] = client
	e.mu.Unlock()
	if addr := client.Address(); addr != nil {
		e.backend.Open(pid, addr.IP.String(), addr.Port)
	}

	val := nex.NewStreamOut(srv)
	val.WriteUInt32LE(responseCheck + 1)
	buf := nex.NewStreamOut(srv)
	buf.WriteBuffer(val.Bytes())
	srv.AcknowledgePacket(packet, buf.Bytes())

	client.UpdateRC4Key(sessionKey)
	client.SetSessionKey(sessionKey)
	e.logf("wscedge: connect PID=%d from %v", pid, client.Address())
}

func (e *Edge) onDisconnect(packet *nex.PacketV1) {
	pid := packet.Sender().PID()
	if pid == 0 {
		return
	}
	e.mu.Lock()
	// Stale-disconnect guard, like wsc-secure: only the current client for this PID counts.
	if cur, ok := e.clients[pid]; !ok || cur != packet.Sender() {
		e.mu.Unlock()
		return
	}
	delete(e.clients, pid)
	e.mu.Unlock()
	e.backend.Close(pid)
	e.logf("wscedge: disconnect PID=%d", pid)
}

func (e *Edge) onData(packet *nex.PacketV1) {
	defer func() {
		if r := recover(); r != nil {
			e.logf("wscedge: bad data packet from %v: %v", packet.Sender().Address(), r)
		}
	}()
	req := packet.RMCRequest()
	client := packet.Sender()
	addr := client.Address()
	call := Call{
		PID: client.PID(), CallID: req.CallID(), Protocol: req.ProtocolID(),
		Method: req.MethodID(), Params: req.Parameters(),
	}
	if addr != nil {
		call.IP, call.Port = addr.IP.String(), addr.Port
	}
	// Never block the packet loop: the main may take a while.
	go func() {
		start := time.Now()
		rep, err := e.backend.Handle(call)
		if err != nil {
			e.logf("wscedge: PID=%d proto=%#x method=%#x failed: %v", call.PID, call.Protocol, call.Method, err)
			return
		}
		e.Respond(client, call.Protocol, call.CallID, call.Method, rep.Payload)
		e.logf("wscedge: PID=%d proto=%#x method=%#x answered in %v", call.PID, call.Protocol, call.Method, time.Since(start).Round(time.Millisecond))
	}()
}

// Respond sends an RMC success response to a player: the same packet wsc-secure builds.
func (e *Edge) Respond(client *nex.Client, protocolID uint8, callID uint32, methodID uint32, payload []byte) {
	rmc := nex.NewRMCResponse(protocolID, callID)
	rmc.SetSuccess(methodID, payload)
	pkt, _ := nex.NewPacketV1(client, nil)
	pkt.SetVersion(1)
	pkt.SetSource(0xA1)
	pkt.SetDestination(0xAF)
	pkt.SetType(nex.DataPacket)
	pkt.SetPayload(rmc.Bytes())
	pkt.AddFlag(nex.FlagNeedsAck)
	pkt.AddFlag(nex.FlagReliable)
	e.srv.Send(pkt)
}

// EchoBackend is milestone 1: prove the handshake, acks and keepalives locally.
type EchoBackend struct{ Logf func(string, ...any) }

func (b EchoBackend) Open(pid uint32, ip string, port int) {}
func (b EchoBackend) Close(pid uint32)                     {}
func (b EchoBackend) Handle(c Call) (Reply, error)         { return Reply{}, nil }
