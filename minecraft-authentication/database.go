package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var accountsCol *mongo.Collection
var accountMu sync.Mutex

func connectDB() {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017/"
	}
	client, err := mongo.NewClient(options.Client().ApplyURI(uri))
	if err != nil {
		panic(err)
	}
	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	if err = client.Connect(ctx); err != nil {
		panic(err)
	}
	accountsCol = client.Database("wsc").Collection("minecraft_nexaccounts")

	// Ensure username index for quick lookup
	accountsCol.Indexes().CreateOne(context.Background(), mongo.IndexModel{
		Keys:    bson.D{{Key: "username", Value: 1}},
		Options: options.Index().SetUnique(false),
	})
}

func nextPID() uint32 {
	var result bson.M
	opts := options.FindOne().SetSort(bson.D{{Key: "pid", Value: -1}})
	err := accountsCol.FindOne(context.Background(), bson.D{}, opts).Decode(&result)
	if err != nil {
		return 1000
	}
	return uint32(result["pid"].(int32)) + 1
}

func randomPassword() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// findOrCreateAccount looks up an account by username. If none exists, creates
// one. Returns (pid, nexPassword).
func findOrCreateAccount(username string) (uint32, string) {
	accountMu.Lock()
	defer accountMu.Unlock()

	var existing bson.M
	err := accountsCol.FindOne(context.Background(), bson.D{{Key: "username", Value: username}}).Decode(&existing)
	if err == nil {
		pid := uint32(existing["pid"].(int32))
		pass := existing["nex_password"].(string)
		return pid, pass
	}

	pid := nextPID()
	pass := randomPassword()
	accountsCol.InsertOne(context.Background(), bson.D{
		{Key: "pid", Value: int32(pid)},
		{Key: "username", Value: username},
		{Key: "nex_password", Value: pass},
	})
	return pid, pass
}

func getPasswordByPID(pid uint32) (string, bool) {
	var result bson.M
	err := accountsCol.FindOne(context.Background(), bson.D{{Key: "pid", Value: int32(pid)}}).Decode(&result)
	if err != nil {
		return "", false
	}
	return result["nex_password"].(string), true
}

func getUsernameByPID(pid uint32) string {
	var result bson.M
	err := accountsCol.FindOne(context.Background(), bson.D{{Key: "pid", Value: int32(pid)}}).Decode(&result)
	if err != nil {
		return ""
	}
	return result["username"].(string)
}

func getPIDByUsername(username string) uint32 {
	var result bson.M
	err := accountsCol.FindOne(context.Background(), bson.D{{Key: "username", Value: username}}).Decode(&result)
	if err != nil {
		return 0
	}
	return uint32(result["pid"].(int32))
}
