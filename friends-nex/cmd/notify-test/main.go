package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"

	pb "github.com/PretendoNetwork/grpc-go/friends"
	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/nex-go/v2/types"
	friends_wiiu_constants "github.com/PretendoNetwork/nex-protocols-go/v2/friends-wiiu/constants"
	friends_wiiu_types "github.com/PretendoNetwork/nex-protocols-go/v2/friends-wiiu/types"
	nintendo_notifications_constants "github.com/PretendoNetwork/nex-protocols-go/v2/nintendo-notifications/constants"
	nintendo_notifications_types "github.com/PretendoNetwork/nex-protocols-go/v2/nintendo-notifications/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("usage: notify-test <target-pid> <caller-pid> <mode>\n  modes: online, offline, presence <json>, ring")
	}
	targetPIDInt, err := strconv.ParseUint(os.Args[1], 10, 32)
	if err != nil {
		log.Fatalf("invalid target pid: %v", err)
	}
	callerPIDInt := uint64(1)
	if len(os.Args) >= 3 {
		callerPIDInt, err = strconv.ParseUint(os.Args[2], 10, 32)
		if err != nil {
			log.Fatalf("invalid caller pid: %v", err)
		}
	}
	mode := "online"
	if len(os.Args) >= 4 {
		mode = os.Args[3]
	}

	callerPID := types.NewPID(callerPIDInt)

	libVersions := nex.NewLibraryVersions()
	libVersions.SetDefault(nex.NewLibraryVersion(1, 1, 0))
	bsSettings := nex.NewByteStreamSettings()
	bsSettings.UseStructureHeader = false

	eventObject := nintendo_notifications_types.NewNintendoNotificationEvent()
	eventObject.SenderPID = callerPID
	eventObject.DataHolder = types.NewDataHolder()

	presence := friends_wiiu_types.NewNintendoPresenceV2()
	presence.GameKey = friends_wiiu_types.NewGameKey()
	presence.PID = callerPID

	switch mode {
	case "online":
		eventObject.Type = nintendo_notifications_constants.NotificationType(24)
		presence.Online = true
		presence.ChangedFlags = friends_wiiu_constants.PresenceChangedFlag(
			friends_wiiu_constants.PresenceChangedFlagGameKey |
				friends_wiiu_constants.PresenceChangedFlagGameServerID |
				friends_wiiu_constants.PresenceChangedFlagOwnerPID |
				friends_wiiu_constants.PresenceChangedFlagGatheringID,
		)
		presence.GameServerID = types.NewUInt32(0x1010EB00)
		presence.GatheringID = types.NewUInt32(uint32(callerPIDInt))
		presence.GameKey.TitleID = types.NewUInt64(0x000500001010EB00)
		presence.GameKey.TitleVersion = types.NewUInt16(0)

	case "offline":
		eventObject.Type = nintendo_notifications_constants.NotificationType(10)
		presence.Online = false
		presence.ChangedFlags = friends_wiiu_constants.PresenceChangedFlagNone

	case "presence":
		// A friend's real presence, forwarded from Pretendo by fetch_friends.py's
		// NotificationForwarder as JSON (argument 4), so games see the actual
		// title, session and in-game activity.
		if len(os.Args) < 5 {
			log.Fatalf("presence mode needs the presence JSON as 4th argument")
		}
		var p struct {
			Flags        uint32 `json:"flags"`
			Online       bool   `json:"is_online"`
			TitleID      uint64 `json:"title_id"`
			TitleVersion uint16 `json:"title_version"`
			Unk1         uint8  `json:"unk1"`
			Message      string `json:"message"`
			Unk2         uint32 `json:"unk2"`
			Unk3         uint8  `json:"unk3"`
			GameServerID uint32 `json:"game_server_id"`
			Unk4         uint32 `json:"unk4"`
			PID          uint64 `json:"pid"`
			GatheringID  uint32 `json:"gathering_id"`
			AppDataHex   string `json:"app_data_hex"`
			Unk5         uint8  `json:"unk5"`
			Unk6         uint8  `json:"unk6"`
			Unk7         uint8  `json:"unk7"`
		}
		if err := json.Unmarshal([]byte(os.Args[4]), &p); err != nil {
			log.Fatalf("presence JSON: %v", err)
		}
		appData, err := hex.DecodeString(p.AppDataHex)
		if err != nil {
			log.Fatalf("presence app data: %v", err)
		}
		eventObject.Type = nintendo_notifications_constants.NotificationType(24)
		presence.ChangedFlags = friends_wiiu_constants.PresenceChangedFlag(p.Flags)
		presence.Online = types.NewBool(p.Online)
		presence.GameKey.TitleID = types.NewUInt64(p.TitleID)
		presence.GameKey.TitleVersion = types.NewUInt16(p.TitleVersion)
		presence.Unknown1 = types.NewUInt8(p.Unk1)
		presence.Message = types.NewString(p.Message)
		presence.Unknown2 = types.NewUInt32(p.Unk2)
		presence.Unknown3 = types.NewUInt8(p.Unk3)
		presence.GameServerID = types.NewUInt32(p.GameServerID)
		presence.Unknown4 = types.NewUInt32(p.Unk4)
		if p.PID != 0 {
			presence.PID = types.NewPID(p.PID)
		}
		presence.GatheringID = types.NewUInt32(p.GatheringID)
		presence.ApplicationData = types.NewBuffer(appData)
		presence.Unknown5 = types.NewUInt8(p.Unk5)
		presence.Unknown6 = types.NewUInt8(p.Unk6)
		presence.Unknown7 = types.NewUInt8(p.Unk7)

	case "ring":
		// Mirrors send_friends_notification.go exactly: type=0 (unset), PID=1 (count), no ChangedFlags.
		presence.Online = true
		presence.PID = types.NewPID(1)
		presence.GatheringID = types.NewUInt32(uint32(callerPIDInt))
		presence.Unknown2 = types.NewUInt32(0x65)
		presence.GameServerID = types.NewUInt32(0x1005A000)
		presence.GameKey.TitleID = types.NewUInt64(0x000500101005A100)
		presence.GameKey.TitleVersion = types.NewUInt16(55)
		appDataStream := nex.NewByteStreamOut(libVersions, bsSettings)
		types.NewPID(targetPIDInt).WriteTo(appDataStream)
		presence.ApplicationData = types.NewBuffer(appDataStream.Bytes())

	default:
		log.Fatalf("unknown mode %q — use: online, offline, ring", mode)
	}

	eventObject.DataHolder.Object = presence

	stream := nex.NewByteStreamOut(libVersions, bsSettings)
	eventObject.WriteTo(stream)
	eventBytes := stream.Bytes()
	fmt.Printf("[notify-test] mode=%s type=%d senderPID=%v bytes(%d): %x\n", mode, eventObject.Type, eventObject.SenderPID, len(eventBytes), eventBytes)

	apiKey := os.Getenv("PN_WUC_FRIENDS_GRPC_API_KEY")
	if apiKey == "" {
		apiKey = "54336802e28123736fc1918659dedb564e0fc981dc1342b52f526d2c514e9ac6"
	}
	port := os.Getenv("PN_WUC_FRIENDS_GRPC_PORT")
	if port == "" {
		port = "9002"
	}

	conn, err := grpc.NewClient("localhost:"+port, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	client := pb.NewFriendsClient(conn)
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-api-key", apiKey))

	_, err = client.SendUserNotificationWiiU(ctx, &pb.SendUserNotificationWiiURequest{
		Pid:              uint32(targetPIDInt),
		NotificationData: eventBytes,
	})
	if err != nil {
		log.Fatalf("SendUserNotificationWiiU: %v", err)
	}
	fmt.Printf("[%s] notification sent to PID=%d (caller PID=%d)\n", mode, targetPIDInt, callerPIDInt)
}
