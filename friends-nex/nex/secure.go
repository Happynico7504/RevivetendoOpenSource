package nex

import (
	"fmt"
	"os"
	"strconv"

	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/friends-nex/database"
	"github.com/PretendoNetwork/friends-nex/globals"
)

func StartSecureServer() {
	globals.SecureServer = nex.NewPRUDPServer()
	globals.SecureEndpoint = nex.NewPRUDPEndPoint(1)
	globals.SecureEndpoint.ServerAccount = globals.SecureServerAccount
	globals.SecureEndpoint.AccountDetailsByPID = globals.AccountDetailsByPID
	globals.SecureEndpoint.AccountDetailsByUsername = globals.AccountDetailsByUsername
	globals.SecureEndpoint.IsSecureEndPoint = true
	globals.SecureServer.BindPRUDPEndPoint(globals.SecureEndpoint)

	globals.SecureServer.LibraryVersions.SetDefault(nex.NewLibraryVersion(1, 1, 0))
	globals.SecureServer.AccessKey = "ridfebb9"
	globals.SecureServer.ByteStreamSettings.UseStructureHeader = false
	globals.SecureServer.SessionKeyLength = 16
	globals.SecureServer.SetFragmentSize(962)

	globals.SecureEndpoint.OnDisconnect(func(packet nex.PacketInterface) {
		pid := uint64(packet.Sender().PID())
		globals.ConnectedClients.Delete(pid)
		database.SetOnlineStatus(pid, false)
		go database.StopPretendoPresence(pid)
		conn := packet.Sender().(*nex.PRUDPConnection)
		fmt.Printf("[FRIENDS-SECURE] PID=%d disconnected (state=%d sessionKey=%x)\n", pid, conn.ConnectionState, conn.SessionKey)
	})

	registerCommonSecureServerProtocols()

	port, _ := strconv.Atoi(os.Getenv("PN_FRIENDS_SECURE_SERVER_PORT"))
	fmt.Printf("[FRIENDS-SECURE] listening on port %d\n", port)
	globals.SecureServer.Listen(port)
}
