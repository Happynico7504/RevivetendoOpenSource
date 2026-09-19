package nex

import (
	"fmt"
	"net"

	"github.com/PretendoNetwork/nex-go/v2/constants"
	"github.com/PretendoNetwork/nex-go/v2/types"
)

// Local additions for the Wii U Chat edge relay (Revivetendo). They only add capabilities; nothing
// here changes how ordinary connections behave.

// RTT returns the connection's round trip tracker (smoothed time between sending a reliable packet
// and receiving its acknowledgement). It keeps working when a player's router drops ICMP.
func (pc *PRUDPConnection) RTT() *RTT {
	return pc.rtt
}

// NewVirtualConnection creates an established connection for a session that is terminated on
// another host (a relay), registers it with the endpoint so lookups by PID and connection ID find
// it, and returns it. Set OutHook on it to receive what the server sends to the player. It has no
// timers and no sliding windows: nothing here ever touches a socket for it.
func (pep *PRUDPEndPoint) NewVirtualConnection(address net.Addr, pid types.PID, streamType constants.StreamType, streamID uint8) *PRUDPConnection {
	conn := NewPRUDPConnection(NewSocketConnection(pep.Server, address, nil))
	conn.endpoint = pep
	conn.ID = pep.ConnectionIDCounter.Next()
	conn.DefaultPRUDPVersion = 1
	conn.StreamType = streamType
	conn.StreamID = streamID
	conn.StreamSettings = pep.DefaultStreamSettings.Copy()
	conn.ConnectionState = StateConnected
	conn.SetPID(pid)
	pep.Connections.Set(virtualDiscriminator(address, streamType, streamID), conn)
	return conn
}

// RemoveVirtualConnection unregisters a connection made by NewVirtualConnection (only if it is still
// the one registered for its address) and fires the endpoint's connection-ended handlers.
func (pep *PRUDPEndPoint) RemoveVirtualConnection(conn *PRUDPConnection) {
	key := virtualDiscriminator(conn.Socket.Address, conn.StreamType, conn.StreamID)
	removed := false
	pep.Connections.RunAndDelete(key, func(_ string, registered *PRUDPConnection) {
		removed = registered == conn
	})
	if removed {
		pep.emitConnectionEnded(conn)
	}
}

func virtualDiscriminator(address net.Addr, streamType constants.StreamType, streamID uint8) string {
	return fmt.Sprintf("%s-%d-%d", address.String(), streamType, streamID)
}
