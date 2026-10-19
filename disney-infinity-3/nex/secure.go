package nex

import (
	"fmt"
	common_globals "github.com/PretendoNetwork/nex-protocols-common-go/v2/globals"
	"os"
	"strconv"

	"github.com/PretendoNetwork/disney-infinity-3-nex/globals"
	"github.com/PretendoNetwork/nex-go/v2"
)

func StartSecureServer() {
	globals.SecureServer = nex.NewPRUDPServer()
	globals.SecureServer.ByteStreamSettings.UseStructureHeader = true

	globals.SecureEndpoint = nex.NewPRUDPEndPoint(1)
	globals.SecureEndpoint.IsSecureEndPoint = true
	globals.SecureEndpoint.ServerAccount = globals.SecureServerAccount
	globals.SecureEndpoint.AccountDetailsByPID = globals.AccountDetailsByPID
	globals.SecureEndpoint.AccountDetailsByUsername = globals.AccountDetailsByUsername
	globals.SecureServer.BindPRUDPEndPoint(globals.SecureEndpoint)

	globals.SecureServer.LibraryVersions.SetDefault(nex.NewLibraryVersion(3, 9, 1))
	globals.SecureServer.AccessKey = "83a601e5"

	globals.SecureEndpoint.OnData(func(packet nex.PacketInterface) {
		request := packet.RMCMessage()

		fmt.Println("==Disney Infinity 3.0 - Secure==")
		fmt.Printf("Protocol ID: %d\n", request.ProtocolID)
		fmt.Printf("Method ID: %d\n", request.MethodID)
		fmt.Println("===============")
	})

	globals.SecureEndpoint.OnError(func(err *nex.Error) {
		globals.Logger.Errorf("Secure: %v", err)
	})

	globals.MatchmakingManager = common_globals.NewMatchmakingManager(globals.SecureEndpoint, globals.Postgres)
	globals.MessagingManager = common_globals.NewMessagingManager(globals.SecureEndpoint, globals.Postgres)
	globals.UtilityManager = common_globals.NewUtilityManager(globals.SecureEndpoint, globals.Postgres)
	globals.DatastoreManager = common_globals.NewDataStoreManager(globals.SecureEndpoint, globals.Postgres)
	globals.DatastoreManager.SetS3Config(globals.S3Bucket, globals.S3KeyBase, globals.S3Manager)
	globals.DatastoreManager.GetUserFriendPIDs = globals.GetUserFriendPIDs

	registerCommonSecureServerProtocols()

	port, _ := strconv.Atoi(os.Getenv("PN_INFINITY3_SECURE_SERVER_PORT"))

	globals.SecureServer.Listen(port)
}
