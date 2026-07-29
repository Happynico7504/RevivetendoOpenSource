package main

import (
	"context"
	"math/rand"
	"os"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var sessionsCol *mongo.Collection
var gatheringsCol *mongo.Collection

func connectDB() {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		uri = "mongodb://localhost:27017/"
	}
	client, err := mongo.NewClient(options.Client().ApplyURI(uri))
	if err != nil {
		panic(err)
	}
	if err = client.Connect(context.Background()); err != nil {
		panic(err)
	}
	db := client.Database("wsc")
	sessionsCol = db.Collection("sessions")
	gatheringsCol = db.Collection("gatherings")
}

func dbUpsertSession(pid uint32, urls []string, ip, port string) {
	filter := bson.D{{Key: "pid", Value: pid}}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "pid", Value: pid},
		{Key: "urls", Value: urls},
		{Key: "ip", Value: ip},
		{Key: "port", Value: port},
	}}}
	sessionsCol.UpdateOne(context.Background(), filter, update, options.Update().SetUpsert(true))
}

func dbGetPlayerURLs(pid uint32) []string {
	var result bson.M
	err := sessionsCol.FindOne(context.Background(), bson.D{{Key: "pid", Value: pid}}).Decode(&result)
	if err != nil {
		return nil
	}
	raw := result["urls"].(bson.A)
	urls := make([]string, len(raw))
	for i, v := range raw {
		urls[i] = v.(string)
	}
	return urls
}

func dbUpdateSessionURL(pid uint32, oldURL, newURL string) {
	urls := dbGetPlayerURLs(pid)
	if urls == nil {
		return
	}
	for i, u := range urls {
		if u == oldURL {
			urls[i] = newURL
		}
	}
	sessionsCol.UpdateOne(context.Background(),
		bson.D{{Key: "pid", Value: pid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "urls", Value: urls}}}})
}

func dbFindGathering(gameMode uint32) uint32 {
	var result bson.M
	err := gatheringsCol.FindOne(context.Background(), bson.D{
		{Key: "game_mode", Value: gameMode},
		{Key: "open", Value: true},
	}).Decode(&result)
	if err != nil {
		return 0
	}
	return uint32(result["gid"].(int64))
}

func dbNewGathering(hostPID, gameMode, maxPlayers uint32) uint32 {
	for {
		gid := rand.Uint32()%500000 + 1
		var check bson.M
		err := gatheringsCol.FindOne(context.Background(), bson.D{{Key: "gid", Value: gid}}).Decode(&check)
		if err == nil {
			continue
		}
		gatheringsCol.InsertOne(context.Background(), bson.D{
			{Key: "gid", Value: gid},
			{Key: "host", Value: hostPID},
			{Key: "game_mode", Value: gameMode},
			{Key: "max_players", Value: maxPlayers},
			{Key: "player_count", Value: int64(1)},
			{Key: "players", Value: bson.A{hostPID}},
			{Key: "open", Value: maxPlayers > 1},
		})
		return gid
	}
}

func dbJoinGathering(gid, pid uint32) {
	var result bson.M
	err := gatheringsCol.FindOne(context.Background(), bson.D{{Key: "gid", Value: gid}}).Decode(&result)
	if err != nil {
		return
	}
	players := result["players"].(bson.A)
	for _, p := range players {
		if uint32(p.(int64)) == pid {
			return
		}
	}
	newPlayers := make([]interface{}, len(players)+1)
	copy(newPlayers, players)
	newPlayers[len(players)] = pid

	maxPlayers := result["max_players"].(int64)
	count := int64(len(newPlayers))
	gatheringsCol.UpdateOne(context.Background(),
		bson.D{{Key: "gid", Value: gid}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "players", Value: newPlayers},
			{Key: "player_count", Value: count},
			{Key: "open", Value: count < maxPlayers},
		}}})
}

func dbGetGatheringHost(gid uint32) uint32 {
	var result bson.M
	err := gatheringsCol.FindOne(context.Background(), bson.D{{Key: "gid", Value: gid}}).Decode(&result)
	if err != nil {
		return 0
	}
	return uint32(result["host"].(int64))
}

func dbUpdateGatheringHost(gid, pid uint32) {
	gatheringsCol.UpdateOne(context.Background(),
		bson.D{{Key: "gid", Value: gid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "host", Value: pid}}}})
}

func dbLeaveGathering(gid, pid uint32) {
	var result bson.M
	err := gatheringsCol.FindOne(context.Background(), bson.D{{Key: "gid", Value: gid}}).Decode(&result)
	if err != nil {
		return
	}
	players := result["players"].(bson.A)
	newPlayers := make([]interface{}, 0, len(players))
	for _, p := range players {
		if uint32(p.(int64)) != pid {
			newPlayers = append(newPlayers, p)
		}
	}
	if len(newPlayers) == 0 {
		gatheringsCol.DeleteOne(context.Background(), bson.D{{Key: "gid", Value: gid}})
		return
	}
	maxPlayers := result["max_players"].(int64)
	count := int64(len(newPlayers))
	newHost := result["host"]
	if uint32(result["host"].(int64)) == pid {
		newHost = newPlayers[0]
	}
	gatheringsCol.UpdateOne(context.Background(),
		bson.D{{Key: "gid", Value: gid}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "players", Value: newPlayers},
			{Key: "player_count", Value: count},
			{Key: "open", Value: count < maxPlayers},
			{Key: "host", Value: newHost},
		}}})
}
