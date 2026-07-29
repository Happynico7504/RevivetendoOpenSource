package main

import (
	"fmt"
	"os"
	"strconv"

	nex "github.com/PretendoNetwork/nex-go"
	"github.com/PretendoNetwork/nex-protocols-go/datastore"
	secure_connection "github.com/PretendoNetwork/nex-protocols-go/secure-connection"
)

var nexServer *nex.Server

func main() {
	nexServer = nex.NewServer()
	nexServer.SetPRUDPVersion(1)
	nexServer.SetPRUDPProtocolMinorVersion(3)
	nexServer.SetDefaultNEXVersion(&nex.NEXVersion{
		Major: 3,
		Minor: 4,
		Patch: 0,
	})
	nexServer.SetKerberosPassword(os.Getenv("KERBEROS_PASSWORD"))
	nexServer.SetAccessKey("4d324052")

	nexServer.On("Connect", func(packet *nex.PacketV1) {
		payload := packet.Payload()
		stream := nex.NewStreamIn(payload, nexServer)

		ticketBytes, _ := stream.ReadBuffer()
		serverKey := nex.DeriveKerberosKey(2, []byte(nexServer.KerberosPassword()))
		ticketCipher := nex.NewKerberosEncryption(serverKey)
		decryptedInternal := ticketCipher.Decrypt(ticketBytes)

		internalStream := nex.NewStreamIn(decryptedInternal, nexServer)
		_ = internalStream.ReadDateTime()
		_ = internalStream.ReadUInt32LE()
		sessionKey := internalStream.ReadBytesNext(int64(nexServer.KerberosKeySize()))

		checkData, _ := stream.ReadBuffer()
		checkCipher := nex.NewKerberosEncryption(sessionKey)
		checkDataDecrypted := checkCipher.Decrypt(checkData)

		checkStream := nex.NewStreamIn(checkDataDecrypted, nexServer)
		userPID := checkStream.ReadUInt32LE()
		_ = checkStream.ReadUInt32LE() // CID
		responseCheck := checkStream.ReadUInt32LE()

		packet.Sender().SetPID(userPID)

		responseValueStream := nex.NewStreamOut(nexServer)
		responseValueStream.WriteUInt32LE(responseCheck + 1)
		responseValueBufferStream := nex.NewStreamOut(nexServer)
		responseValueBufferStream.WriteBuffer(responseValueStream.Bytes())

		nexServer.AcknowledgePacket(packet, responseValueBufferStream.Bytes())

		packet.Sender().UpdateRC4Key(sessionKey)
		packet.Sender().SetSessionKey(sessionKey)

		fmt.Printf("Connect: PID=%d\n", userPID)
	})

	nexServer.On("Data", func(packet *nex.PacketV1) {
		request := packet.RMCRequest()
		fmt.Printf("==WSC Secure== proto=%#v method=%#v\n", request.ProtocolID(), request.MethodID())
	})

	secureProto := secure_connection.NewSecureConnectionProtocol(nexServer)
	secureProto.Register(register)
	secureProto.ReplaceURL(replaceURL)

	dsProto := datastore.NewDataStoreProtocol(nexServer)
	dsProto.SearchObject(searchObject)

	nexServer.Listen(":60015")
}

func register(err error, client *nex.Client, callID uint32, stationUrls []*nex.StationURL) {
	if err != nil {
		fmt.Println("Register error:", err)
		return
	}

	localStation := stationUrls[0]
	localStationURL := localStation.EncodeToString()

	connectionID := uint32(nexServer.ConnectionIDCounter().Increment())
	client.SetConnectionID(connectionID)
	client.SetLocalStationURL(localStationURL)

	address := client.Address().IP.String()
	port := strconv.Itoa(client.Address().Port)

	localStation.SetAddress(address)
	localStation.SetPort(port)
	localStation.SetNatf("0")
	localStation.SetNatm("0")
	localStation.SetType("3")

	globalStationURL := localStation.EncodeToString()

	fmt.Printf("Register: PID=%d addr=%s:%s connID=%d\n", client.PID(), address, port, connectionID)

	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteUInt32LE(0x10001)
	rmcResponseStream.WriteUInt32LE(connectionID)
	rmcResponseStream.WriteString(globalStationURL)

	rmcResponse := nex.NewRMCResponse(secure_connection.ProtocolID, callID)
	rmcResponse.SetSuccess(secure_connection.MethodRegister, rmcResponseStream.Bytes())

	responsePacket, _ := nex.NewPacketV1(client, nil)
	responsePacket.SetVersion(1)
	responsePacket.SetSource(0xA1)
	responsePacket.SetDestination(0xAF)
	responsePacket.SetType(nex.DataPacket)
	responsePacket.SetPayload(rmcResponse.Bytes())
	responsePacket.AddFlag(nex.FlagNeedsAck)
	responsePacket.AddFlag(nex.FlagReliable)

	nexServer.Send(responsePacket)
}

func replaceURL(err error, client *nex.Client, callID uint32, oldStation *nex.StationURL, newStation *nex.StationURL) {
	station := newStation
	if err != nil {
		fmt.Println("ReplaceURL error:", err)
		return
	}

	address := client.Address().IP.String()
	port := strconv.Itoa(client.Address().Port)

	station.SetAddress(address)
	station.SetPort(port)
	station.SetNatf("0")
	station.SetNatm("0")
	station.SetType("3")

	client.SetLocalStationURL(station.EncodeToString())

	fmt.Printf("ReplaceURL: PID=%d addr=%s:%s\n", client.PID(), address, port)

	rmcResponse := nex.NewRMCResponse(secure_connection.ProtocolID, callID)
	rmcResponse.SetSuccess(secure_connection.MethodReplaceURL, []byte{})

	responsePacket, _ := nex.NewPacketV1(client, nil)
	responsePacket.SetVersion(1)
	responsePacket.SetSource(0xA1)
	responsePacket.SetDestination(0xAF)
	responsePacket.SetType(nex.DataPacket)
	responsePacket.SetPayload(rmcResponse.Bytes())
	responsePacket.AddFlag(nex.FlagNeedsAck)
	responsePacket.AddFlag(nex.FlagReliable)

	nexServer.Send(responsePacket)
}

func searchObject(err error, client *nex.Client, callID uint32, param *datastore.DataStoreSearchParam) {
	if err != nil {
		fmt.Println("SearchObject error:", err)
		return
	}

	fmt.Printf("SearchObject: PID=%d\n", client.PID())

	result := datastore.NewDataStoreSearchResult()
	result.TotalCount = 0
	result.TotalCountType = 1

	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteStructure(result)

	rmcResponse := nex.NewRMCResponse(datastore.ProtocolID, callID)
	rmcResponse.SetSuccess(datastore.MethodSearchObject, rmcResponseStream.Bytes())

	responsePacket, _ := nex.NewPacketV1(client, nil)
	responsePacket.SetVersion(1)
	responsePacket.SetSource(0xA1)
	responsePacket.SetDestination(0xAF)
	responsePacket.SetType(nex.DataPacket)
	responsePacket.SetPayload(rmcResponse.Bytes())
	responsePacket.AddFlag(nex.FlagNeedsAck)
	responsePacket.AddFlag(nex.FlagReliable)

	nexServer.Send(responsePacket)
}
