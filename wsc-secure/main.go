package main

import (
	"fmt"
	"os"
	"strconv"

	nex "github.com/PretendoNetwork/nex-go"
	"github.com/PretendoNetwork/nex-protocols-go/datastore"
	match_making "github.com/PretendoNetwork/nex-protocols-go/match-making"
	match_making_ext "github.com/PretendoNetwork/nex-protocols-go/match-making-ext"
	matchmake_extension "github.com/PretendoNetwork/nex-protocols-go/matchmake-extension"
	nat_traversal "github.com/PretendoNetwork/nex-protocols-go/nat-traversal"
	secure_connection "github.com/PretendoNetwork/nex-protocols-go/secure-connection"
)

var nexServer *nex.Server

func main() {
	connectDB()

	nexServer = nex.NewServer()
	nexServer.SetPRUDPVersion(1)
	nexServer.SetPRUDPProtocolMinorVersion(3)
	nexServer.SetDefaultNEXVersion(&nex.NEXVersion{Major: 3, Minor: 4, Patch: 0})
	nexServer.SetMatchMakingProtocolVersion(&nex.NEXVersion{Major: 3, Minor: 4, Patch: 0})
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
		_ = checkStream.ReadUInt32LE()
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

	mmExtProto := matchmake_extension.NewMatchmakeExtensionProtocol(nexServer)
	mmExtProto.AutoMatchmakeWithSearchCriteria_Postpone(autoMatchmake)

	mmProto := match_making.NewMatchMakingProtocol(nexServer)
	mmProto.GetSessionURLs(getSessionURLs)
	mmProto.UpdateSessionHostV1(updateSessionHostV1)

	mmExtProto2 := match_making_ext.NewMatchMakingExtProtocol(nexServer)
	mmExtProto2.EndParticipation(endParticipation)

	natProto := nat_traversal.NewNATTraversalProtocol(nexServer)
	natProto.RequestProbeInitiationExt(requestProbeInitiationExt)
	natProto.ReportNATProperties(reportNATProperties)

	nexServer.Listen(":60015")
}

func sendResponse(client *nex.Client, protocolID uint8, callID uint32, methodID uint32, payload []byte) {
	rmcResponse := nex.NewRMCResponse(protocolID, callID)
	rmcResponse.SetSuccess(methodID, payload)
	pkt, _ := nex.NewPacketV1(client, nil)
	pkt.SetVersion(1)
	pkt.SetSource(0xA1)
	pkt.SetDestination(0xAF)
	pkt.SetType(nex.DataPacket)
	pkt.SetPayload(rmcResponse.Bytes())
	pkt.AddFlag(nex.FlagNeedsAck)
	pkt.AddFlag(nex.FlagReliable)
	nexServer.Send(pkt)
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

	dbUpsertSession(client.PID(), []string{localStationURL, globalStationURL}, address, port)

	fmt.Printf("Register: PID=%d addr=%s:%s connID=%d\n", client.PID(), address, port, connectionID)

	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteUInt32LE(0x10001)
	rmcResponseStream.WriteUInt32LE(connectionID)
	rmcResponseStream.WriteString(globalStationURL)

	sendResponse(client, secure_connection.ProtocolID, callID, secure_connection.MethodRegister, rmcResponseStream.Bytes())
}

func replaceURL(err error, client *nex.Client, callID uint32, oldStation *nex.StationURL, newStation *nex.StationURL) {
	if err != nil {
		fmt.Println("ReplaceURL error:", err)
		return
	}

	address := client.Address().IP.String()
	port := strconv.Itoa(client.Address().Port)

	newStation.SetAddress(address)
	newStation.SetPort(port)
	newStation.SetNatf("0")
	newStation.SetNatm("0")
	newStation.SetType("3")

	newURL := newStation.EncodeToString()
	dbUpdateSessionURL(client.PID(), oldStation.EncodeToString(), newURL)
	client.SetLocalStationURL(newURL)

	fmt.Printf("ReplaceURL: PID=%d addr=%s:%s\n", client.PID(), address, port)

	sendResponse(client, secure_connection.ProtocolID, callID, secure_connection.MethodReplaceURL, []byte{})
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

	sendResponse(client, datastore.ProtocolID, callID, datastore.MethodSearchObject, rmcResponseStream.Bytes())
}

func autoMatchmake(err error, client *nex.Client, callID uint32, session *match_making.MatchmakeSession, message string) {
	if err != nil {
		fmt.Println("AutoMatchmake error:", err)
		return
	}

	gameMode := session.GameMode
	maxPlayers := uint32(session.MaximumParticipants)
	if maxPlayers == 0 {
		maxPlayers = 2
	}

	gid := dbFindGathering(gameMode)
	if gid == 0 {
		gid = dbNewGathering(client.PID(), gameMode, maxPlayers)
		fmt.Printf("AutoMatchmake: PID=%d created gathering gid=%d gameMode=%d\n", client.PID(), gid, gameMode)
	} else {
		dbJoinGathering(gid, client.PID())
		fmt.Printf("AutoMatchmake: PID=%d joined gathering gid=%d gameMode=%d\n", client.PID(), gid, gameMode)
	}

	hostPID := dbGetGatheringHost(gid)

	session.Gathering.ID = gid
	session.Gathering.OwnerPID = hostPID
	session.Gathering.HostPID = hostPID
	session.Gathering.MinimumParticipants = 1
	session.SessionKey = make([]byte, 16)

	// Encode DataHolder response: TypeName + lengths + Gathering + MatchmakeSession
	contentStream := nex.NewStreamOut(nexServer)
	contentStream.WriteStructure(session.Gathering)
	contentStream.WriteStructure(session)
	content := contentStream.Bytes()

	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteString("MatchmakeSession")
	rmcResponseStream.WriteUInt32LE(uint32(len(content)) + 4)
	rmcResponseStream.WriteUInt32LE(uint32(len(content)))
	rmcResponseStream.Grow(int64(len(content)))
	rmcResponseStream.WriteBytesNext(content)

	sendResponse(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodAutoMatchmakeWithSearchCriteria_Postpone, rmcResponseStream.Bytes())
}

func getSessionURLs(err error, client *nex.Client, callID uint32, gatheringID uint32) {
	if err != nil {
		fmt.Println("GetSessionURLs error:", err)
		return
	}

	hostPID := dbGetGatheringHost(gatheringID)
	urls := dbGetPlayerURLs(hostPID)
	if urls == nil {
		urls = []string{}
	}

	fmt.Printf("GetSessionURLs: gid=%d hostPID=%d urls=%v\n", gatheringID, hostPID, urls)

	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteListString(urls)

	sendResponse(client, match_making.ProtocolID, callID, match_making.MethodGetSessionURLs, rmcResponseStream.Bytes())
}

func updateSessionHostV1(err error, client *nex.Client, callID uint32, gid uint32) {
	if err != nil {
		fmt.Println("UpdateSessionHostV1 error:", err)
		return
	}

	dbUpdateGatheringHost(gid, client.PID())
	fmt.Printf("UpdateSessionHostV1: gid=%d newHost=%d\n", gid, client.PID())

	sendResponse(client, match_making.ProtocolID, callID, match_making.MethodUpdateSessionHostV1, nil)
}

func endParticipation(err error, client *nex.Client, callID uint32, gid uint32, message string) {
	if err != nil {
		fmt.Println("EndParticipation error:", err)
		return
	}

	dbLeaveGathering(gid, client.PID())
	fmt.Printf("EndParticipation: PID=%d gid=%d\n", client.PID(), gid)

	sendResponse(client, match_making_ext.ProtocolID, callID, match_making_ext.MethodEndParticipation, []byte{0x1})
}

func requestProbeInitiationExt(err error, client *nex.Client, callID uint32, targetList []string, stationToProbe string) {
	if err != nil {
		fmt.Println("RequestProbeInitiationExt error:", err)
		return
	}

	fmt.Printf("RequestProbeInitiationExt: PID=%d targets=%v probe=%s\n", client.PID(), targetList, stationToProbe)

	sendResponse(client, nat_traversal.ProtocolID, callID, nat_traversal.MethodRequestProbeInitiationExt, nil)

	// Forward InitiateProbe to each target
	rmcMessage := nex.RMCRequest{}
	rmcMessage.SetProtocolID(nat_traversal.ProtocolID)
	rmcMessage.SetCallID(0xffff0000 + callID)
	rmcMessage.SetMethodID(nat_traversal.MethodInitiateProbe)
	probeStream := nex.NewStreamOut(nexServer)
	probeStream.WriteString(stationToProbe)
	rmcMessage.SetParameters(probeStream.Bytes())

	for _, target := range targetList {
		targetURL := nex.NewStationURL(target)
		rvcID, _ := strconv.Atoi(targetURL.RVCID())
		targetClient := nexServer.FindClientFromConnectionID(uint32(rvcID))
		if targetClient != nil {
			msgPkt, _ := nex.NewPacketV1(targetClient, nil)
			msgPkt.SetVersion(1)
			msgPkt.SetSource(0xA1)
			msgPkt.SetDestination(0xAF)
			msgPkt.SetType(nex.DataPacket)
			msgPkt.SetPayload(rmcMessage.Bytes())
			msgPkt.AddFlag(nex.FlagNeedsAck)
			msgPkt.AddFlag(nex.FlagReliable)
			nexServer.Send(msgPkt)
		}
	}
}

func reportNATProperties(err error, client *nex.Client, callID uint32, natm uint32, natf uint32, rtt uint32) {
	if err != nil {
		fmt.Println("ReportNATProperties error:", err)
		return
	}

	fmt.Printf("ReportNATProperties: PID=%d natm=%d natf=%d\n", client.PID(), natm, natf)

	urls := dbGetPlayerURLs(client.PID())
	pid := strconv.FormatUint(uint64(client.PID()), 10)
	rvcid := strconv.FormatUint(uint64(client.ConnectionID()), 10)

	for _, urlStr := range urls {
		u := nex.NewStationURL(urlStr)
		if u.Type() == "3" {
			u.SetNatm(strconv.FormatUint(uint64(natm), 10))
			u.SetNatf(strconv.FormatUint(uint64(natf), 10))
		}
		u.SetPID(pid)
		u.SetRVCID(rvcid)
		dbUpdateSessionURL(client.PID(), urlStr, u.EncodeToString())
	}

	sendResponse(client, nat_traversal.ProtocolID, callID, nat_traversal.MethodReportNATProperties, nil)
}
