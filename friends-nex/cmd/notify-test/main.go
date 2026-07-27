package main

import (
	"context"
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
		log.Fatalf("usage: notify-test <target-pid> <caller-pid> <mode>\n  modes: online, offline, ring")
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
