package relaylink

// Wire types of the WSC edge: a relay terminates a player's PRUDP session and passes complete
// RMC calls to the main over the real-time stream (see relayd/deploy/WSC-EDGE.md).
const (
	MethodWSCOpen  = "wsc.open"  // relay -> hub call: a player finished the handshake on this relay
	MethodWSCRMC   = "wsc.rmc"   // relay -> hub call: one RMC request from a player
	MethodWSCAlive = "wsc.alive" // relay -> hub call: players this relay heard from recently
	MethodWSCClose = "wsc.close" // relay -> hub call: a player's session ended
	TopicWSCOut    = "wsc.out"   // hub -> relay event: an RMC message for a player
)

// WSCOpen announces a player whose Kerberos handshake the relay has verified. IP and Port
// are the player's public address as the relay sees it (what the game puts in station URLs).
type WSCOpen struct {
	PID  uint32 `json:"pid"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// WSCRMC is one decoded RMC request.
type WSCRMC struct {
	PID    uint32 `json:"pid"`
	Call   uint32 `json:"call"`
	Proto  uint8  `json:"proto"`
	Custom uint16 `json:"custom,omitempty"` // the 16-bit protocol id that follows proto 0x7f (WSC's club protocol is 0x83)
	Method uint32 `json:"method"`
	Params []byte `json:"params"`
}

// WSCAlive lists players whose packets the relay saw since its last report; it stands in
// for the packet stream the main's stale-connection detection normally watches.
type WSCAlive struct {
	PIDs []uint32 `json:"pids"`
}

// WSCClose ends a session (the player disconnected or went stale).
type WSCClose struct {
	PID uint32 `json:"pid"`
}

// WSCOut is a plain RMC message (a response or a server-initiated notification) for a
// player. The relay encrypts and sends it over the player's PRUDP session.
type WSCOut struct {
	PID     uint32 `json:"pid"`
	Payload []byte `json:"payload"`
}

// EdgePipeMsg is the line protocol between relayd and the edge child process (JSON lines
// over inherited descriptors 3, parent -> child, and 4, child -> parent).
//
// Child to parent: open, rmc (both wait for an "ack"), alive, close.
// Parent to child: ack (answers an open or rmc by ID), out (an RMC message for a player),
// reset (the stream to the main was lost: the main has closed every session, so drop them).
type EdgePipeMsg struct {
	T       string   `json:"t"`
	ID      uint64   `json:"id,omitempty"`
	PID     uint32   `json:"pid,omitempty"`
	IP      string   `json:"ip,omitempty"`
	Port    int      `json:"port,omitempty"`
	Call    uint32   `json:"call,omitempty"`
	Proto   uint8    `json:"proto,omitempty"`
	Custom  uint16   `json:"custom,omitempty"`
	Method  uint32   `json:"method,omitempty"`
	Params  []byte   `json:"params,omitempty"`
	PIDs    []uint32 `json:"pids,omitempty"`
	Payload []byte   `json:"payload,omitempty"`
	Err     string   `json:"err,omitempty"`
}

// EdgePipe message types.
const (
	EdgeOpen  = "open"
	EdgeRMC   = "rmc"
	EdgeAlive = "alive"
	EdgeClose = "close"
	EdgeAck   = "ack"
	EdgeOut   = "out"
	EdgeReset = "reset"
)
