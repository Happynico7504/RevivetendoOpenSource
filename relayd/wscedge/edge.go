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

	"github.com/Happynico7504/relaylink"
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
	Custom   uint16 // protocol id extension when Protocol is 0x7f
	Method   uint32
	Params   []byte
}

// Backend is where the edge sends what it terminates. Responses do not come back from
// Handle: the main answers asynchronously, and the answer arrives as Edge.Out. (The echo
// backend used in tests answers itself.)
type Backend interface {
	// Open reports a player who finished the handshake. An error refuses the session: the
	// Connect is not acknowledged, so the console sees a failed connection, exactly as if
	// the server were down.
	Open(pid uint32, ip string, port int) error
	Close(pid uint32)
	Handle(c Call) error
	// Alive reports players heard from since the last call.
	Alive(pids []uint32)
	// Stats reports what the edge measured toward a player (see probe.go).
	Stats(s relaylink.WSCStats)
	// Trace reports a traceroute run toward a player.
	Trace(t relaylink.WSCTrace)
}

// Edge is one running terminator.
type Edge struct {
	cfg     Config
	backend Backend
	srv     *nex.Server

	mu        sync.Mutex
	clients   map[uint32]*nex.Client // pid -> live client
	seen      map[uint32]struct{}    // players heard from since the last liveness report
	lastTrace map[uint32]time.Time   // last loss-triggered traceroute per player
	lastCall  map[uint32]*callNote   // the player's last RMC call and when it was answered
}

type callNote struct {
	proto    uint8
	method   uint32
	at       time.Time
	answered time.Time
}

func New(cfg Config, b Backend) *Edge {
	if cfg.AccessKey == "" {
		cfg.AccessKey = "4d324052"
	}
	return &Edge{cfg: cfg, backend: b, clients: map[uint32]*nex.Client{}, seen: map[uint32]struct{}{}, lastCall: map[uint32]*callNote{}}
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
	srv.On("Kick", e.onKick) // nex-go's own timeout ends a session like a disconnect
	srv.On("Data", e.onData)
	srv.On("Packet", e.onPacket)
	srv.OnRebind(func(c *nex.Client, from, to *net.UDPAddr) {
		e.logf("wscedge: PID=%d moved from %v to %v (its NAT changed the port; session kept)", c.PID(), from, to)
	})
	go e.reportAlive()
	go e.probeLoop()

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

	if addr := client.Address(); addr != nil {
		if err := e.backend.Open(pid, addr.IP.String(), addr.Port); err != nil {
			// Not acknowledged: the console retries and then reports a failed connection.
			e.logf("wscedge: PID=%d refused by the main: %v", pid, err)
			return
		}
	}
	e.mu.Lock()
	e.clients[pid] = client
	e.mu.Unlock()

	val := nex.NewStreamOut(srv)
	val.WriteUInt32LE(responseCheck + 1)
	buf := nex.NewStreamOut(srv)
	buf.WriteBuffer(val.Bytes())
	srv.AcknowledgePacket(packet, buf.Bytes())

	client.UpdateRC4Key(sessionKey)
	client.SetSessionKey(sessionKey)
	e.logf("wscedge: connect PID=%d from %v", pid, client.Address())
	if addr := client.Address(); addr != nil && addr.IP != nil {
		// A path snapshot from the moment they connect, to compare a later degradation with.
		e.traceAsync(pid, addr.IP.String(), "connect")
	}
}

func (e *Edge) onKick(packet *nex.PacketV1) { e.endSession(packet, "nex-go timed it out") }

func (e *Edge) onDisconnect(packet *nex.PacketV1) {
	why := "console sent DISCONNECT"
	if packet.Type() == nex.SynPacket {
		why = "console opened a new connection from the same address"
	}
	e.endSession(packet, why)
}

// endSession closes a player's session, logging why and what the edge still had in flight to
// it: a console also gives up when the server's answers do not reach it.
func (e *Edge) endSession(packet *nex.PacketV1, why string) {
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
	last := "no call seen"
	if n := e.lastCall[pid]; n != nil {
		state := "unanswered"
		if !n.answered.IsZero() {
			state = fmt.Sprintf("answered after %v", n.answered.Sub(n.at).Round(time.Millisecond))
		}
		last = fmt.Sprintf("last call proto=%#x method=%#x %v ago, %s", n.proto, n.method, time.Since(n.at).Round(time.Second), state)
		delete(e.lastCall, pid)
	}
	e.mu.Unlock()
	e.backend.Close(pid)
	count, oldest, retries := packet.Sender().PendingStats()
	e.logf("wscedge: disconnect PID=%d (%s; %d unacknowledged, oldest %v, resent up to %dx; %s)", pid, why, count, oldest.Round(time.Millisecond), retries, last)
}

func (e *Edge) onData(packet *nex.PacketV1) {
	defer func() {
		if r := recover(); r != nil {
			e.logf("wscedge: bad data packet from %v: %v", packet.Sender().Address(), r)
		}
	}()
	req := packet.RMCRequest()
	client := packet.Sender()
	e.mu.Lock()
	cur := e.clients[client.PID()]
	e.mu.Unlock()
	if client.PID() == 0 || cur != client {
		return // never completed the handshake here (or was refused by the main)
	}
	e.mu.Lock()
	e.lastCall[client.PID()] = &callNote{proto: req.ProtocolID(), method: req.MethodID(), at: time.Now()}
	e.mu.Unlock()
	addr := client.Address()
	call := Call{
		PID: client.PID(), CallID: req.CallID(), Protocol: req.ProtocolID(), Custom: req.CustomID(),
		Method: req.MethodID(), Params: req.Parameters(),
	}
	if addr != nil {
		call.IP, call.Port = addr.IP.String(), addr.Port
	}
	// Never block the packet loop: the main may take a while.
	go func() {
		if err := e.backend.Handle(call); err != nil {
			e.logf("wscedge: PID=%d proto=%#x method=%#x failed: %v", call.PID, call.Protocol, call.Method, err)
		}
	}()
}

// onPacket notes that a player is alive: every packet counts, including the keepalive pings
// the edge answers locally and the main never sees.
func (e *Edge) onPacket(packet *nex.PacketV1) {
	if pid := packet.Sender().PID(); pid != 0 {
		e.mu.Lock()
		e.seen[pid] = struct{}{}
		e.mu.Unlock()
	}
}

// AliveEvery is how often liveness is reported to the main.
var AliveEvery = 2 * time.Second

func (e *Edge) reportAlive() {
	for range time.Tick(AliveEvery) {
		e.mu.Lock()
		pids := make([]uint32, 0, len(e.seen))
		for pid := range e.seen {
			if _, live := e.clients[pid]; live {
				pids = append(pids, pid)
			}
		}
		e.seen = map[uint32]struct{}{}
		e.mu.Unlock()
		if len(pids) > 0 {
			e.backend.Alive(pids)
		}
	}
}

// Out delivers an RMC message from the main (a response or a notification) to a player.
func (e *Edge) Out(pid uint32, payload []byte) {
	e.mu.Lock()
	client := e.clients[pid]
	if n := e.lastCall[pid]; n != nil && n.answered.IsZero() {
		n.answered = time.Now() // any message from the main after the call (normally its answer)
	}
	e.mu.Unlock()
	if client == nil {
		e.logf("wscedge: message for PID=%d, who is not connected here", pid)
		return
	}
	e.send(client, payload)
}

// Reset drops every session without telling the backend: the main has already closed them
// (its stream to this relay was lost), and the consoles reconnect and handshake again.
func (e *Edge) Reset() {
	e.mu.Lock()
	old := e.clients
	e.clients = map[uint32]*nex.Client{}
	e.mu.Unlock()
	for _, c := range old {
		e.srv.Kick(c)
	}
	e.logf("wscedge: reset: dropped %d sessions", len(old))
}

// Respond sends an RMC success response to a player: the same packet wsc-secure builds.
func (e *Edge) Respond(client *nex.Client, protocolID uint8, callID uint32, methodID uint32, payload []byte) {
	rmc := nex.NewRMCResponse(protocolID, callID)
	rmc.SetSuccess(methodID, payload)
	e.send(client, rmc.Bytes())
}

// send puts a complete RMC message into a reliable data packet for the player.
func (e *Edge) send(client *nex.Client, rmcBytes []byte) {
	pkt, _ := nex.NewPacketV1(client, nil)
	pkt.SetVersion(1)
	pkt.SetSource(0xA1)
	pkt.SetDestination(0xAF)
	pkt.SetType(nex.DataPacket)
	pkt.SetPayload(rmcBytes)
	pkt.AddFlag(nex.FlagNeedsAck)
	pkt.AddFlag(nex.FlagReliable)
	e.srv.Send(pkt)
}

// EchoBackend answers every call with an empty success, itself: the handshake, acks and
// keepalives are still terminated locally. Set Edge after New.
type EchoBackend struct{ Edge *Edge }

func (b *EchoBackend) Open(pid uint32, ip string, port int) error { return nil }
func (b *EchoBackend) Close(pid uint32)                           {}
func (b *EchoBackend) Alive(pids []uint32)                        {}
func (b *EchoBackend) Stats(s relaylink.WSCStats)                 {}
func (b *EchoBackend) Trace(t relaylink.WSCTrace)                 {}
func (b *EchoBackend) Handle(c Call) error {
	b.Edge.mu.Lock()
	client := b.Edge.clients[c.PID]
	b.Edge.mu.Unlock()
	if client != nil {
		b.Edge.Respond(client, c.Protocol, c.CallID, c.Method, nil)
	}
	return nil
}
