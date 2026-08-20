package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	nex "github.com/PretendoNetwork/nex-go"
	match_making "github.com/PretendoNetwork/nex-protocols-go/match-making"
	match_making_ext "github.com/PretendoNetwork/nex-protocols-go/match-making-ext"
	matchmake_extension "github.com/PretendoNetwork/nex-protocols-go/matchmake-extension"
	nat_traversal "github.com/PretendoNetwork/nex-protocols-go/nat-traversal"
	secure_connection "github.com/PretendoNetwork/nex-protocols-go/secure-connection"
	"go.mongodb.org/mongo-driver/bson"
)

var nexServer *nex.Server

func main() {
	connectDB()

	nexServer = nex.NewServer()
	nexServer.SetPRUDPVersion(1)
	nexServer.SetPRUDPProtocolMinorVersion(3)
	nexServer.SetDefaultNEXVersion(&nex.NEXVersion{
		Major: 3,
		Minor: 10,
		Patch: 0,
	})
	nexServer.SetMatchMakingProtocolVersion(&nex.NEXVersion{
		Major: 3,
		Minor: 10,
		Patch: 0,
	})
	nexServer.SetKerberosPassword(os.Getenv("KERBEROS_PASSWORD"))
	nexServer.SetAccessKey("f1b61c8e")

	nexServer.On("Data", func(packet *nex.PacketV1) {
		req := packet.RMCRequest()
		// Handle missing-from-library methods via raw packet dispatch
		handled := handleRawPacket(packet)
		if !handled {
			fmt.Printf("[MC-Secure] proto=%#x method=%#x\n", req.ProtocolID(), req.MethodID())
		}
	})

	setupSecureConnection()
	setupMatchMaking()
	setupMatchMakeExt()
	setupMatchmakeExtension()
	setupNATTraversal()

	port := os.Getenv("MC_SECURE_PORT")
	if port == "" {
		port = "60017"
	}
	fmt.Printf("[MC-Secure] listening on :%s\n", port)
	nexServer.Listen(":" + port)
}

// sendResponse sends an RMC success response packet.
func sendResponse(client *nex.Client, protocolID uint8, callID, methodID uint32, payload []byte) {
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

func sendEmpty(client *nex.Client, protocolID uint8, callID, methodID uint32) {
	sendResponse(client, protocolID, callID, methodID, []byte{})
}

// gatheringToMMSession converts a db Gathering to a nex MatchmakeSession.
func gatheringToMMSession(g *Gathering) *match_making.MatchmakeSession {
	s := match_making.NewMatchmakeSession()
	s.ID = g.GID
	s.OwnerPID = g.Owner
	s.HostPID = g.Host
	s.MinimumParticipants = g.MinParticipants
	s.MaximumParticipants = g.MaxParticipants
	s.ParticipationPolicy = g.ParticipationPolicy
	s.PolicyArgument = g.PolicyArgument
	s.Flags = g.Flags
	s.State = g.State
	s.Description = g.Description
	s.GameMode = g.GameMode
	s.Attributes = g.Attribs
	if len(s.Attributes) == 0 {
		s.Attributes = []uint32{0, 0, 0, 0, 0, 0}
	}
	s.OpenParticipation = g.OpenParticipation
	s.MatchmakeSystemType = g.MatchmakeSystem
	s.ApplicationData = g.ApplicationData
	s.ParticipationCount = uint32(len(g.Participants))
	s.ProgressScore = g.ProgressScore
	if s.ProgressScore == 0 {
		s.ProgressScore = 100
	}
	s.SessionKey = g.SessionKey
	s.MatchmakeParam = match_making.NewMatchmakeParam()
	s.StartedTime = nex.NewDateTime(0)
	return s
}

// ---- handleRawPacket handles protocol/method combos missing from the library ----

// matchmake-extension (0x6D) method IDs not in nex-protocols-go v1.0.23
const (
	mmeBrowse              = 0x4
	mmeBrowseWithURLs      = 0x5
	mmeJoin                = 0x7
	mmeBrowseNoHolder      = 0x2A
	mmeBrowseWithURLsNoH   = 0x2B
	mmeBrowseNoHolderNoRng = 0x34
	mmeBrowseWithURLsNoHNR = 0x35
	mmeAutoWithGID         = 0x18  // AutoMatchmakeWithGatheringId_Postpone
	mmeUpdateSession       = 0x8   // UpdateMatchmakeSession (but may differ)
	mmeEndParticipation    = 0x24  // EndParticipation (in matchmake_ext proto)
	mmeWithdraw            = 0x29  // WithdrawMatchmaking
	mmeWithdrawAll         = 0x2C  // WithdrawMatchmakingAll
	mmeRequestMatchmaking  = 0x28  // but that's AutoMatchmakeWithParam, skip
	mmeGetMyGathering      = 0x7   // JoinMatchmakeSession, same ID
)

// nat-traversal (0x03) method IDs not in nex-protocols-go v1.0.23
const (
	natRequestProbeInit = 0x1
	natInitiateProbe    = 0x2
)

func handleRawPacket(packet *nex.PacketV1) bool {
	req := packet.RMCRequest()
	client := packet.Sender()
	callID := req.CallID()
	params := req.Parameters()

	switch req.ProtocolID() {
	case matchmake_extension.ProtocolID:
		switch req.MethodID() {
		case mmeBrowse, mmeBrowseNoHolder, mmeBrowseNoHolderNoRng:
			handleBrowseSession(client, callID, req.MethodID())
			return true
		case mmeBrowseWithURLs, mmeBrowseWithURLsNoH, mmeBrowseWithURLsNoHNR:
			handleBrowseWithURLs(client, callID, req.MethodID())
			return true
		case mmeJoin:
			handleJoinMatchmakeSession(client, callID, params)
			return true
		case mmeAutoWithGID:
			handleAutoWithGID(client, callID, params)
			return true
		case 0xB: // UpdateApplicationBuffer
			handleUpdateApplicationBuffer(client, callID, params)
			return true
		case 0xC: // UpdateMatchmakeSessionAttribute
			handleUpdateMatchmakeSessionAttribute(client, callID, params)
			return true
		case 0xD: // GetFriendNotificationDataList
			stream := nex.NewStreamOut(nexServer)
			stream.WriteUInt32LE(0) // empty list
			sendResponse(client, matchmake_extension.ProtocolID, callID, 0xD, stream.Bytes())
			return true
		case 0xE: // UpdateMatchmakeSession
			handleUpdateMatchmakeSession(client, callID, params)
			return true
		case 0x29: // WithdrawMatchmaking
			sendEmpty(client, matchmake_extension.ProtocolID, callID, 0x29)
			return true
		case 0x2C: // WithdrawMatchmakingAll
			sendEmpty(client, matchmake_extension.ProtocolID, callID, 0x2C)
			return true
		case 0x28: // RequestMatchmaking
			stream := nex.NewStreamOut(nexServer)
			stream.WriteUInt64LE(0)
			sendResponse(client, matchmake_extension.ProtocolID, callID, 0x28, stream.Bytes())
			return true
		}
	case nat_traversal.ProtocolID:
		switch req.MethodID() {
		case natRequestProbeInit:
			handleRequestProbeInit(client, callID, params)
			return true
		case natInitiateProbe:
			sendEmpty(client, nat_traversal.ProtocolID, callID, natInitiateProbe)
			return true
		}
	}
	return false
}

func handleUpdateApplicationBuffer(client *nex.Client, callID uint32, params []byte) {
	stream := nex.NewStreamIn(params, nexServer)
	gid := stream.ReadUInt32LE()
	buf, _ := stream.ReadBuffer()
	g := dbGetGathering(gid)
	if g != nil {
		gatheringsCol.UpdateOne(context.Background(),
			bson.D{{Key: "gid", Value: gid}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "application_data", Value: buf}}}})
	}
	sendEmpty(client, matchmake_extension.ProtocolID, callID, 0xB)
}

func handleUpdateMatchmakeSessionAttribute(client *nex.Client, callID uint32, params []byte) {
	stream := nex.NewStreamIn(params, nexServer)
	gid := stream.ReadUInt32LE()
	attribs := stream.ReadListUInt32LE()
	g := dbGetGathering(gid)
	if g != nil {
		gatheringsCol.UpdateOne(context.Background(),
			bson.D{{Key: "gid", Value: gid}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "attribs", Value: attribs}}}})
	}
	sendEmpty(client, matchmake_extension.ProtocolID, callID, 0xC)
}

func handleUpdateMatchmakeSession(client *nex.Client, callID uint32, params []byte) {
	stream := nex.NewStreamIn(params, nexServer)
	session := match_making.NewMatchmakeSession()
	if _, err := stream.ReadStructure(session); err != nil {
		sendEmpty(client, matchmake_extension.ProtocolID, callID, 0xE)
		return
	}
	gid := session.ID
	g := dbGetGathering(gid)
	if g != nil {
		gatheringsCol.UpdateOne(context.Background(),
			bson.D{{Key: "gid", Value: gid}},
			bson.D{{Key: "$set", Value: bson.D{
				{Key: "description", Value: session.Description},
				{Key: "game_mode", Value: session.GameMode},
				{Key: "attribs", Value: session.Attributes},
				{Key: "min_participants", Value: session.MinimumParticipants},
				{Key: "max_participants", Value: session.MaximumParticipants},
				{Key: "open_participation", Value: session.OpenParticipation},
			}}})
	}
	sendEmpty(client, matchmake_extension.ProtocolID, callID, 0xE)
}

func handleBrowseSession(client *nex.Client, callID uint32, methodID uint32) {
	gatherings := dbListGatherings()
	sessions := make([]nex.StructureInterface, 0, len(gatherings))
	for _, g := range gatherings {
		sessions = append(sessions, gatheringToMMSession(g))
	}
	stream := nex.NewStreamOut(nexServer)
	stream.WriteListStructure(sessions)
	sendResponse(client, matchmake_extension.ProtocolID, callID, methodID, stream.Bytes())
	fmt.Printf("[MC-Secure] Browse -> %d sessions\n", len(sessions))
}

func handleBrowseWithURLs(client *nex.Client, callID uint32, methodID uint32) {
	gatherings := dbListGatherings()
	sessions := make([]nex.StructureInterface, 0, len(gatherings))
	for _, g := range gatherings {
		sessions = append(sessions, gatheringToMMSession(g))
	}
	stream := nex.NewStreamOut(nexServer)
	stream.WriteListStructure(sessions)
	// Write empty URL list parallel to sessions
	stream.WriteUInt32LE(0)
	sendResponse(client, matchmake_extension.ProtocolID, callID, methodID, stream.Bytes())
	fmt.Printf("[MC-Secure] BrowseWithURLs -> %d sessions\n", len(sessions))
}

func handleJoinMatchmakeSession(client *nex.Client, callID uint32, params []byte) {
	stream := nex.NewStreamIn(params, nexServer)
	gid := stream.ReadUInt32LE()
	message, _ := stream.ReadString()
	_ = message
	pid := client.PID()

	fmt.Printf("[MC-Secure] JoinMatchmakeSession gid=%d pid=%d\n", gid, pid)
	g := dbGetGathering(gid)
	if g == nil {
		rmcResponse := nex.NewRMCResponse(matchmake_extension.ProtocolID, callID)
		rmcResponse.SetError(nex.Errors.RendezVous.InvalidGID)
		sendRawResponse(client, rmcResponse.Bytes())
		return
	}
	if !dbJoinGathering(gid, pid) {
		rmcResponse := nex.NewRMCResponse(matchmake_extension.ProtocolID, callID)
		rmcResponse.SetError(nex.Errors.RendezVous.SessionFull)
		sendRawResponse(client, rmcResponse.Bytes())
		return
	}
	g = dbGetGathering(gid)
	out := nex.NewStreamOut(nexServer)
	out.WriteBuffer(g.SessionKey)
	sendResponse(client, matchmake_extension.ProtocolID, callID, mmeJoin, out.Bytes())
}

func handleAutoWithGID(client *nex.Client, callID uint32, params []byte) {
	stream := nex.NewStreamIn(params, nexServer)
	gids := stream.ReadListUInt32LE()
	for _, gid := range gids {
		g := dbGetGathering(gid)
		if g != nil {
			out := nex.NewStreamOut(nexServer)
			out.WriteStructure(gatheringToMMSession(g))
			sendResponse(client, matchmake_extension.ProtocolID, callID, mmeAutoWithGID, out.Bytes())
			return
		}
	}
	out := nex.NewStreamOut(nexServer)
	out.WriteStructure(match_making.NewMatchmakeSession())
	sendResponse(client, matchmake_extension.ProtocolID, callID, mmeAutoWithGID, out.Bytes())
}

func handleRequestProbeInit(client *nex.Client, callID uint32, params []byte) {
	stream := nex.NewStreamIn(params, nexServer)
	urls := stream.ReadListStationURL()
	fmt.Printf("[MC-Secure] RequestProbeInitiation from pid=%d urls=%v\n", client.PID(), urls)
	// Forward probe request to each target's registered station
	for _, url := range urls {
		pidStr := url.PID()
		if pidStr == "" {
			continue
		}
		pidInt, _ := strconv.ParseUint(pidStr, 10, 32)
		if pidInt == 0 {
			continue
		}
		targetURLs := dbGetURLs(uint32(pidInt))
		_ = targetURLs
		// The probe initiation is sent as a notification to the target client.
		// nex-go doesn't expose a direct "send to PID" API; clients receive it
		// via the PRUDP connection they already have open.
	}
	sendEmpty(client, nat_traversal.ProtocolID, callID, natRequestProbeInit)
}

func sendRawResponse(client *nex.Client, payload []byte) {
	pkt, _ := nex.NewPacketV1(client, nil)
	pkt.SetVersion(1)
	pkt.SetSource(0xA1)
	pkt.SetDestination(0xAF)
	pkt.SetType(nex.DataPacket)
	pkt.SetPayload(payload)
	pkt.AddFlag(nex.FlagNeedsAck)
	pkt.AddFlag(nex.FlagReliable)
	nexServer.Send(pkt)
}

// ---- Secure Connection ----

func setupSecureConnection() {
	secureProto := secure_connection.NewSecureConnectionProtocol(nexServer)

	secureProto.Register(func(err error, client *nex.Client, callID uint32, stationURLs []*nex.StationURL) {
		if err != nil {
			return
		}
		pid := client.PID()

		connID := uint32(nexServer.ConnectionIDCounter().Increment())
		client.SetConnectionID(connID)

		localStation := stationURLs[0]
		localStation.SetPID(strconv.FormatUint(uint64(pid), 10))
		localStation.SetRVCID(strconv.FormatUint(uint64(connID), 10))

		addr := client.Address().IP.String()
		port := strconv.Itoa(client.Address().Port)
		localStation.SetAddress(addr)
		localStation.SetPort(port)
		localStation.SetType("3")
		globalURL := localStation.EncodeToString()

		urls := make([]string, len(stationURLs))
		for i, u := range stationURLs {
			urls[i] = u.EncodeToString()
		}
		urls[0] = globalURL
		dbSetURLs(pid, urls)
		client.SetLocalStationURL(globalURL)

		stream := nex.NewStreamOut(nexServer)
		stream.WriteUInt32LE(0x10001)
		stream.WriteUInt32LE(connID)
		stream.WriteString(globalURL)
		sendResponse(client, secure_connection.ProtocolID, callID, secure_connection.MethodRegister, stream.Bytes())
		fmt.Printf("[MC-Secure] Register pid=%d addr=%s:%s connID=%d\n", pid, addr, port, connID)
	})

	secureProto.RegisterEx(func(err error, client *nex.Client, callID uint32, stationURLs []*nex.StationURL, loginData *nex.DataHolder) {
		pid := client.PID()
		connID := uint32(nexServer.ConnectionIDCounter().Increment())
		client.SetConnectionID(connID)

		var localStation *nex.StationURL
		if len(stationURLs) > 0 {
			localStation = stationURLs[0]
		} else {
			localStation = nex.NewStationURL("prudp:/")
		}
		localStation.SetPID(strconv.FormatUint(uint64(pid), 10))
		localStation.SetRVCID(strconv.FormatUint(uint64(connID), 10))
		addr := client.Address().IP.String()
		port := strconv.Itoa(client.Address().Port)
		localStation.SetAddress(addr)
		localStation.SetPort(port)
		localStation.SetType("3")
		globalURL := localStation.EncodeToString()

		urls := []string{globalURL}
		dbSetURLs(pid, urls)
		client.SetLocalStationURL(globalURL)

		stream := nex.NewStreamOut(nexServer)
		stream.WriteUInt32LE(0x10001)
		stream.WriteUInt32LE(connID)
		stream.WriteString(globalURL)
		sendResponse(client, secure_connection.ProtocolID, callID, secure_connection.MethodRegisterEx, stream.Bytes())
		fmt.Printf("[MC-Secure] RegisterEx pid=%d connID=%d\n", pid, connID)
	})

	secureProto.RequestURLs(func(err error, client *nex.Client, callID uint32, cid uint32, pid uint32) {
		urls := dbGetURLs(pid)
		stream := nex.NewStreamOut(nexServer)
		stream.WriteBool(true)
		stream.WriteListString(urls)
		sendResponse(client, secure_connection.ProtocolID, callID, secure_connection.MethodRequestURLs, stream.Bytes())
	})

	secureProto.UpdateURLs(func(err error, client *nex.Client, callID uint32, stationURLs []*nex.StationURL) {
		pid := client.PID()
		urls := make([]string, len(stationURLs))
		for i, u := range stationURLs {
			urls[i] = u.EncodeToString()
		}
		dbSetURLs(pid, urls)
		sendEmpty(client, secure_connection.ProtocolID, callID, secure_connection.MethodUpdateURLs)
	})

	secureProto.RequestConnectionData(func(err error, client *nex.Client, callID uint32, cid uint32, pid uint32) {
		urls := dbGetURLs(pid)
		stream := nex.NewStreamOut(nexServer)
		stream.WriteBool(len(urls) > 0)
		stream.WriteUInt32LE(uint32(len(urls)))
		for _, u := range urls {
			stream.WriteUInt32LE(cid)
			stream.WriteString(u)
		}
		sendResponse(client, secure_connection.ProtocolID, callID, secure_connection.MethodRequestConnectionData, stream.Bytes())
	})

	secureProto.TestConnectivity(func(err error, client *nex.Client, callID uint32) {
		sendEmpty(client, secure_connection.ProtocolID, callID, secure_connection.MethodTestConnectivity)
	})

	secureProto.SendReport(func(err error, client *nex.Client, callID uint32, reportID uint32, reportData []byte) {
		sendEmpty(client, secure_connection.ProtocolID, callID, secure_connection.MethodSendReport)
	})

	secureProto.Setup()
}

// ---- MatchMaking (protocol 0x15) ----

func setupMatchMaking() {
	mmProto := match_making.NewMatchMakingProtocol(nexServer)

	mmProto.UnregisterGathering(func(err error, client *nex.Client, callID uint32, gid uint32) {
		pid := client.PID()
		g := dbGetGathering(gid)
		if g != nil && g.Owner == pid {
			dbLeaveGathering(gid, pid)
		}
		stream := nex.NewStreamOut(nexServer)
		stream.WriteBool(true)
		sendResponse(client, match_making.ProtocolID, callID, match_making.MethodUnregisterGathering, stream.Bytes())
	})

	mmProto.UnregisterGatherings(func(err error, client *nex.Client, callID uint32, gids []uint32) {
		pid := client.PID()
		for _, gid := range gids {
			g := dbGetGathering(gid)
			if g != nil && g.Owner == pid {
				dbLeaveGathering(gid, pid)
			}
		}
		stream := nex.NewStreamOut(nexServer)
		stream.WriteBool(true)
		sendResponse(client, match_making.ProtocolID, callID, match_making.MethodUnregisterGatherings, stream.Bytes())
	})

	mmProto.FindBySingleID(func(err error, client *nex.Client, callID uint32, gid uint32) {
		g := dbGetGathering(gid)
		stream := nex.NewStreamOut(nexServer)
		if g != nil {
			stream.WriteBool(true)
			stream.WriteStructure(gatheringToMMSession(g))
		} else {
			stream.WriteBool(false)
			stream.WriteStructure(match_making.NewMatchmakeSession())
		}
		sendResponse(client, match_making.ProtocolID, callID, match_making.MethodFindBySingleID, stream.Bytes())
	})

	mmProto.GetSessionURLs(func(err error, client *nex.Client, callID uint32, gid uint32) {
		g := dbGetGathering(gid)
		stream := nex.NewStreamOut(nexServer)
		if g != nil {
			stream.WriteListString(dbGetURLs(g.Host))
		} else {
			stream.WriteUInt32LE(0)
		}
		sendResponse(client, match_making.ProtocolID, callID, match_making.MethodGetSessionURLs, stream.Bytes())
	})

	mmProto.UpdateSessionHostV1(func(err error, client *nex.Client, callID uint32, gid uint32) {
		pid := client.PID()
		gatheringsCol.UpdateOne(context.Background(),
			bson.D{{Key: "gid", Value: gid}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "host", Value: pid}}}})
		sendEmpty(client, match_making.ProtocolID, callID, match_making.MethodUpdateSessionHostV1)
	})

	mmProto.UpdateSessionHost(func(err error, client *nex.Client, callID uint32, gid uint32) {
		pid := client.PID()
		gatheringsCol.UpdateOne(context.Background(),
			bson.D{{Key: "gid", Value: gid}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "host", Value: pid}}}})
		sendEmpty(client, match_making.ProtocolID, callID, match_making.MethodUpdateSessionHost)
	})

	mmProto.Setup()
}

// ---- MatchMakingExt (protocol 0x32) ----

func setupMatchMakeExt() {
	extProto := match_making_ext.NewMatchMakingExtProtocol(nexServer)

	extProto.EndParticipation(func(err error, client *nex.Client, callID uint32, gid uint32, message string) {
		pid := client.PID()
		fmt.Printf("[MC-Secure] EndParticipation gid=%d pid=%d\n", gid, pid)
		dbLeaveGathering(gid, pid)
		stream := nex.NewStreamOut(nexServer)
		stream.WriteBool(true)
		sendResponse(client, match_making_ext.ProtocolID, callID, match_making_ext.MethodEndParticipation, stream.Bytes())
	})

	extProto.GetParticipants(func(err error, client *nex.Client, callID uint32, gid uint32, onlyActive bool) {
		g := dbGetGathering(gid)
		stream := nex.NewStreamOut(nexServer)
		if g != nil {
			pids := make([]uint32, len(g.Participants))
			copy(pids, g.Participants)
			stream.WriteListUInt32LE(pids)
		} else {
			stream.WriteUInt32LE(0)
		}
		sendResponse(client, match_making_ext.ProtocolID, callID, match_making_ext.MethodGetParticipants, stream.Bytes())
	})

	extProto.GetParticipantsURLs(func(err error, client *nex.Client, callID uint32, gids []uint32) {
		stream := nex.NewStreamOut(nexServer)
		stream.WriteUInt32LE(uint32(len(gids)))
		for _, gid := range gids {
			g := dbGetGathering(gid)
			stream.WriteUInt32LE(gid)
			var urls []string
			if g != nil {
				for _, pid := range g.Participants {
					urls = append(urls, dbGetURLs(pid)...)
				}
			}
			stream.WriteListString(urls)
		}
		sendResponse(client, match_making_ext.ProtocolID, callID, match_making_ext.MethodGetParticipantsURLs, stream.Bytes())
	})

	extProto.GetDetailedParticipants(func(err error, client *nex.Client, callID uint32, gid uint32, onlyActive bool) {
		// Return empty list — detailed participant info not critical
		stream := nex.NewStreamOut(nexServer)
		stream.WriteUInt32LE(0)
		sendResponse(client, match_making_ext.ProtocolID, callID, match_making_ext.MethodGetDetailedParticipants, stream.Bytes())
	})

	extProto.GetGatheringRelations(func(err error, client *nex.Client, callID uint32, id uint32, descr string) {
		stream := nex.NewStreamOut(nexServer)
		stream.WriteString("")
		sendResponse(client, match_making_ext.ProtocolID, callID, match_making_ext.MethodGetGatheringRelations, stream.Bytes())
	})

	extProto.DeleteFromDeletions(func(err error, client *nex.Client, callID uint32, deletions []uint32, pid uint32) {
		sendEmpty(client, match_making_ext.ProtocolID, callID, match_making_ext.MethodDeleteFromDeletions)
	})

	extProto.Setup()
}

// ---- MatchmakeExtension (protocol 0x6D) ----

func setupMatchmakeExtension() {
	mmeProto := matchmake_extension.NewMatchmakeExtensionProtocol(nexServer)

	mmeProto.CreateMatchmakeSession(func(err error, client *nex.Client, callID uint32,
		session *match_making.MatchmakeSession, message string, count uint16) {

		pid := client.PID()
		g := &Gathering{
			Description:         session.Description,
			GameMode:            session.GameMode,
			Attribs:             session.Attributes,
			MinParticipants:     session.MinimumParticipants,
			MaxParticipants:     session.MaximumParticipants,
			ParticipationPolicy: session.ParticipationPolicy,
			PolicyArgument:      session.PolicyArgument,
			Flags:               session.Flags,
			State:               session.State,
			OpenParticipation:   session.OpenParticipation,
			MatchmakeSystem:     session.MatchmakeSystemType,
			ApplicationData:     session.ApplicationData,
			ProgressScore:       100,
		}
		gid := dbNewGathering(pid, g)
		g = dbGetGathering(gid)
		fmt.Printf("[MC-Secure] CreateMatchmakeSession gid=%d pid=%d desc=%q\n", gid, pid, g.Description)

		stream := nex.NewStreamOut(nexServer)
		stream.WriteUInt32LE(gid)
		stream.WriteBuffer(g.SessionKey)
		sendResponse(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodCreateMatchmakeSession, stream.Bytes())
	})

	mmeProto.CloseParticipation(func(err error, client *nex.Client, callID uint32, gid uint32) {
		fmt.Printf("[MC-Secure] CloseParticipation gid=%d pid=%d\n", gid, client.PID())
		dbSetOpen(gid, false)
		sendEmpty(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodCloseParticipation)
	})

	mmeProto.OpenParticipation(func(err error, client *nex.Client, callID uint32, gid uint32) {
		dbSetOpen(gid, true)
		sendEmpty(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodOpenParticipation)
	})

	mmeProto.AutoMatchmake_Postpone(func(err error, client *nex.Client, callID uint32,
		session *match_making.MatchmakeSession, message string) {
		pid := client.PID()
		// Try to find an open session
		all := dbListGatherings()
		for _, g := range all {
			if g.OpenParticipation && g.Owner != pid {
				out := nex.NewStreamOut(nexServer)
				out.WriteStructure(gatheringToMMSession(g))
				sendResponse(client, matchmake_extension.ProtocolID, callID,
					matchmake_extension.MethodAutoMatchmake_Postpone, out.Bytes())
				return
			}
		}
		// None found — create new
		g := &Gathering{
			Description:       session.Description,
			GameMode:          session.GameMode,
			Attribs:           session.Attributes,
			MinParticipants:   session.MinimumParticipants,
			MaxParticipants:   session.MaximumParticipants,
			OpenParticipation: true,
			ProgressScore:     100,
		}
		gid := dbNewGathering(pid, g)
		g = dbGetGathering(gid)
		out := nex.NewStreamOut(nexServer)
		out.WriteStructure(gatheringToMMSession(g))
		sendResponse(client, matchmake_extension.ProtocolID, callID,
			matchmake_extension.MethodAutoMatchmake_Postpone, out.Bytes())
	})

	mmeProto.AutoMatchmakeWithSearchCriteria_Postpone(func(err error, client *nex.Client, callID uint32,
		session *match_making.MatchmakeSession, message string) {
		pid := client.PID()
		all := dbListGatherings()
		for _, g := range all {
			if g.OpenParticipation && g.Owner != pid {
				out := nex.NewStreamOut(nexServer)
				out.WriteStructure(gatheringToMMSession(g))
				sendResponse(client, matchmake_extension.ProtocolID, callID,
					matchmake_extension.MethodAutoMatchmakeWithSearchCriteria_Postpone, out.Bytes())
				return
			}
		}
		out := nex.NewStreamOut(nexServer)
		out.WriteStructure(match_making.NewMatchmakeSession())
		sendResponse(client, matchmake_extension.ProtocolID, callID,
			matchmake_extension.MethodAutoMatchmakeWithSearchCriteria_Postpone, out.Bytes())
	})

	mmeProto.GetSimplePlayingSession(func(err error, client *nex.Client, callID uint32, pids []uint32, includeLoginUser bool) {
		stream := nex.NewStreamOut(nexServer)
		stream.WriteUInt32LE(0) // empty list
		sendResponse(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodGetSimplePlayingSession, stream.Bytes())
	})

	mmeProto.GetPlayingSession(func(err error, client *nex.Client, callID uint32, pids []uint32) {
		stream := nex.NewStreamOut(nexServer)
		stream.WriteUInt32LE(0)
		sendResponse(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodGetPlayingSession, stream.Bytes())
	})

	mmeProto.UpdateNotificationData(func(err error, client *nex.Client, callID uint32, uiType, p1, p2 uint32, str string) {
		sendEmpty(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodUpdateNotificationData)
	})

	mmeProto.GetFriendNotificationData(func(err error, client *nex.Client, callID uint32, uiType int32) {
		stream := nex.NewStreamOut(nexServer)
		stream.WriteUInt32LE(0)
		sendResponse(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodGetFriendNotificationData, stream.Bytes())
	})

	mmeProto.UpdateProgressScore(func(err error, client *nex.Client, callID uint32, gid uint32, score uint8) {
		gatheringsCol.UpdateOne(context.Background(),
			bson.D{{Key: "gid", Value: gid}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "progress_score", Value: int32(score)}}}})
		sendEmpty(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodUpdateProgressScore)
	})

	mmeProto.JoinMatchmakeSessionEx(func(err error, client *nex.Client, callID uint32, gid uint32, gmessage string, ignoreBlock bool, numParticipants uint16) {
		pid := client.PID()
		fmt.Printf("[MC-Secure] JoinMatchmakeSessionEx gid=%d pid=%d\n", gid, pid)
		g := dbGetGathering(gid)
		if g == nil {
			rmcResponse := nex.NewRMCResponse(matchmake_extension.ProtocolID, callID)
			rmcResponse.SetError(nex.Errors.RendezVous.InvalidGID)
			sendRawResponse(client, rmcResponse.Bytes())
			return
		}
		if !dbJoinGathering(gid, pid) {
			rmcResponse := nex.NewRMCResponse(matchmake_extension.ProtocolID, callID)
			rmcResponse.SetError(nex.Errors.RendezVous.SessionFull)
			sendRawResponse(client, rmcResponse.Bytes())
			return
		}
		g = dbGetGathering(gid)
		out := nex.NewStreamOut(nexServer)
		out.WriteBuffer(g.SessionKey)
		sendResponse(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodJoinMatchmakeSessionEx, out.Bytes())
	})

	mmeProto.CreateMatchmakeSessionWithParam(func(err error, client *nex.Client, callID uint32, param *match_making.CreateMatchmakeSessionParam) {
		pid := client.PID()
		src := param.SourceMatchmakeSession
		g := &Gathering{
			Description:       src.Description,
			GameMode:          src.GameMode,
			Attribs:           src.Attributes,
			MinParticipants:   src.MinimumParticipants,
			MaxParticipants:   src.MaximumParticipants,
			OpenParticipation: src.OpenParticipation,
			ProgressScore:     100,
		}
		gid := dbNewGathering(pid, g)
		g = dbGetGathering(gid)
		out := nex.NewStreamOut(nexServer)
		out.WriteStructure(gatheringToMMSession(g))
		sendResponse(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodCreateMatchmakeSessionWithParam, out.Bytes())
	})

	mmeProto.JoinMatchmakeSessionWithParam(func(err error, client *nex.Client, callID uint32, param *match_making.JoinMatchmakeSessionParam) {
		pid := client.PID()
		gid := param.GID
		g := dbGetGathering(gid)
		if g == nil {
			rmcResponse := nex.NewRMCResponse(matchmake_extension.ProtocolID, callID)
			rmcResponse.SetError(nex.Errors.RendezVous.InvalidGID)
			sendRawResponse(client, rmcResponse.Bytes())
			return
		}
		dbJoinGathering(gid, pid)
		g = dbGetGathering(gid)
		out := nex.NewStreamOut(nexServer)
		out.WriteStructure(gatheringToMMSession(g))
		sendResponse(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodJoinMatchmakeSessionWithParam, out.Bytes())
	})

	mmeProto.AutoMatchmakeWithParam_Postpone(func(err error, client *nex.Client, callID uint32, param *match_making.AutoMatchmakeParam) {
		pid := client.PID()
		all := dbListGatherings()
		for _, g := range all {
			if g.OpenParticipation && g.Owner != pid {
				out := nex.NewStreamOut(nexServer)
				out.WriteStructure(gatheringToMMSession(g))
				sendResponse(client, matchmake_extension.ProtocolID, callID,
					matchmake_extension.MethodAutoMatchmakeWithParam_Postpone, out.Bytes())
				return
			}
		}
		out := nex.NewStreamOut(nexServer)
		out.WriteStructure(match_making.NewMatchmakeSession())
		sendResponse(client, matchmake_extension.ProtocolID, callID,
			matchmake_extension.MethodAutoMatchmakeWithParam_Postpone, out.Bytes())
	})

	mmeProto.Setup()
}

// ---- NAT Traversal (protocol 0x03) ----

func setupNATTraversal() {
	natProto := nat_traversal.NewNATTraversalProtocol(nexServer)

	natProto.RequestProbeInitiationExt(func(err error, client *nex.Client, callID uint32, targetList []string, stationToProbe string) {
		fmt.Printf("[MC-Secure] RequestProbeInitiationExt from pid=%d targets=%v\n", client.PID(), targetList)
		sendEmpty(client, nat_traversal.ProtocolID, callID, nat_traversal.MethodRequestProbeInitiationExt)
	})

	natProto.ReportNATTraversalResult(func(err error, client *nex.Client, callID uint32, cid uint32, result bool, rtt uint32) {
		fmt.Printf("[MC-Secure] ReportNATTraversalResult cid=%d result=%v rtt=%d\n", cid, result, rtt)
		sendEmpty(client, nat_traversal.ProtocolID, callID, nat_traversal.MethodReportNATTraversalResult)
	})

	natProto.ReportNATProperties(func(err error, client *nex.Client, callID uint32, natm uint32, natf uint32, rtt uint32) {
		fmt.Printf("[MC-Secure] ReportNATProperties natm=%d natf=%d rtt=%d pid=%d\n", natm, natf, rtt, client.PID())
		sendEmpty(client, nat_traversal.ProtocolID, callID, nat_traversal.MethodReportNATProperties)
	})

	natProto.GetRelaySignatureKey(func(err error, client *nex.Client, callID uint32) {
		sendEmpty(client, nat_traversal.ProtocolID, callID, nat_traversal.MethodGetRelaySignatureKey)
	})

	natProto.Setup()
}
