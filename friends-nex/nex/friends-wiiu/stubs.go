package nex_friends_wiiu

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/nex-go/v2/types"
	friends_wiiu "github.com/PretendoNetwork/nex-protocols-go/v2/friends-wiiu"
	friends_wiiu_types "github.com/PretendoNetwork/nex-protocols-go/v2/friends-wiiu/types"
	"github.com/PretendoNetwork/friends-nex/database"
	"github.com/PretendoNetwork/friends-nex/globals"
)


func UpdatePresence(
	err error, packet nex.PacketInterface, callID uint32,
	presence friends_wiiu_types.NintendoPresenceV2,
) (*nex.RMCMessage, *nex.Error) {
	pid := uint64(packet.Sender().PID())
	database.SaveLocalPresence(pid,
		bool(presence.Online),
		uint64(presence.GameKey.TitleID),
		uint16(presence.GameKey.TitleVersion),
		uint32(presence.GameServerID),
	)
	go func() {
		cmd := fmt.Sprintf(`{"cmd":"update_presence","is_online":%t,"title_id":%d,"title_version":%d,"game_server_id":%d}`,
			bool(presence.Online), uint64(presence.GameKey.TitleID),
			uint16(presence.GameKey.TitleVersion), uint32(presence.GameServerID))
		if !database.ForwardPresenceCommand(pid, cmd) {
			database.TriggerPretendoSync(pid)
		}
	}()
	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, nil)
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodUpdatePresence
	return rmcResponse, nil
}

func UpdateMii(
	err error, packet nex.PacketInterface, callID uint32,
	mii friends_wiiu_types.MiiV2,
) (*nex.RMCMessage, *nex.Error) {
	pid := uint64(packet.Sender().PID())
	miiData := []byte(mii.MiiData)
	database.SaveLocalMii(pid, string(mii.Name), miiData)
	go func() {
		cmd := fmt.Sprintf(`{"cmd":"update_mii","name":%q,"data_hex":%q}`,
			string(mii.Name), fmt.Sprintf("%x", miiData))
		if !database.ForwardPresenceCommand(pid, cmd) {
			database.TriggerPretendoSync(pid)
		}
	}()
	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, nil)
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodUpdateMii
	return rmcResponse, nil
}

func AddFriend(
	err error, packet nex.PacketInterface, callID uint32,
	friendPID types.PID,
) (*nex.RMCMessage, *nex.Error) {
	if err != nil {
		return nil, nex.NewError(nex.ResultCodes.Core.InvalidArgument, err.Error())
	}
	pid := uint64(packet.Sender().PID())
	targetPID := uint64(friendPID)
	go func() {
		cmd := fmt.Sprintf(`{"cmd":"add_friend","target_pid":%d}`, targetPID)
		if !database.ForwardPresenceCommand(pid, cmd) {
			database.QueuePretendoCommand(pid, "add_friend", fmt.Sprintf(`{"target_pid":%d}`, targetPID))
			database.TriggerPretendoSync(pid)
		}
	}()

	// Return a stub FriendInfo; the next UpdateAndGetAllInformation will have the real data after sync.
	fi := friends_wiiu_types.NewFriendInfo()
	fi.NNAInfo.PrincipalBasicInfo.PID = types.NewPID(targetPID)
	fi.NNAInfo.PrincipalBasicInfo.NNID = types.NewString("")
	fi.NNAInfo.PrincipalBasicInfo.Mii.Name = types.NewString("")
	fi.NNAInfo.PrincipalBasicInfo.Mii.MiiData = types.NewBuffer(nil)
	fi.Presence.Online = types.NewBool(false)
	fi.BecameFriend = fi.BecameFriend.Now()
	fi.LastOnline = fi.LastOnline.Now()

	rmcResponseStream := nex.NewByteStreamOut(globals.SecureServer.LibraryVersions, globals.SecureServer.ByteStreamSettings)
	fi.WriteTo(rmcResponseStream)
	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, rmcResponseStream.Bytes())
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodAddFriend
	return rmcResponse, nil
}

func RemoveFriend(
	err error, packet nex.PacketInterface, callID uint32,
	friendPID types.PID,
) (*nex.RMCMessage, *nex.Error) {
	if err != nil {
		return nil, nex.NewError(nex.ResultCodes.Core.InvalidArgument, err.Error())
	}
	pid := uint64(packet.Sender().PID())
	targetPID := uint64(friendPID)
	database.DeleteLocalFriend(pid, targetPID)
	go func() {
		cmd := fmt.Sprintf(`{"cmd":"remove_friend","target_pid":%d}`, targetPID)
		if !database.ForwardPresenceCommand(pid, cmd) {
			database.QueuePretendoCommand(pid, "remove_friend", fmt.Sprintf(`{"target_pid":%d}`, targetPID))
			database.TriggerPretendoSync(pid)
		}
	}()
	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, nil)
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodRemoveFriend
	return rmcResponse, nil
}

func UpdateComment(
	err error, packet nex.PacketInterface, callID uint32,
	comment friends_wiiu_types.Comment,
) (*nex.RMCMessage, *nex.Error) {
	pid := uint64(packet.Sender().PID())
	changedAt := comment.LastChanged.Standard()
	if changedAt.Year() < 2000 {
		changedAt = time.Now()
	}
	database.SaveUserComment(pid, uint8(comment.Unknown), string(comment.Contents), changedAt)
	go func() {
		cmd := fmt.Sprintf(`{"cmd":"update_comment","unk":%d,"text":%q}`,
			uint8(comment.Unknown), string(comment.Contents))
		if !database.ForwardPresenceCommand(pid, cmd) {
			database.TriggerPretendoSync(pid)
		}
	}()
	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, nil)
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodUpdateComment
	return rmcResponse, nil
}

func UpdatePreference(
	err error, packet nex.PacketInterface, callID uint32,
	preference friends_wiiu_types.PrincipalPreference,
) (*nex.RMCMessage, *nex.Error) {
	pid := uint64(packet.Sender().PID())
	database.SaveUserPreference(pid, bool(preference.ShowOnlinePresence), bool(preference.ShowCurrentTitle), bool(preference.BlockFriendRequests))
	go func() {
		cmd := fmt.Sprintf(`{"cmd":"update_preference","show_online":%t,"show_title":%t,"block_requests":%t}`,
			bool(preference.ShowOnlinePresence), bool(preference.ShowCurrentTitle), bool(preference.BlockFriendRequests))
		if !database.ForwardPresenceCommand(pid, cmd) {
			database.TriggerPretendoSync(pid)
		}
	}()
	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, nil)
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodUpdatePreference
	return rmcResponse, nil
}

func CheckSettingStatus(
	err error, packet nex.PacketInterface, callID uint32,
) (*nex.RMCMessage, *nex.Error) {
	rmcResponseStream := nex.NewByteStreamOut(globals.SecureServer.LibraryVersions, globals.SecureServer.ByteStreamSettings)
	rmcResponseStream.WriteUInt8(0)

	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, rmcResponseStream.Bytes())
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodCheckSettingStatus
	return rmcResponse, nil
}

func GetRequestBlockSettings(
	err error, packet nex.PacketInterface, callID uint32,
	pids types.List[types.UInt32],
) (*nex.RMCMessage, *nex.Error) {
	rmcResponseStream := nex.NewByteStreamOut(globals.SecureServer.LibraryVersions, globals.SecureServer.ByteStreamSettings)
	rmcResponseStream.WriteUInt32LE(0) // empty list

	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, rmcResponseStream.Bytes())
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodGetRequestBlockSettings
	return rmcResponse, nil
}

func DeletePersistentNotification(
	err error, packet nex.PacketInterface, callID uint32,
	notifications types.List[friends_wiiu_types.PersistentNotification],
) (*nex.RMCMessage, *nex.Error) {
	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, nil)
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodDeletePersistentNotification
	return rmcResponse, nil
}

func pretendoLookup(targetPID, callerPID uint64) (nnid, miiName string) {
	url := fmt.Sprintf("http://127.0.0.1:9191/internal/lookup/%d?caller=%d", targetPID, callerPID)
	resp, err := http.Get(url)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var r struct {
		PNID    string `json:"pnid"`
		MiiName string `json:"mii_name"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) == nil {
		nnid = r.PNID
		miiName = r.MiiName
	}
	return
}

func AcceptFriendRequest(
	err error, packet nex.PacketInterface, callID uint32,
	id types.UInt64,
) (*nex.RMCMessage, *nex.Error) {
	if err != nil {
		return nil, nex.NewError(nex.ResultCodes.Core.InvalidArgument, err.Error())
	}
	pid := uint64(packet.Sender().PID())
	requestID := uint64(id)

	requesterPID := database.GetFriendRequestByID(pid, requestID)
	database.DeleteIncomingFriendRequestByID(pid, requestID)

	if requesterPID != 0 {
		go func() {
			cmd := fmt.Sprintf(`{"cmd":"accept_friend_request","request_id":%d}`, requestID)
			if !database.ForwardPresenceCommand(pid, cmd) {
				database.QueuePretendoCommand(pid, "accept_friend_request", fmt.Sprintf(`{"request_id":%d}`, requestID))
				database.TriggerPretendoSync(pid)
			}
		}()
	}

	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, nil)
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodAcceptFriendRequest
	return rmcResponse, nil
}

func CancelFriendRequest(
	err error, packet nex.PacketInterface, callID uint32,
	id types.UInt64,
) (*nex.RMCMessage, *nex.Error) {
	if err != nil {
		return nil, nex.NewError(nex.ResultCodes.Core.InvalidArgument, err.Error())
	}
	pid := uint64(packet.Sender().PID())
	database.DeleteIncomingFriendRequestByID(pid, uint64(id))

	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, nil)
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodCancelFriendRequest
	return rmcResponse, nil
}

func AddFriendRequest(
	err error, packet nex.PacketInterface, callID uint32,
	pid types.PID, unknown2 types.UInt8, message types.String,
	unknown4 types.UInt8, unknown5 types.String,
	gameKey friends_wiiu_types.GameKey, unknown6 types.DateTime,
) (*nex.RMCMessage, *nex.Error) {
	if err != nil {
		return nil, nex.NewError(nex.ResultCodes.Core.InvalidArgument, err.Error())
	}
	callerPID := uint64(packet.Sender().PID())
	targetPID := uint64(pid)

	// Forward to Pretendo the same way AddFriend does.
	go func() {
		cmd := fmt.Sprintf(`{"cmd":"add_friend","target_pid":%d}`, targetPID)
		if !database.ForwardPresenceCommand(callerPID, cmd) {
			database.QueuePretendoCommand(callerPID, "add_friend", fmt.Sprintf(`{"target_pid":%d}`, targetPID))
			database.TriggerPretendoSync(callerPID)
		}
	}()

	// Look up the target's basic info for the response.
	nnid, miiName, miiData := database.GetBasicInfoForPID(targetPID)
	if nnid == "" {
		nnid, miiName = pretendoLookup(targetPID, callerPID)
	}

	fr := friends_wiiu_types.NewFriendRequest()
	fr.PrincipalInfo.PID = types.NewPID(targetPID)
	fr.PrincipalInfo.NNID = types.NewString(nnid)
	fr.PrincipalInfo.Mii.Name = types.NewString(miiName)
	fr.PrincipalInfo.Mii.MiiData = types.NewBuffer(miiData)
	fr.PrincipalInfo.Unknown = types.NewUInt8(2)
	fr.Message.FriendRequestID = types.NewUInt64(uint64(time.Now().UnixNano()))
	fr.Message.Received = types.NewBool(false)
	fr.Message.Unknown2 = unknown2
	fr.Message.Message = message
	fr.Message.Unknown3 = unknown4
	fr.Message.Unknown4 = unknown5
	fr.Message.GameKey = gameKey
	fr.Message.Unknown5 = unknown6
	fr.Message.ExpiresOn = fr.Message.ExpiresOn.Now()
	fr.SentOn = fr.SentOn.Now()

	rmcResponseStream := nex.NewByteStreamOut(globals.SecureServer.LibraryVersions, globals.SecureServer.ByteStreamSettings)
	fr.WriteTo(rmcResponseStream)
	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, rmcResponseStream.Bytes())
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodAddFriendRequest
	return rmcResponse, nil
}

func GetBasicInfo(
	err error, packet nex.PacketInterface, callID uint32,
	pids types.List[types.PID],
) (*nex.RMCMessage, *nex.Error) {
	callerPID := uint64(packet.Sender().PID())
	result := make(types.List[friends_wiiu_types.PrincipalBasicInfo], 0, len(pids))
	for _, pid := range pids {
		nnid, miiName, miiData := database.GetBasicInfoForPID(uint64(pid))
		if nnid == "" {
			nnid, miiName = pretendoLookup(uint64(pid), callerPID)
		}
		pbi := friends_wiiu_types.NewPrincipalBasicInfo()
		pbi.PID = types.NewPID(uint64(pid))
		pbi.NNID = types.NewString(nnid)
		pbi.Mii.Name = types.NewString(miiName)
		pbi.Mii.MiiData = types.NewBuffer(miiData)
		pbi.Unknown = types.NewUInt8(2)
		result = append(result, pbi)
	}

	rmcResponseStream := nex.NewByteStreamOut(globals.SecureServer.LibraryVersions, globals.SecureServer.ByteStreamSettings)
	result.WriteTo(rmcResponseStream)

	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, rmcResponseStream.Bytes())
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = friends_wiiu.MethodGetBasicInfo
	return rmcResponse, nil
}
