package nex

import (
	"fmt"
	"net"

	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/nex-go/v2/constants"
	"github.com/PretendoNetwork/nex-go/v2/types"
	common_secure "github.com/PretendoNetwork/nex-protocols-common-go/v2/secure-connection"
	friends_wiiu "github.com/PretendoNetwork/nex-protocols-go/v2/friends-wiiu"
	secure "github.com/PretendoNetwork/nex-protocols-go/v2/secure-connection"
	"github.com/PretendoNetwork/friends-nex/database"
	"github.com/PretendoNetwork/friends-nex/globals"
	nex_friends_wiiu "github.com/PretendoNetwork/friends-nex/nex/friends-wiiu"
)

func registerEx(err error, packet nex.PacketInterface, callID uint32, vecMyURLs types.List[types.StationURL], hCustomData types.DataHolder) (*nex.RMCMessage, *nex.Error) {
	if err != nil {
		fmt.Printf("[FRIENDS-SECURE] RegisterEx error: %v\n", err)
		return nil, nex.NewError(nex.ResultCodes.Core.InvalidArgument, err.Error())
	}

	connection := packet.Sender().(*nex.PRUDPConnection)
	endpoint := connection.Endpoint()
	pid := uint32(connection.PID())

	// Determine platform from the DataHolder type — skip token validation since PID is
	// already established from the Kerberos CONNECT.
	loginDataType := hCustomData.Object.DataObjectID().(types.String)
	fmt.Printf("[FRIENDS-SECURE] RegisterEx: PID=%d platform=%s\n", pid, loginDataType)

	var localStation *types.StationURL
	var publicStation *types.StationURL

	for _, stationURL := range vecMyURLs {
		natf, ok := stationURL.NATFiltering()
		if !ok {
			if localStation == nil {
				localStation = stationURL.CopyRef().(*types.StationURL)
			}
			continue
		}
		natm, ok := stationURL.NATMapping()
		if !ok {
			if localStation == nil {
				localStation = stationURL.CopyRef().(*types.StationURL)
			}
			continue
		}
		if localStation == nil && !stationURL.IsPublic() {
			localStation = stationURL.CopyRef().(*types.StationURL)
		}
		if localStation == nil && natf == constants.UnknownNATFiltering && natm == constants.UnknownNATMapping {
			localStation = stationURL.CopyRef().(*types.StationURL)
		}
		if publicStation == nil && stationURL.IsPublic() {
			publicStation = stationURL.CopyRef().(*types.StationURL)
		}
	}

	if localStation == nil && len(vecMyURLs) > 0 {
		cp := vecMyURLs[0]
		localStation = &cp
	}

	if localStation == nil {
		return nil, nex.NewError(nex.ResultCodes.Core.InvalidArgument, "no station found")
	}

	if publicStation == nil {
		publicStation = localStation.CopyRef().(*types.StationURL)
		var address string
		var port uint16
		switch clientAddress := connection.Address().(type) {
		case *net.UDPAddr:
			address = clientAddress.IP.String()
			port = uint16(clientAddress.Port)
		case *net.TCPAddr:
			address = clientAddress.IP.String()
			port = uint16(clientAddress.Port)
		}
		publicStation.SetAddress(address)
		publicStation.SetPortNumber(port)
		publicStation.SetNATFiltering(constants.UnknownNATFiltering)
		publicStation.SetNATMapping(constants.UnknownNATMapping)
		publicStation.SetType(uint8(constants.StationURLFlagPublic) | uint8(constants.StationURLFlagBehindNAT))
	}

	localStation.SetPrincipalID(connection.PID())
	publicStation.SetPrincipalID(connection.PID())
	localStation.SetRVConnectionID(connection.ID)
	publicStation.SetRVConnectionID(connection.ID)

	connection.StationURLs = append(connection.StationURLs, *localStation)
	connection.StationURLs = append(connection.StationURLs, *publicStation)

	globals.ConnectedClients.Store(uint64(pid), &globals.ConnectionInfo{
		Connection:  connection,
		SubstreamID: 0,
	})
	database.SetOnlineStatus(uint64(pid), true)

	fmt.Printf("[FRIENDS-SECURE] RegisterEx: success PID=%d CID=%d\n", pid, connection.ID)

	rmcStream := nex.NewByteStreamOut(endpoint.LibraryVersions(), endpoint.ByteStreamSettings())
	types.NewQResultSuccess(nex.ResultCodes.Core.Unknown).WriteTo(rmcStream)
	types.NewUInt32(connection.ID).WriteTo(rmcStream)
	types.NewString(publicStation.URL()).WriteTo(rmcStream)

	rmcResponse := nex.NewRMCSuccess(endpoint, rmcStream.Bytes())
	rmcResponse.ProtocolID = secure.ProtocolID
	rmcResponse.MethodID = secure.MethodRegisterEx
	rmcResponse.CallID = callID
	return rmcResponse, nil
}

func registerCommonSecureServerProtocols() {
	secureProtocol := secure.NewProtocol()
	commonSecure := common_secure.NewCommonProtocol(secureProtocol)
	commonSecure.EnableInsecureRegister()
	commonSecure.CreateReportDBRecord = func(pid types.PID, reportID types.UInt32, reportData types.QBuffer) error {
		return nil
	}
	secureProtocol.SetHandlerRegisterEx(registerEx)
	globals.SecureEndpoint.RegisterServiceProtocol(secureProtocol)

	friendsProtocol := friends_wiiu.NewProtocol()
	globals.SecureEndpoint.RegisterServiceProtocol(friendsProtocol)
	friendsProtocol.SetHandlerUpdateAndGetAllInformation(nex_friends_wiiu.UpdateAndGetAllInformation)
	friendsProtocol.SetHandlerAddFriend(nex_friends_wiiu.AddFriend)
	friendsProtocol.SetHandlerAddFriendRequest(nex_friends_wiiu.AddFriendRequest)
	friendsProtocol.SetHandlerAcceptFriendRequest(nex_friends_wiiu.AcceptFriendRequest)
	friendsProtocol.SetHandlerCancelFriendRequest(nex_friends_wiiu.CancelFriendRequest)
	friendsProtocol.SetHandlerRemoveFriend(nex_friends_wiiu.RemoveFriend)
	friendsProtocol.SetHandlerUpdatePresence(nex_friends_wiiu.UpdatePresence)
	friendsProtocol.SetHandlerUpdateMii(nex_friends_wiiu.UpdateMii)
	friendsProtocol.SetHandlerUpdateComment(nex_friends_wiiu.UpdateComment)
	friendsProtocol.SetHandlerUpdatePreference(nex_friends_wiiu.UpdatePreference)
	friendsProtocol.SetHandlerCheckSettingStatus(nex_friends_wiiu.CheckSettingStatus)
	friendsProtocol.SetHandlerGetRequestBlockSettings(nex_friends_wiiu.GetRequestBlockSettings)
	friendsProtocol.SetHandlerDeletePersistentNotification(nex_friends_wiiu.DeletePersistentNotification)
	friendsProtocol.SetHandlerGetBasicInfo(nex_friends_wiiu.GetBasicInfo)
}
