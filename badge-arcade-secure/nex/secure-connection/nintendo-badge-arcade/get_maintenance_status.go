package nex_secure_connection_nintendo_badge_arcade

import (
	secure_connection_nintendo_badge_arcade "github.com/PretendoNetwork/nex-protocols-go/secure-connection/nintendo-badge-arcade"
	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/globals"

	"github.com/PretendoNetwork/nex-go"
)

func GetMaintenanceStatus(err error, client *nex.Client, callID uint32) {
	// Stock Pretendo value. 2026-09-16 bring-up changed this to 0, blaming it for
	// a 004-3003 - but 004 is a SpotPass/BOSS error series, and that one was
	// really the startup BOSS task 404ing (fixed by serving archived content).
	// Restored 2026-10-05 while chasing new players stalling right after this
	// call; the real encoding is still unconfirmed by captured traffic.
	var maintenanceStatus uint16 = 0xFFFF
	var maintenanceTime uint32 = 0
	var isSuccess bool = true

	rmcResponseStream := nex.NewStreamOut(globals.NEXServer)

	rmcResponseStream.WriteUInt16LE(maintenanceStatus)
	rmcResponseStream.WriteUInt32LE(maintenanceTime)
	rmcResponseStream.WriteBool(isSuccess)

	rmcResponseBody := rmcResponseStream.Bytes()

	rmcResponse := nex.NewRMCResponse(secure_connection_nintendo_badge_arcade.ProtocolID, callID)
	rmcResponse.SetSuccess(secure_connection_nintendo_badge_arcade.MethodGetMaintenanceStatus, rmcResponseBody)

	rmcResponseBytes := rmcResponse.Bytes()

	responsePacket, _ := nex.NewPacketV1(client, nil)

	responsePacket.SetVersion(1)
	responsePacket.SetSource(0xA1)
	responsePacket.SetDestination(0xAF)
	responsePacket.SetType(nex.DataPacket)
	responsePacket.SetPayload(rmcResponseBytes)

	responsePacket.AddFlag(nex.FlagNeedsAck)
	responsePacket.AddFlag(nex.FlagReliable)

	globals.NEXServer.Send(responsePacket)
}
