package main

import (
	"context"
	"log"
	"net"
	"os"

	pb "github.com/PretendoNetwork/grpc-go/friends"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/joho/godotenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type friendsServer struct {
	pb.UnimplementedFriendsServer
}

func (s *friendsServer) SendUserNotificationWiiU(ctx context.Context, req *pb.SendUserNotificationWiiURequest) (*empty.Empty, error) {
	log.Printf("SendUserNotificationWiiU: PID=%d, data=%d bytes", req.Pid, len(req.NotificationData))
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

func main() {
	godotenv.Load("../.env", "../../wiiu-chat-secure/.env")

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
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer(grpc.UnaryInterceptor(apiKeyInterceptor(apiKey)))
	pb.RegisterFriendsServer(s, &friendsServer{})

	log.Printf("friends gRPC listening on :%s", port)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
