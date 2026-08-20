package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	mathrand "math/rand"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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
	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	if err = client.Connect(ctx); err != nil {
		panic(err)
	}
	db := client.Database("wsc")
	sessionsCol = db.Collection("minecraft_sessions")
	gatheringsCol = db.Collection("minecraft_gatherings")
	sessionsCol.DeleteMany(context.Background(), bson.D{})
	gatheringsCol.DeleteMany(context.Background(), bson.D{})
}

// ---- sessions (URL registry) ----

func dbSetURLs(pid uint32, urls []string) {
	filter := bson.D{{Key: "pid", Value: pid}}
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "pid", Value: pid},
		{Key: "urls", Value: urls},
	}}}
	sessionsCol.UpdateOne(context.Background(), filter, update, options.Update().SetUpsert(true))
}

func dbGetURLs(pid uint32) []string {
	var result bson.M
	err := sessionsCol.FindOne(context.Background(), bson.D{{Key: "pid", Value: pid}}).Decode(&result)
	if err != nil {
		return nil
	}
	raw := result["urls"].(bson.A)
	out := make([]string, len(raw))
	for i, v := range raw {
		out[i] = v.(string)
	}
	return out
}

// ---- gatherings ----

type Gathering struct {
	GID                uint32
	Owner              uint32
	Host               uint32
	Description        string
	GameMode           uint32
	Attribs            []uint32
	MinParticipants    uint16
	MaxParticipants    uint16
	ParticipationPolicy uint32
	PolicyArgument     uint32
	Flags              uint32
	State              uint32
	OpenParticipation  bool
	MatchmakeSystem    uint32
	ApplicationData    []byte
	ProgressScore      uint8
	SessionKey         []byte
	Participants       []uint32
}

func randomSessionKey() []byte {
	b := make([]byte, 16)
	rand.Read(b)
	return b
}

func randomSessionKeyHex() string {
	return hex.EncodeToString(randomSessionKey())
}

func dbNewGathering(owner uint32, g *Gathering) uint32 {
	for {
		gid := uint32(mathrand.Uint32()%500000) + 1
		var check bson.M
		if err := gatheringsCol.FindOne(context.Background(), bson.D{{Key: "gid", Value: gid}}).Decode(&check); err == nil {
			continue
		}
		g.GID = gid
		g.Owner = owner
		g.Host = owner
		if len(g.Participants) == 0 {
			g.Participants = []uint32{owner}
		}
		if len(g.SessionKey) == 0 {
			g.SessionKey = randomSessionKey()
		}
		attribs := g.Attribs
		if len(attribs) == 0 {
			attribs = []uint32{0, 0, 0, 0, 0, 0}
		}
		gatheringsCol.InsertOne(context.Background(), bson.D{
			{Key: "gid", Value: gid},
			{Key: "owner", Value: owner},
			{Key: "host", Value: owner},
			{Key: "description", Value: g.Description},
			{Key: "game_mode", Value: g.GameMode},
			{Key: "attribs", Value: attribs},
			{Key: "min_participants", Value: g.MinParticipants},
			{Key: "max_participants", Value: g.MaxParticipants},
			{Key: "participation_policy", Value: g.ParticipationPolicy},
			{Key: "policy_argument", Value: g.PolicyArgument},
			{Key: "flags", Value: g.Flags},
			{Key: "state", Value: g.State},
			{Key: "open_participation", Value: g.OpenParticipation},
			{Key: "matchmake_system", Value: g.MatchmakeSystem},
			{Key: "application_data", Value: g.ApplicationData},
			{Key: "progress_score", Value: int32(g.ProgressScore)},
			{Key: "session_key", Value: g.SessionKey},
			{Key: "participants", Value: g.Participants},
			{Key: "created", Value: time.Now().Unix()},
		})
		return gid
	}
}

func docToGathering(d bson.M) *Gathering {
	g := &Gathering{}
	g.GID = uint32(d["gid"].(int32))
	g.Owner = uint32(d["owner"].(int32))
	g.Host = uint32(d["host"].(int32))
	g.Description, _ = d["description"].(string)
	if gm, ok := d["game_mode"]; ok {
		g.GameMode = uint32(gm.(int32))
	}
	if raw, ok := d["attribs"].(bson.A); ok {
		for _, v := range raw {
			g.Attribs = append(g.Attribs, uint32(v.(int32)))
		}
	}
	if v, ok := d["min_participants"]; ok {
		g.MinParticipants = uint16(v.(int32))
	}
	if v, ok := d["max_participants"]; ok {
		g.MaxParticipants = uint16(v.(int32))
	}
	if v, ok := d["participation_policy"]; ok {
		g.ParticipationPolicy = uint32(v.(int32))
	}
	if v, ok := d["policy_argument"]; ok {
		g.PolicyArgument = uint32(v.(int32))
	}
	if v, ok := d["flags"]; ok {
		g.Flags = uint32(v.(int32))
	}
	if v, ok := d["state"]; ok {
		g.State = uint32(v.(int32))
	}
	g.OpenParticipation, _ = d["open_participation"].(bool)
	if v, ok := d["matchmake_system"]; ok {
		g.MatchmakeSystem = uint32(v.(int32))
	}
	if v, ok := d["application_data"].(primitive.Binary); ok {
		g.ApplicationData = v.Data
	}
	if v, ok := d["progress_score"]; ok {
		g.ProgressScore = uint8(v.(int32))
	} else {
		g.ProgressScore = 100
	}
	if v, ok := d["session_key"].(primitive.Binary); ok {
		g.SessionKey = v.Data
	}
	if raw, ok := d["participants"].(bson.A); ok {
		for _, v := range raw {
			g.Participants = append(g.Participants, uint32(v.(int32)))
		}
	}
	return g
}

func dbGetGathering(gid uint32) *Gathering {
	var result bson.M
	err := gatheringsCol.FindOne(context.Background(), bson.D{{Key: "gid", Value: gid}}).Decode(&result)
	if err != nil {
		return nil
	}
	return docToGathering(result)
}

func dbListGatherings() []*Gathering {
	cursor, err := gatheringsCol.Find(context.Background(), bson.D{})
	if err != nil {
		return nil
	}
	var docs []bson.M
	cursor.All(context.Background(), &docs)
	out := make([]*Gathering, 0, len(docs))
	for _, d := range docs {
		out = append(out, docToGathering(d))
	}
	return out
}

func dbJoinGathering(gid, pid uint32) bool {
	g := dbGetGathering(gid)
	if g == nil {
		return false
	}
	for _, p := range g.Participants {
		if p == pid {
			return true
		}
	}
	if uint16(len(g.Participants)) >= g.MaxParticipants {
		return false
	}
	newParts := append(g.Participants, pid)
	open := uint16(len(newParts)) < g.MaxParticipants
	gatheringsCol.UpdateOne(context.Background(),
		bson.D{{Key: "gid", Value: gid}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "participants", Value: newParts},
			{Key: "open_participation", Value: open},
		}}})
	return true
}

func dbLeaveGathering(gid, pid uint32) {
	g := dbGetGathering(gid)
	if g == nil {
		return
	}
	newParts := make([]uint32, 0, len(g.Participants))
	for _, p := range g.Participants {
		if p != pid {
			newParts = append(newParts, p)
		}
	}
	if len(newParts) == 0 {
		gatheringsCol.DeleteOne(context.Background(), bson.D{{Key: "gid", Value: gid}})
		return
	}
	newHost := g.Host
	if newHost == pid && len(newParts) > 0 {
		newHost = newParts[0]
	}
	gatheringsCol.UpdateOne(context.Background(),
		bson.D{{Key: "gid", Value: gid}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "participants", Value: newParts},
			{Key: "host", Value: newHost},
			{Key: "open_participation", Value: uint16(len(newParts)) < g.MaxParticipants},
		}}})
}

func dbLeaveAllGatherings(pid uint32) {
	cursor, err := gatheringsCol.Find(context.Background(), bson.D{{Key: "participants", Value: pid}})
	if err != nil {
		return
	}
	var docs []bson.M
	cursor.All(context.Background(), &docs)
	for _, d := range docs {
		gid := uint32(d["gid"].(int32))
		dbLeaveGathering(gid, pid)
	}
}

func dbSetOpen(gid uint32, open bool) {
	gatheringsCol.UpdateOne(context.Background(),
		bson.D{{Key: "gid", Value: gid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "open_participation", Value: open}}}})
}

func dbFindGatheringByPID(pid uint32) uint32 {
	var result bson.M
	err := gatheringsCol.FindOne(context.Background(), bson.D{{Key: "participants", Value: pid}}).Decode(&result)
	if err != nil {
		return 0
	}
	return uint32(result["gid"].(int32))
}
