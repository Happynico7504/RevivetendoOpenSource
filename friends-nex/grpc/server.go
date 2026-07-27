package grpc

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"

	pb "github.com/PretendoNetwork/grpc-go/friends"
	"github.com/golang/protobuf/ptypes/empty"
	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/nex-go/v2/constants"
	nintendo_notifications "github.com/PretendoNetwork/nex-protocols-go/v2/nintendo-notifications"
	nintendo_notifications_types "github.com/PretendoNetwork/nex-protocols-go/v2/nintendo-notifications/types"
	"bytes"
	"encoding/json"
	"net/http"
	"github.com/PretendoNetwork/friends-nex/database"
	"github.com/PretendoNetwork/friends-nex/globals"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type friendsServer struct {
	pb.UnimplementedFriendsServer
}

func (s *friendsServer) GetUserFriendPIDs(ctx context.Context, req *pb.GetUserFriendPIDsRequest) (*pb.GetUserFriendPIDsResponse, error) {
	rows := database.GetFriends(uint64(req.Pid))
	pids := make([]uint32, 0, len(rows))
	for _, r := range rows {
		pids = append(pids, r.FriendPID)
	}
	fmt.Printf("[FRIENDS-GRPC] GetUserFriendPIDs: PID=%d → %d friends\n", req.Pid, len(pids))
	return &pb.GetUserFriendPIDsResponse{Pids: pids}, nil
}

func (s *friendsServer) SendUserNotificationWiiU(ctx context.Context, req *pb.SendUserNotificationWiiURequest) (*empty.Empty, error) {
	targetPID := uint64(req.Pid)

	val, ok := globals.ConnectedClients.Load(targetPID)
	if !ok {
		fmt.Printf("[FRIENDS-GRPC] SendUserNotificationWiiU: PID=%d not connected, dropping\n", targetPID)
		return &empty.Empty{}, nil
	}
	info := val.(*globals.ConnectionInfo)

	// Decode the NintendoNotificationEvent from the request bytes.
	fmt.Printf("[FRIENDS-GRPC] SendUserNotificationWiiU: raw bytes (%d): %x\n", len(req.NotificationData), req.NotificationData)
	eventObject := nintendo_notifications_types.NewNintendoNotificationEvent()
	stream := nex.NewByteStreamIn(req.NotificationData, globals.SecureServer.LibraryVersions, globals.SecureServer.ByteStreamSettings)
	if err := eventObject.ExtractFrom(stream); err != nil {
		fmt.Printf("[FRIENDS-GRPC] SendUserNotificationWiiU: failed to decode notification: %v\n", err)
		return &empty.Empty{}, nil
	}
	fmt.Printf("[FRIENDS-GRPC] SendUserNotificationWiiU: decoded type=%d senderPID=%v dataHolder=%v\n",
		eventObject.Type, eventObject.SenderPID, eventObject.DataHolder)

	// Re-encode as the parameters for ProcessNintendoNotificationEvent2.
	paramStream := nex.NewByteStreamOut(globals.SecureServer.LibraryVersions, globals.SecureServer.ByteStreamSettings)
	eventObject.WriteTo(paramStream)
	fmt.Printf("[FRIENDS-GRPC] SendUserNotificationWiiU: re-encoded bytes (%d): %x\n", len(paramStream.Bytes()), paramStream.Bytes())

	// Build RMC request targeting the client.
	rmcRequest := nex.NewRMCRequest(globals.SecureEndpoint)
	rmcRequest.ProtocolID = nintendo_notifications.ProtocolID
	rmcRequest.CallID = 0xFFFF
	rmcRequest.MethodID = nintendo_notifications.MethodProcessNintendoNotificationEvent2
	rmcRequest.Parameters = paramStream.Bytes()

	// Build PRUDP v0 packet to the target connection.
	prudpPacket, err := nex.NewPRUDPPacketV0(globals.SecureServer, info.Connection, nil)
	if err != nil {
		fmt.Printf("[FRIENDS-GRPC] SendUserNotificationWiiU: failed to create packet: %v\n", err)
		return &empty.Empty{}, nil
	}

	prudpPacket.SetType(constants.DataPacket)
	prudpPacket.AddFlag(constants.PacketFlagReliable)
	prudpPacket.AddFlag(constants.PacketFlagNeedsAck)
	prudpPacket.SetSourceVirtualPortStreamType(info.Connection.StreamType)
	prudpPacket.SetSourceVirtualPortStreamID(globals.SecureEndpoint.StreamID)
	prudpPacket.SetDestinationVirtualPortStreamType(info.Connection.StreamType)
	prudpPacket.SetDestinationVirtualPortStreamID(info.Connection.StreamID)
	prudpPacket.SetSubstreamID(info.SubstreamID)
	prudpPacket.SetPayload(rmcRequest.Bytes())

	globals.SecureServer.Send(prudpPacket)
	fmt.Printf("[FRIENDS-GRPC] SendUserNotificationWiiU: delivered to PID=%d\n", targetPID)

	// For ring notifications (type=0, WiiU Chat), also send a Discord DM via the bot.
	if eventObject.Type == 0 {
		go func() {
			body, _ := json.Marshal(map[string]any{
				"target_pid": targetPID,
				"caller_pid": uint64(eventObject.SenderPID),
			})
			resp, err := http.Post("http://127.0.0.1:9203/ring", "application/json", bytes.NewReader(body))
			if err != nil {
				fmt.Printf("[FRIENDS-GRPC] ring Discord DM failed: %v\n", err)
				return
			}
			resp.Body.Close()
		}()
	}

	return &empty.Empty{}, nil
}

func apiKeyInterceptor(apiKey string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok || len(md["x-api-key"]) == 0 || md["x-api-key"][0] != apiKey {
			return nil, status.Error(codes.Unauthenticated, "invalid API key")
		}
		return handler(ctx, req)
	}
}

func StartServer() {
	apiKey := os.Getenv("PN_WUC_FRIENDS_GRPC_API_KEY")
	if apiKey == "" {
		log.Fatal("PN_WUC_FRIENDS_GRPC_API_KEY not set")
	}

	port := os.Getenv("PN_WUC_FRIENDS_GRPC_PORT")
	if port == "" {
		port = "9002"
	}

	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("friends-nex gRPC: failed to listen: %v", err)
	}

	s := grpc.NewServer(grpc.UnaryInterceptor(apiKeyInterceptor(apiKey)))
	pb.RegisterFriendsServer(s, &friendsServer{})

	fmt.Printf("[FRIENDS-GRPC] listening on :%s\n", port)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("friends-nex gRPC: failed to serve: %v", err)
	}
}
