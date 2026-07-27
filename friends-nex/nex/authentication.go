package nex

import (
	"fmt"
	"os"
	"strconv"

	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/friends-nex/globals"
)

func StartAuthenticationServer() {
	globals.AuthenticationServer = nex.NewPRUDPServer()
	globals.AuthenticationEndpoint = nex.NewPRUDPEndPoint(1)
	globals.AuthenticationEndpoint.ServerAccount = globals.AuthenticationServerAccount
	globals.AuthenticationEndpoint.AccountDetailsByPID = globals.AccountDetailsByPID
	globals.AuthenticationEndpoint.AccountDetailsByUsername = globals.AccountDetailsByUsername
	globals.AuthenticationServer.BindPRUDPEndPoint(globals.AuthenticationEndpoint)

	globals.AuthenticationServer.LibraryVersions.SetDefault(nex.NewLibraryVersion(1, 1, 0))
	globals.AuthenticationServer.AccessKey = "ridfebb9"
	globals.AuthenticationServer.ByteStreamSettings.UseStructureHeader = false
	globals.AuthenticationServer.SessionKeyLength = 16
	globals.AuthenticationServer.SetFragmentSize(962)

	registerCommonAuthenticationServerProtocols()

	port, _ := strconv.Atoi(os.Getenv("PN_FRIENDS_AUTHENTICATION_SERVER_PORT"))
	fmt.Printf("[FRIENDS-AUTH] listening on port %d\n", port)
	globals.AuthenticationServer.Listen(port)
}
