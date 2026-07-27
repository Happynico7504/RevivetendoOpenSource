package nex_friends_wiiu

import (
	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/nex-go/v2/types"
	friends_wiiu "github.com/PretendoNetwork/nex-protocols-go/v2/friends-wiiu"
	friends_wiiu_constants "github.com/PretendoNetwork/nex-protocols-go/v2/friends-wiiu/constants"
	friends_wiiu_types "github.com/PretendoNetwork/nex-protocols-go/v2/friends-wiiu/types"
	"github.com/PretendoNetwork/friends-nex/database"
	"github.com/PretendoNetwork/friends-nex/globals"
)

func UpdateAndGetAllInformation(
	err error,
	packet nex.PacketInterface,
	callID uint32,
	nnaInfo friends_wiiu_types.NNAInfo,
	presence friends_wiiu_types.NintendoPresenceV2,
	birthday types.DateTime,
) (*nex.RMCMessage, *nex.Error) {
	pid := uint64(packet.Sender().PID())
	globals.Logger.Infof("[FRIENDS] UpdateAndGetAllInformation PID=%d", pid)

	// Forward the Wii U's NNAInfo and presence to Pretendo so other users see us correctly.
	// Always treat the user as online since they're connected to our server.
	database.SaveLocalNNAAndPresence(pid,
		string(nnaInfo.PrincipalBasicInfo.NNID),
		string(nnaInfo.PrincipalBasicInfo.Mii.Name),
		[]byte(nnaInfo.PrincipalBasicInfo.Mii.MiiData),
		true,
		uint64(presence.GameKey.TitleID),
		uint16(presence.GameKey.TitleVersion),
		uint32(presence.GameServerID),
	)
	go database.StartPretendoPresence(pid)

	settings := database.GetUserSettings(pid)
	friendRows := database.GetFriends(pid)

	friendsList := make(types.List[friends_wiiu_types.FriendInfo], 0, len(friendRows))
	for _, row := range friendRows {
		fi := friends_wiiu_types.NewFriendInfo()

		fi.NNAInfo.PrincipalBasicInfo.PID = types.NewPID(uint64(row.FriendPID))
		fi.NNAInfo.PrincipalBasicInfo.NNID = types.NewString(row.FriendNNID)
		fi.NNAInfo.PrincipalBasicInfo.Mii.Name = types.NewString(row.MiiName)
		fi.NNAInfo.PrincipalBasicInfo.Mii.MiiData = types.NewBuffer(row.MiiData)

		_, onServer := globals.ConnectedClients.Load(uint64(row.FriendPID))
		fi.Presence.Online = types.NewBool(onServer || row.IsOnline)
		fi.Presence.ChangedFlags = friends_wiiu_constants.PresenceChangedFlag(row.PresenceFlags)
		fi.Presence.GameKey.TitleID = types.NewUInt64(row.TitleID)
		fi.Presence.GameKey.TitleVersion = types.NewUInt16(row.TitleVersion)
		fi.Presence.GameServerID = types.NewUInt32(row.GameServerID)
		fi.Presence.PID = types.NewPID(row.PresencePID)
		fi.Presence.GatheringID = types.NewUInt32(row.GatheringID)
		fi.Presence.Unknown5 = types.NewUInt8(row.PresenceUnk5)
		fi.Presence.Unknown6 = types.NewUInt8(row.PresenceUnk6)
		fi.Presence.Unknown7 = types.NewUInt8(row.PresenceUnk7)

		fi.Status.Unknown = types.NewUInt8(row.CommentUnknown)
		fi.Status.Contents = types.NewString(row.CommentText)
		if !row.CommentChangedAt.IsZero() {
			fi.Status.LastChanged.FromTimestamp(row.CommentChangedAt)
		} else {
			fi.Status.LastChanged = fi.Status.LastChanged.Now()
		}

		if !row.BecameFriend.IsZero() {
			fi.BecameFriend.FromTimestamp(row.BecameFriend)
		}
		if !row.LastOnline.IsZero() {
			fi.LastOnline.FromTimestamp(row.LastOnline)
		}

		friendsList = append(friendsList, fi)
	}

	globals.Logger.Infof("[FRIENDS] returning %d friends for PID=%d", len(friendsList), pid)

	rmcResponseStream := nex.NewByteStreamOut(globals.SecureServer.LibraryVersions, globals.SecureServer.ByteStreamSettings)

	// PrincipalPreference
	pref := friends_wiiu_types.NewPrincipalPreference()
	pref.ShowOnlinePresence = types.NewBool(settings.ShowOnlinePresence)
	pref.ShowCurrentTitle = types.NewBool(settings.ShowCurrentTitle)
	pref.BlockFriendRequests = types.NewBool(settings.BlockFriendRequests)
	pref.WriteTo(rmcResponseStream)
	// Comment
	comment := friends_wiiu_types.NewComment()
	comment.Unknown = types.NewUInt8(settings.CommentUnknown)
	comment.Contents = types.NewString(settings.CommentText)
	comment.LastChanged.FromTimestamp(settings.CommentChangedAt)
	comment.WriteTo(rmcResponseStream)
	// Friends list
	friendsList.WriteTo(rmcResponseStream)
	// Sent friend requests (empty — we don't track outgoing)
	rmcResponseStream.WriteUInt32LE(0)
	// Received (incoming) friend requests from Pretendo
	incomingRows := database.GetIncomingFriendRequests(pid)
	incomingList := make(types.List[friends_wiiu_types.FriendRequest], 0, len(incomingRows))
	for _, row := range incomingRows {
		fr := friends_wiiu_types.NewFriendRequest()
		fr.PrincipalInfo.PID = types.NewPID(row.RequesterPID)
		fr.PrincipalInfo.NNID = types.NewString(row.NNID)
		fr.PrincipalInfo.Mii.Name = types.NewString(row.MiiName)
		fr.PrincipalInfo.Mii.MiiData = types.NewBuffer(row.MiiData)
		fr.PrincipalInfo.Unknown = types.NewUInt8(2)
		fr.Message.FriendRequestID = types.NewUInt64(row.ID)
		fr.Message.Received = types.NewBool(true)
		fr.Message.Unknown2 = types.NewUInt8(row.Unk1)
		fr.Message.Message = types.NewString(row.Message)
		fr.Message.Unknown3 = types.NewUInt8(row.Unk2)
		fr.Message.Unknown4 = types.NewString(row.StrField)
		fr.Message.GameKey.TitleID = types.NewUInt64(row.TitleID)
		fr.Message.GameKey.TitleVersion = types.NewUInt16(row.TitleVersion)
		fr.Message.Unknown5.FromTimestamp(row.SentOn)
		fr.Message.ExpiresOn.FromTimestamp(row.ExpiresOn)
		fr.SentOn.FromTimestamp(row.SentOn)
		incomingList = append(incomingList, fr)
	}
	incomingList.WriteTo(rmcResponseStream)
	// Block list (empty)
	rmcResponseStream.WriteUInt32LE(0)
	// Unknown bool
	rmcResponseStream.WriteBool(false)
	// Persistent notifications (empty)
	rmcResponseStream.WriteUInt32LE(0)
	// Unknown bool
	rmcResponseStream.WriteBool(false)

	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, rmcResponseStream.Bytes())
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodUpdateAndGetAllInformation

	return rmcResponse, nil
}
