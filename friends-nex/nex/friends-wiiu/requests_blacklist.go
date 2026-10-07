package nex_friends_wiiu

// Friend request handling and the block list, following Pretendo's friends
// server (github.com/PretendoNetwork/friends, nex/friends-wiiu): local state is
// updated right away and every change is forwarded to Pretendo, which holds
// the real friend relationships.

import (
	"encoding/json"
	"fmt"

	"github.com/PretendoNetwork/friends-nex/database"
	"github.com/PretendoNetwork/friends-nex/globals"
	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/nex-go/v2/types"
	friends_wiiu "github.com/PretendoNetwork/nex-protocols-go/v2/friends-wiiu"
	friends_wiiu_types "github.com/PretendoNetwork/nex-protocols-go/v2/friends-wiiu/types"
)

// forwardToPretendo sends cmd with args to the player's live Pretendo
// connection, or queues it for the next sync when there is none.
func forwardToPretendo(pid uint64, cmd string, args map[string]any) {
	go func() {
		a, _ := json.Marshal(args)
		live := map[string]any{"cmd": cmd}
		for k, v := range args {
			live[k] = v
		}
		l, _ := json.Marshal(live)
		if !database.ForwardPresenceCommand(pid, string(l)) {
			database.QueuePretendoCommand(pid, cmd, string(a))
			database.TriggerPretendoSync(pid)
		}
	}()
}

// principalBasicInfo describes pid for responses; ok is false if the PID is
// unknown both locally and to Pretendo.
func principalBasicInfo(pid, callerPID uint64) (friends_wiiu_types.PrincipalBasicInfo, bool) {
	nnid, miiName, miiData := database.GetBasicInfoForPID(pid)
	if nnid == "" {
		nnid, miiName = pretendoLookup(pid, callerPID)
	}
	pbi := friends_wiiu_types.NewPrincipalBasicInfo()
	pbi.PID = types.NewPID(pid)
	pbi.NNID = types.NewString(nnid)
	pbi.Mii.Name = types.NewString(miiName)
	pbi.Mii.MiiData = types.NewBuffer(miiData)
	pbi.Unknown = types.NewUInt8(2)
	return pbi, nnid != ""
}

func rmcSuccess(callID, methodID uint32, body []byte) *nex.RMCMessage {
	rmcResponse := nex.NewRMCSuccess(globals.SecureEndpoint, body)
	rmcResponse.ProtocolID = friends_wiiu.ProtocolID
	rmcResponse.CallID = callID
	rmcResponse.MethodID = methodID
	return rmcResponse
}

func writeBlacklistedPrincipal(bp friends_wiiu_types.BlacklistedPrincipal) []byte {
	stream := nex.NewByteStreamOut(globals.SecureServer.LibraryVersions, globals.SecureServer.ByteStreamSettings)
	bp.WriteTo(stream)
	return stream.Bytes()
}

// DenyFriendRequest declines a received request and, like Pretendo, blocks
// its sender; the response is the new block list entry.
func DenyFriendRequest(
	err error, packet nex.PacketInterface, callID uint32,
	id types.UInt64,
) (*nex.RMCMessage, *nex.Error) {
	if err != nil {
		return nil, nex.NewError(nex.ResultCodes.FPD.InvalidArgument, err.Error())
	}
	pid := uint64(packet.Sender().PID())
	requestID := uint64(id)
	senderPID := database.GetFriendRequestByID(pid, requestID)
	if senderPID == 0 {
		return nil, nex.NewError(nex.ResultCodes.FPD.InvalidMessageID, fmt.Sprintf("no friend request %d", requestID))
	}
	database.DeleteIncomingFriendRequestByID(pid, requestID)
	database.AddBlocked(pid, senderPID, 0, 0)
	forwardToPretendo(pid, "deny_friend_request", map[string]any{"request_id": requestID})

	bp := friends_wiiu_types.NewBlacklistedPrincipal()
	bp.PrincipalBasicInfo, _ = principalBasicInfo(senderPID, pid)
	bp.GameKey = friends_wiiu_types.NewGameKey()
	bp.BlackListedSince = types.NewDateTime(0).Now()
	return rmcSuccess(callID, friends_wiiu.MethodDenyFriendRequest, writeBlacklistedPrincipal(bp)), nil
}

// DeleteFriendRequest removes a received request without blocking its sender.
func DeleteFriendRequest(
	err error, packet nex.PacketInterface, callID uint32,
	id types.UInt64,
) (*nex.RMCMessage, *nex.Error) {
	if err != nil {
		return nil, nex.NewError(nex.ResultCodes.FPD.InvalidArgument, err.Error())
	}
	pid := uint64(packet.Sender().PID())
	requestID := uint64(id)
	if database.GetFriendRequestByID(pid, requestID) == 0 {
		return nil, nex.NewError(nex.ResultCodes.FPD.InvalidMessageID, fmt.Sprintf("no friend request %d", requestID))
	}
	database.DeleteIncomingFriendRequestByID(pid, requestID)
	forwardToPretendo(pid, "delete_friend_request", map[string]any{"request_id": requestID})
	return rmcSuccess(callID, friends_wiiu.MethodDeleteFriendRequest, nil), nil
}

// MarkFriendRequestsAsReceived tells Pretendo the console has seen these
// requests (ours are always reported as received already).
func MarkFriendRequestsAsReceived(
	err error, packet nex.PacketInterface, callID uint32,
	ids types.List[types.UInt64],
) (*nex.RMCMessage, *nex.Error) {
	if err != nil {
		return nil, nex.NewError(nex.ResultCodes.FPD.InvalidArgument, err.Error())
	}
	if len(ids) > 0 {
		list := make([]uint64, 0, len(ids))
		for _, id := range ids {
			list = append(list, uint64(id))
		}
		forwardToPretendo(uint64(packet.Sender().PID()), "mark_friend_requests_as_received", map[string]any{"request_ids": list})
	}
	return rmcSuccess(callID, friends_wiiu.MethodMarkFriendRequestsAsReceived, nil), nil
}

// AddBlackList blocks a user; the response is the entry with the user's info
// filled in and today's date, as on Pretendo.
func AddBlackList(
	err error, packet nex.PacketInterface, callID uint32,
	blacklistedPrincipal friends_wiiu_types.BlacklistedPrincipal,
) (*nex.RMCMessage, *nex.Error) {
	if err != nil {
		return nil, nex.NewError(nex.ResultCodes.FPD.InvalidArgument, err.Error())
	}
	pid := uint64(packet.Sender().PID())
	blockedPID := uint64(blacklistedPrincipal.PrincipalBasicInfo.PID)
	info, ok := principalBasicInfo(blockedPID, pid)
	if !ok {
		return nil, nex.NewError(nex.ResultCodes.FPD.InvalidPrincipalID, fmt.Sprintf("unknown PID %d", blockedPID))
	}
	titleID := uint64(blacklistedPrincipal.GameKey.TitleID)
	titleVersion := uint16(blacklistedPrincipal.GameKey.TitleVersion)
	database.AddBlocked(pid, blockedPID, titleID, titleVersion)
	forwardToPretendo(pid, "add_black_list", map[string]any{"target_pid": blockedPID, "title_id": titleID, "title_version": titleVersion})

	blacklistedPrincipal.PrincipalBasicInfo = info
	blacklistedPrincipal.BlackListedSince = types.NewDateTime(0).Now()
	return rmcSuccess(callID, friends_wiiu.MethodAddBlackList, writeBlacklistedPrincipal(blacklistedPrincipal)), nil
}

// RemoveBlackList unblocks a user. Blocks made before we kept a local list
// only exist on Pretendo, so an unknown PID is still forwarded and succeeds.
func RemoveBlackList(
	err error, packet nex.PacketInterface, callID uint32,
	pid types.PID,
) (*nex.RMCMessage, *nex.Error) {
	if err != nil {
		return nil, nex.NewError(nex.ResultCodes.FPD.InvalidArgument, err.Error())
	}
	ownerPID := uint64(packet.Sender().PID())
	database.RemoveBlocked(ownerPID, uint64(pid))
	forwardToPretendo(ownerPID, "remove_black_list", map[string]any{"target_pid": uint64(pid)})
	return rmcSuccess(callID, friends_wiiu.MethodRemoveBlackList, nil), nil
}

// blacklistFor builds the player's block list for UpdateAndGetAllInformation.
func blacklistFor(pid uint64) types.List[friends_wiiu_types.BlacklistedPrincipal] {
	rows := database.GetBlocked(pid)
	list := make(types.List[friends_wiiu_types.BlacklistedPrincipal], 0, len(rows))
	for _, r := range rows {
		bp := friends_wiiu_types.NewBlacklistedPrincipal()
		bp.PrincipalBasicInfo, _ = principalBasicInfo(r.BlockedPID, pid)
		bp.GameKey = friends_wiiu_types.NewGameKey()
		bp.GameKey.TitleID = types.NewUInt64(r.TitleID)
		bp.GameKey.TitleVersion = types.NewUInt16(r.TitleVersion)
		bp.BlackListedSince.FromTimestamp(r.Since)
		list = append(list, bp)
	}
	return list
}
