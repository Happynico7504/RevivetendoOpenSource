package globals

import (
	"database/sql"
	pbfriends "github.com/PretendoNetwork/grpc/go/friends"
	"github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/nex-protocols-common-go/v2/globals"
	"github.com/PretendoNetwork/plogger-go"
	"github.com/minio/minio-go/v7"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

var Logger *plogger.Logger
var KerberosPassword = "password" // * Default password

var AuthenticationServer *nex.PRUDPServer
var AuthenticationEndpoint *nex.PRUDPEndPoint

var SecureServer *nex.PRUDPServer
var SecureEndpoint *nex.PRUDPEndPoint

var Postgres *sql.DB
var MatchmakingManager *common_globals.MatchmakingManager
var MessagingManager *common_globals.MessagingManager
var UtilityManager *common_globals.UtilityManager
var DatastoreManager *common_globals.DataStoreManager

var GRPCFriendsClientConnection *grpc.ClientConn
var GRPCFriendsClient pbfriends.FriendsClient
var GRPCFriendsCommonMetadata metadata.MD

var MinIOClient *minio.Client
var S3Manager *common_globals.MinIOManager
var S3Bucket string
var S3KeyBase string
