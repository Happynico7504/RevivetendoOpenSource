package nex_datastore

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/database"
	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/globals"

	"github.com/PretendoNetwork/nex-go"
	"github.com/PretendoNetwork/nex-protocols-go/datastore"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

func PrepareGetObject(err error, client *nex.Client, callID uint32, dataStorePrepareGetParam *datastore.DataStorePrepareGetParam) {
	pReqGetInfo := datastore.NewDataStoreReqGetInfo()

	dataVersion := database.GetVersionByDataID(uint32(dataStorePrepareGetParam.DataID))

	bucket := os.Getenv("PN_NBA_CONFIG_S3_BUCKET")
	key := fmt.Sprintf("%s/%011d-%05d", os.Getenv("PN_NBA_CONFIG_S3_PATH"), dataStorePrepareGetParam.DataID, dataVersion)
	dataSize, err := globals.S3ObjectSize(bucket, key)
	if err != nil {
		globals.Logger.Error(err.Error())
	}

	// Previously an unsigned, bucket-less URL (fmt.Sprintf("%s/%s", endpoint, key) -
	// missing the bucket entirely and never actually presigned) that could never
	// have worked against a private bucket. Now a real presigned GET, relayed
	// through the same already-3DS-trusted host used for uploads (see
	// globals.RelayThroughHPPHost's doc comment) since the 3DS won't trust
	// Exoscale's real TLS cert directly either way.
	presignClient := awss3.NewPresignClient(globals.S3Client)
	presigned, presignErr := presignClient.PresignGetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}, awss3.WithPresignExpires(15*time.Minute))
	if presignErr != nil {
		globals.Logger.Error(presignErr.Error())
	} else {
		pReqGetInfo.URL = globals.RelayThroughHPPHost(presigned.URL)
	}
	pReqGetInfo.RequestHeaders = []*datastore.DataStoreKeyValue{}
	pReqGetInfo.Size = uint32(dataSize)
	pReqGetInfo.RootCA = globals.NNCACertDER()
	pReqGetInfo.DataID = dataStorePrepareGetParam.DataID

	rmcResponseStream := nex.NewStreamOut(globals.NEXServer)

	rmcResponseStream.WriteStructure(pReqGetInfo)

	rmcResponseBody := rmcResponseStream.Bytes()

	rmcResponse := nex.NewRMCResponse(datastore.ProtocolID, callID)
	rmcResponse.SetSuccess(datastore.MethodPrepareGetObject, rmcResponseBody)

	rmcResponseBytes := rmcResponse.Bytes()

	responsePacket, _ := nex.NewPacketV1(client, nil)

	responsePacket.SetVersion(1)
	responsePacket.SetSource(0xA1)
	responsePacket.SetDestination(0xAF)
	responsePacket.SetType(nex.DataPacket)
	responsePacket.SetPayload(rmcResponseBytes)

	responsePacket.AddFlag(nex.FlagNeedsAck)
	responsePacket.AddFlag(nex.FlagReliable)

	globals.NEXServer.Send(responsePacket)
}
