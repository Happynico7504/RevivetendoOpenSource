package nex_secure_connection_nintendo_badge_arcade

import (
	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/globals"
	secure_connection_nintendo_badge_arcade "github.com/PretendoNetwork/nex-protocols-go/secure-connection/nintendo-badge-arcade"

	"github.com/PretendoNetwork/nex-go"
)

func GetMaintenanceStatus(err error, client *nex.Client, callID uint32) {
	// Stock Pretendo code hardcoded maintenanceStatus=0xFFFF here (a "TODO: don't
	// hardcode" stub) which produced a real-hardware error (004-3003) right after
	// this call - 0xFFFF looks like an uninitialized/all-bits sentinel rather than
	// a real "not in maintenance" value, so use 0 instead until real captured
	// traffic confirms the correct encoding.
	var maintenanceStatus uint16 = 0
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
