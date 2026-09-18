package main

import (
	"context"
	"os"

	"github.com/PretendoNetwork/plogger-go"
	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/database"
	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/globals"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/joho/godotenv"
)

var logger = plogger.NewLogger()

func init() {
	err := godotenv.Load()

	if err != nil {
		logger.Warning("Error loading .env file")
	}

	s3Endpoint := os.Getenv("PN_NBA_CONFIG_S3_ENDPOINT")
	s3Region := os.Getenv("PN_NBA_CONFIG_S3_REGION")
	s3AccessKey := os.Getenv("PN_NBA_CONFIG_S3_ACCESS_KEY")
	s3AccessSecret := os.Getenv("PN_NBA_CONFIG_S3_ACCESS_SECRET")

	staticCredentials := credentials.NewStaticCredentialsProvider(s3AccessKey, s3AccessSecret, "")

	// SigningRegion must be set here explicitly - a custom EndpointResolverWithOptions
	// silently overrides cfg.Region for SigV4 signing purposes on S3, and an empty
	// SigningRegion produces a credential scope like ".../20260915//s3/aws4_request"
	// (empty region between the two slashes), which Exoscale rejects with a plain
	// 400 Bad Request. Confirmed 2026-09-15 via raw SDK request/response logging.
	endpointResolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
		return aws.Endpoint{
			URL:           s3Endpoint,
			SigningRegion: s3Region,
		}, nil
	})

	cfg, err := config.LoadDefaultConfig(
		context.TODO(),
		config.WithRegion(s3Region),
		config.WithCredentialsProvider(staticCredentials),
		config.WithEndpointResolverWithOptions(endpointResolver),
	)

	if err != nil {
		panic(err)
	}

	// UsePathStyle is required for Exoscale SOS: aws-sdk-go-v2 defaults to
	// virtual-hosted-style addressing (bucket.endpoint), which Exoscale's S3 API
	// rejects with a 400 Bad Request even though DNS/TLS resolve fine for the
	// wildcard subdomain - confirmed 2026-09-15 via a real HeadObject 400.
	globals.S3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})
	globals.S3PresignClient = globals.NewPresignClient(cfg)

	database.ConnectAll()
}
