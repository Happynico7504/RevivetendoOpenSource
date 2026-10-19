package main

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	pbfriends "github.com/PretendoNetwork/grpc/go/friends"
	"os"
	"strconv"
	"strings"

	"github.com/PretendoNetwork/disney-infinity-3-nex/globals"
	"github.com/PretendoNetwork/nex-go/v2"
	common_globals "github.com/PretendoNetwork/nex-protocols-common-go/v2/globals"
	"github.com/PretendoNetwork/plogger-go"
	"github.com/joho/godotenv"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func init() {
	globals.Logger = plogger.NewLogger()

	var err error

	err = godotenv.Load()
	if err != nil {
		globals.Logger.Warning("Error loading .env file")
	}

	s3Endpoint := os.Getenv("PN_INFINITY3_CONFIG_S3_ENDPOINT")
	s3AccessKey := os.Getenv("PN_INFINITY3_CONFIG_S3_ACCESS_KEY")
	s3AccessSecret := os.Getenv("PN_INFINITY3_CONFIG_S3_ACCESS_SECRET")
	s3Bucket := os.Getenv("PN_INFINITY3_CONFIG_S3_BUCKET")
	s3KeyBase := os.Getenv("PN_INFINITY3_CONFIG_S3_KEY_BASE")
	s3Insecure := os.Getenv("PN_INFINITY3_CONFIG_S3_INSECURE")

	authenticationServerPort := os.Getenv("PN_INFINITY3_AUTHENTICATION_SERVER_PORT")
	secureServerHost := os.Getenv("PN_INFINITY3_SECURE_SERVER_HOST")
	secureServerPort := os.Getenv("PN_INFINITY3_SECURE_SERVER_PORT")
	accountGRPCHost := os.Getenv("PN_INFINITY3_ACCOUNT_GRPC_HOST")
	accountGRPCPort := os.Getenv("PN_INFINITY3_ACCOUNT_GRPC_PORT")
	accountGRPCAPIKey := os.Getenv("PN_INFINITY3_ACCOUNT_GRPC_API_KEY")
	friendsGRPCHost := os.Getenv("PN_INFINITY3_FRIENDS_GRPC_HOST")
	friendsGRPCPort := os.Getenv("PN_INFINITY3_FRIENDS_GRPC_PORT")
	friendsGRPCAPIKey := os.Getenv("PN_INFINITY3_FRIENDS_GRPC_API_KEY")
	healthCheckPort := os.Getenv("PN_INFINITY3_HEALTH_CHECK_PORT")

	kerberosPassword := make([]byte, 0x10)
	_, err = rand.Read(kerberosPassword)
	if err != nil {
		globals.Logger.Error("Error generating Kerberos password")
		os.Exit(0)
	}

	globals.KerberosPassword = string(kerberosPassword)

	globals.InitAccounts()

	if strings.TrimSpace(authenticationServerPort) == "" {
		globals.Logger.Error("PN_INFINITY3_AUTHENTICATION_SERVER_PORT environment variable not set")
		os.Exit(0)
	}

	if port, err := strconv.Atoi(authenticationServerPort); err != nil {
		globals.Logger.Errorf("PN_INFINITY3_AUTHENTICATION_SERVER_PORT is not a valid port. Expected 0-65535, got %s", authenticationServerPort)
		os.Exit(0)
	} else if port < 0 || port > 65535 {
		globals.Logger.Errorf("PN_INFINITY3_AUTHENTICATION_SERVER_PORT is not a valid port. Expected 0-65535, got %s", authenticationServerPort)
		os.Exit(0)
	}

	if strings.TrimSpace(secureServerHost) == "" {
		globals.Logger.Error("PN_INFINITY3_SECURE_SERVER_HOST environment variable not set")
		os.Exit(0)
	}

	if strings.TrimSpace(secureServerPort) == "" {
		globals.Logger.Error("PN_INFINITY3_SECURE_SERVER_PORT environment variable not set")
		os.Exit(0)
	}

	if port, err := strconv.Atoi(secureServerPort); err != nil {
		globals.Logger.Errorf("PN_INFINITY3_SECURE_SERVER_PORT is not a valid port. Expected 0-65535, got %s", secureServerPort)
		os.Exit(0)
	} else if port < 0 || port > 65535 {
		globals.Logger.Errorf("PN_INFINITY3_SECURE_SERVER_PORT is not a valid port. Expected 0-65535, got %s", secureServerPort)
		os.Exit(0)
	}

	if strings.TrimSpace(accountGRPCHost) == "" {
		globals.Logger.Error("PN_INFINITY3_ACCOUNT_GRPC_HOST environment variable not set")
		os.Exit(0)
	}

	if strings.TrimSpace(accountGRPCPort) == "" {
		globals.Logger.Error("PN_INFINITY3_ACCOUNT_GRPC_PORT environment variable not set")
		os.Exit(0)
	}

	accountPort, err := strconv.Atoi(accountGRPCPort)
	if err != nil {
		globals.Logger.Errorf("PN_INFINITY3_ACCOUNT_GRPC_PORT is not a valid port. Expected 0-65535, got %s", accountGRPCPort)
		os.Exit(0)
	} else if accountPort < 0 || accountPort > 65535 {
		globals.Logger.Errorf("PN_INFINITY3_ACCOUNT_GRPC_PORT is not a valid port. Expected 0-65535, got %s", accountGRPCPort)
		os.Exit(0)
	}

	if strings.TrimSpace(accountGRPCAPIKey) == "" {
		globals.Logger.Warning("Insecure gRPC server detected. PN_INFINITY3_ACCOUNT_GRPC_API_KEY environment variable not set")
	}

	common_globals.ConnectToAccountGRPC(accountGRPCHost, uint16(accountPort), accountGRPCAPIKey)

	if strings.TrimSpace(friendsGRPCHost) == "" {
		globals.Logger.Error("PN_INFINITY3_FRIENDS_GRPC_HOST environment variable not set")
		os.Exit(0)
	}

	if strings.TrimSpace(friendsGRPCPort) == "" {
		globals.Logger.Error("PN_INFINITY3_FRIENDS_GRPC_PORT environment variable not set")
		os.Exit(0)
	}

	if port, err := strconv.Atoi(friendsGRPCPort); err != nil {
		globals.Logger.Errorf("PN_INFINITY3_FRIENDS_GRPC_PORT is not a valid port. Expected 0-65535, got %s", accountGRPCPort)
		os.Exit(0)
	} else if port < 0 || port > 65535 {
		globals.Logger.Errorf("PN_INFINITY3_FRIENDS_GRPC_PORT is not a valid port. Expected 0-65535, got %s", accountGRPCPort)
		os.Exit(0)
	}

	if strings.TrimSpace(friendsGRPCAPIKey) == "" {
		globals.Logger.Warning("Insecure gRPC server detected. PN_INFINITY3_FRIENDS_GRPC_API_KEY environment variable not set")
	}

	globals.GRPCFriendsClientConnection, err = grpc.Dial(fmt.Sprintf("%s:%s", friendsGRPCHost, friendsGRPCPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		globals.Logger.Criticalf("Failed to connect to friends gRPC server: %v", err)
		os.Exit(0)
	}

	globals.GRPCFriendsClient = pbfriends.NewFriendsClient(globals.GRPCFriendsClientConnection)
	globals.GRPCFriendsCommonMetadata = metadata.Pairs(
		"X-API-Key", friendsGRPCAPIKey,
	)

	globals.Postgres, err = sql.Open("postgres", os.Getenv("PN_INFINITY3_POSTGRES_URI"))
	if err != nil {
		globals.Logger.Critical(err.Error())
	}

	globals.Logger.Success("Connected to Postgres!")

	secure := s3Insecure != "1"
	if !secure {
		globals.Logger.Warning("S3 is set to use HTTP! This is insecure.")
	}

	staticCredentials := credentials.NewStaticV4(s3AccessKey, s3AccessSecret, "")

	minIOClient, err := minio.New(s3Endpoint, &minio.Options{
		Creds:  staticCredentials,
		Secure: secure,
	})

	if err != nil {
		panic(err)
	}

	globals.MinIOClient = minIOClient
	globals.S3Manager = common_globals.NewMinIOManager(minIOClient)
	globals.S3Bucket = s3Bucket
	globals.S3KeyBase = s3KeyBase

	if strings.TrimSpace(healthCheckPort) == "" {
		globals.Logger.Warning("Basic UDP health check will not be enabled. PN_INFINITY3_HEALTH_CHECK_PORT environment variable not set")
	} else if port, err := strconv.Atoi(healthCheckPort); err != nil {
		globals.Logger.Errorf("PN_INFINITY3_HEALTH_CHECK_PORT is not a valid port. Expected 0-65535, got %s", healthCheckPort)
		os.Exit(0)
	} else if port < 0 || port > 65535 {
		globals.Logger.Errorf("PN_INFINITY3_HEALTH_CHECK_PORT is not a valid port. Expected 0-65535, got %s", healthCheckPort)
		os.Exit(0)
	} else {
		nex.EnableBasicUDPHealthCheck(port)
	}
}
