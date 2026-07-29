package main

import (
	"fmt"
	"os"

	nex "github.com/PretendoNetwork/nex-go"
	"github.com/PretendoNetwork/nex-protocols-common-go/authentication"
)

var nexServer *nex.Server

func main() {
	nexServer = nex.NewServer()
	nexServer.SetPRUDPVersion(1)
	nexServer.SetPRUDPProtocolMinorVersion(3)
	nexServer.SetDefaultNEXVersion(&nex.NEXVersion{
		Major: 3,
		Minor: 5,
		Patch: 4,
	})
	nexServer.SetKerberosPassword(os.Getenv("KERBEROS_PASSWORD"))
	nexServer.SetAccessKey("LEH7EKJRIp207NT0")

	nexServer.On("Data", func(packet *nex.PacketV1) {
		request := packet.RMCRequest()

		fmt.Println("==WSC - Auth==")
		fmt.Printf("Protocol ID: %#v\n", request.ProtocolID())
		fmt.Printf("Method ID: %#v\n", request.MethodID())
		fmt.Println("===============")
	})

	authenticationProtocol := authentication.NewCommonAuthenticationProtocol(nexServer)

	secureStationURL := nex.NewStationURL("")
	secureStationURL.SetScheme("prudps")
	secureStationURL.SetAddress(os.Getenv("SECURE_SERVER_LOCATION"))
	secureStationURL.SetPort(os.Getenv("SECURE_SERVER_PORT"))
	secureStationURL.SetCID("1")
	secureStationURL.SetPID("2")
	secureStationURL.SetSID("1")
	secureStationURL.SetStream("10")
	secureStationURL.SetType("2")

	authenticationProtocol.SetSecureStationURL(secureStationURL)
	authenticationProtocol.SetBuildName("Pretendo WSC")
	authenticationProtocol.SetPasswordFromPIDFunction(passwordFromPID)

	nexServer.Listen(":60014")
}
