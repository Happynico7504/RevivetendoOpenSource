package nex

import (
	"log"
	"os"

	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/database"
	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/globals"
	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/prudp"

	"github.com/PretendoNetwork/nex-go"
)

func StartNEXServer() {
	globals.NEXServer = nex.NewServer()
	globals.NEXServer.SetPRUDPVersion(1)
	globals.NEXServer.SetPRUDPProtocolMinorVersion(3)
	globals.NEXServer.SetDefaultNEXVersion(&nex.NEXVersion{
		Major: 3,
		Minor: 7,
		Patch: 16,
	})
	globals.NEXServer.SetKerberosPassword(os.Getenv("KERBEROS_PASSWORD"))
	globals.NEXServer.SetAccessKey("82d5962d")

	globals.NEXServer.On("Data", func(packet *nex.PacketV1) {
		request := packet.RMCRequest()
		client := packet.Sender()
		params := request.Parameters()
		shown := params
		if len(shown) > 96 {
			shown = shown[:96]
		}

		// Logs every incoming RMC call, handled or not, so a missing or
		// unexpected call (e.g. a buy-plays request) is visible.
		log.Printf("secure RMC: pid=%d addr=%s proto=%#x custom=%#x method=%#x call=%d params=%dB hex=%x",
			client.PID(), client.Address(), request.ProtocolID(), request.CustomID(), request.MethodID(), request.CallID(), len(params), shown)
	})

	globals.NEXServer.On("Kick", func(packet *nex.PacketV1) {
		log.Printf("secure kick: pid=%d addr=%s", packet.Sender().PID(), packet.Sender().Address())
		database.DeletePlayerSession(packet.Sender().PID())
	})

	globals.NEXServer.On("Connect", prudp.Connect)

	registerNEXProtocols()

	globals.NEXServer.Listen(":60019")
}
