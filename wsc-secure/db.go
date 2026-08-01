package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"time"

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
	// Clear stale data from previous run — all clients disconnect when the server restarts
	sessionsCol.DeleteMany(context.Background(), bson.D{})
	gatheringsCol.DeleteMany(context.Background(), bson.D{})
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

// cleanupStaleGatherings runs every 10 seconds and removes open gatherings
// whose host PID is no longer in connectedPIDs. This catches abrupt disconnects
// (game crash, power-off) faster than waiting for the PRUDP keep-alive timeout.
func cleanupStaleGatherings() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx := context.Background()
		cursor, err := gatheringsCol.Find(ctx, bson.D{{Key: "open", Value: true}})
		if err != nil {
			continue
		}
		var docs []bson.M
		if err := cursor.All(ctx, &docs); err != nil {
			continue
		}
		for _, d := range docs {
			gid := uint32(d["gid"].(int64))
			host := uint32(d["host"].(int64))
			if _, ok := connectedPIDs.Load(host); !ok {
				gatheringsCol.DeleteOne(ctx, bson.D{{Key: "gid", Value: gid}})
				fmt.Printf("CleanupGathering: purged stale gid=%d (host PID=%d offline)\n", gid, host)
			}
		}
	}
}

func dbDeleteSession(pid uint32) {
	sessionsCol.DeleteOne(context.Background(), bson.D{{Key: "pid", Value: pid}})
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

func dbFindGathering(gameMode uint32, requesterNatm uint32) uint32 {
	// Match on sport type (top byte only) — lower bytes encode region/settings which differ
	// between players in the same sport, e.g. 0x03022800 (EU) vs 0x03012400 (NA) are both bowling
	sportType := int64(gameMode >> 24)
	ctx := context.Background()

	filter := bson.D{
		{Key: "sport_type", Value: sportType},
		{Key: "open", Value: true},
	}
	// Only match gatherings whose host has a compatible NAT type.
	// natm ≤ 2 = open/cone (can hole-punch); natm = 3 = symmetric (cannot).
	// Unknown natm (0 or field absent) is treated as compatible with anyone.
	if requesterNatm > 0 {
		if requesterNatm <= 2 {
			filter = append(filter, bson.E{Key: "$or", Value: bson.A{
				bson.D{{Key: "host_natm", Value: bson.D{{Key: "$lte", Value: int64(2)}}}},
				bson.D{{Key: "host_natm", Value: bson.D{{Key: "$exists", Value: false}}}},
			}})
		} else {
			filter = append(filter, bson.E{Key: "$or", Value: bson.A{
				bson.D{{Key: "host_natm", Value: bson.D{{Key: "$gte", Value: int64(3)}}}},
				bson.D{{Key: "host_natm", Value: bson.D{{Key: "$exists", Value: false}}}},
			}})
		}
	}

	for {
		var result bson.M
		err := gatheringsCol.FindOne(ctx, filter).Decode(&result)
		if err != nil {
			return 0
		}
		gid := uint32(result["gid"].(int64))
		host := uint32(result["host"].(int64))
		if _, ok := connectedPIDs.Load(host); ok {
			return gid
		}
		// Host has disconnected — purge the stale gathering and keep looking
		res, err := gatheringsCol.DeleteOne(ctx, bson.D{{Key: "gid", Value: gid}})
		if err != nil || res.DeletedCount == 0 {
			return 0
		}
	}
}

func dbNewGathering(hostPID, gameMode, maxPlayers, hostNatm uint32) uint32 {
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
			{Key: "host_natm", Value: int64(hostNatm)},
			{Key: "game_mode", Value: gameMode},
			{Key: "sport_type", Value: int64(gameMode >> 24)},
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

func dbLeaveAllGatherings(pid uint32) {
	cursor, err := gatheringsCol.Find(context.Background(), bson.D{
		{Key: "players", Value: pid},
	})
	if err != nil {
		return
	}
	var results []bson.M
	cursor.All(context.Background(), &results)
	for _, r := range results {
		gid := uint32(r["gid"].(int64))
		dbLeaveGathering(gid, pid)
	}
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
