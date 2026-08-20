package main

import (
	"crypto/rand"
	"fmt"
	"os"

	nex "github.com/PretendoNetwork/nex-go"
	auth_proto "github.com/PretendoNetwork/nex-protocols-go/authentication"
)

const (
	accessKey       = "f1b61c8e"
	secureServerPID = uint32(2)
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
	nexServer.SetKerberosPassword(os.Getenv("KERBEROS_PASSWORD"))
	nexServer.SetAccessKey(accessKey)

	nexServer.On("Data", func(packet *nex.PacketV1) {
		req := packet.RMCRequest()
		fmt.Printf("[MC-Auth] proto=%#x method=%#x\n", req.ProtocolID(), req.MethodID())
	})

	authProto := auth_proto.NewAuthenticationProtocol(nexServer)
	authProto.Login(handleLogin)
	authProto.LoginEx(handleLoginEx)
	authProto.RequestTicket(handleRequestTicket)
	authProto.GetName(handleGetName)
	authProto.GetPID(handleGetPID)
	authProto.Setup()

	port := os.Getenv("MC_AUTH_PORT")
	if port == "" {
		port = "60016"
	}
	fmt.Printf("[MC-Auth] listening on :%s\n", port)
	nexServer.Listen(":" + port)
}

func sendAuthResponse(client *nex.Client, protocolID uint8, callID, methodID uint32, payload []byte) {
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

func secureStationURL() string {
	location := os.Getenv("SECURE_SERVER_LOCATION")
	port := os.Getenv("MC_SECURE_PORT")
	if port == "" {
		port = "60017"
	}
	url := nex.NewStationURL("")
	url.SetScheme("prudps")
	url.SetAddress(location)
	url.SetPort(port)
	url.SetCID("1")
	url.SetPID("2")
	url.SetSID("1")
	url.SetStream("10")
	url.SetType("2")
	return url.EncodeToString()
}

func generateTicket(userPID, targetPID uint32) []byte {
	userPass, ok := getPasswordByPID(userPID)
	if !ok {
		userPass = "guest"
	}
	targetPass := nexServer.KerberosPassword()
	if targetPID != secureServerPID {
		if p, ok2 := getPasswordByPID(targetPID); ok2 {
			targetPass = p
		}
	}

	userKey := nex.DeriveKerberosKey(userPID, []byte(userPass))
	targetKey := nex.DeriveKerberosKey(targetPID, []byte(targetPass))

	sessionKey := make([]byte, nexServer.KerberosKeySize())
	rand.Read(sessionKey)

	internalData := nex.NewKerberosTicketInternalData()
	internalData.SetTimestamp(nex.NewDateTime(0))
	internalData.SetUserPID(userPID)
	internalData.SetSessionKey(sessionKey)
	encInternal := internalData.Encrypt(targetKey, nex.NewStreamOut(nexServer))

	ticket := nex.NewKerberosTicket()
	ticket.SetSessionKey(sessionKey)
	ticket.SetTargetPID(targetPID)
	ticket.SetInternalData(encInternal)
	return ticket.Encrypt(userKey, nex.NewStreamOut(nexServer))
}

func handleLogin(err error, client *nex.Client, callID uint32, username string) {
	if err != nil {
		return
	}
	pid, _ := findOrCreateAccount(username)
	fmt.Printf("[MC-Auth] Login username=%q pid=%d\n", username, pid)

	// Dummy ticket in Login response; real ticket issued by RequestTicket
	dummyTicket := make([]byte, 32)

	connData := nex.NewRVConnectionData()
	connData.SetStationURL(secureStationURL())
	connData.SetSpecialProtocols([]byte{})
	connData.SetStationURLSpecialProtocols("")
	connData.SetTime(nex.NewDateTime(0).UTC())

	stream := nex.NewStreamOut(nexServer)
	stream.WriteResult(nex.NewResultSuccess(nex.Errors.Core.Unknown))
	stream.WriteUInt32LE(pid)
	stream.WriteBuffer(dummyTicket)
	stream.WriteStructure(connData)
	stream.WriteString("Minecraft WiiU")

	sendAuthResponse(client, auth_proto.ProtocolID, callID, auth_proto.MethodLogin, stream.Bytes())
}

func handleLoginEx(err error, client *nex.Client, callID uint32, username string, authInfo *auth_proto.AuthenticationInfo) {
	handleLogin(err, client, callID, username)
}

func handleRequestTicket(err error, client *nex.Client, callID uint32, userPID, serverPID uint32) {
	if err != nil {
		return
	}
	fmt.Printf("[MC-Auth] RequestTicket userPID=%d serverPID=%d\n", userPID, serverPID)

	ticket := generateTicket(userPID, serverPID)

	stream := nex.NewStreamOut(nexServer)
	stream.WriteResult(nex.NewResultSuccess(nex.Errors.Core.Unknown))
	stream.WriteBuffer(ticket)

	sendAuthResponse(client, auth_proto.ProtocolID, callID, auth_proto.MethodRequestTicket, stream.Bytes())
}

func handleGetName(err error, client *nex.Client, callID uint32, pid uint32) {
	if err != nil {
		return
	}
	name := getUsernameByPID(pid)
	if name == "" {
		name = fmt.Sprintf("Player%d", pid)
	}
	stream := nex.NewStreamOut(nexServer)
	stream.WriteString(name)
	sendAuthResponse(client, auth_proto.ProtocolID, callID, auth_proto.MethodGetName, stream.Bytes())
}

func handleGetPID(err error, client *nex.Client, callID uint32, username string) {
	if err != nil {
		return
	}
	pid := getPIDByUsername(username)
	if pid == 0 {
		pid, _ = findOrCreateAccount(username)
	}
	stream := nex.NewStreamOut(nexServer)
	stream.WriteUInt32LE(pid)
	sendAuthResponse(client, auth_proto.ProtocolID, callID, auth_proto.MethodGetPID, stream.Bytes())
}
