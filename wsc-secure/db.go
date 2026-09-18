package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var sessionsCol *mongo.Collection
var gatheringsCol *mongo.Collection
var matchHistoryCol *mongo.Collection
var rankingScoresCol *mongo.Collection
var rankingCommonDataCol *mongo.Collection
var juxtCommunitiesCol *mongo.Collection
var clubSearchesCol *mongo.Collection

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
	matchHistoryCol = db.Collection("match_history")
	rankingScoresCol = db.Collection("ranking_scores")
	rankingCommonDataCol = db.Collection("ranking_common_data")
	clubSearchesCol = db.Collection("club_searches")
	// Same MongoDB instance, different logical database - Juxt (the Miiverse
	// revival) owns the real club/community names, keyed by the same club code
	// this server already uses in its own eu_NNN/us_NNN DataStore tags. See
	// resolveClubName.
	juxtCommunitiesCol = client.Database("juxt").Collection("communities")
	// Clear stale data from previous run — all clients disconnect when the server restarts
	sessionsCol.DeleteMany(context.Background(), bson.D{})
	gatheringsCol.DeleteMany(context.Background(), bson.D{})
	// match_history, ranking_scores, ranking_common_data are intentionally not
	// cleared — we want history across restarts
}

// dbInsertRankingScore persists a real UploadScore call. category/groups/param
// are logged verbatim (not yet mapped to DataStore search tags — see the
// comment above handleUploadScore in main.go) so real values from actual
// gameplay can be used to reverse-engineer that mapping.
func dbInsertRankingScore(pid uint32, category, score uint32, order, updateMode uint8, groups []uint8, param, uniqueID uint64) {
	groupInts := make([]int32, len(groups))
	for i, g := range groups {
		groupInts[i] = int32(g)
	}
	rankingScoresCol.InsertOne(context.Background(), bson.D{
		{Key: "pid", Value: pid},
		{Key: "category", Value: category},
		{Key: "score", Value: score},
		{Key: "order", Value: order},
		{Key: "update_mode", Value: updateMode},
		{Key: "groups", Value: groupInts},
		{Key: "param", Value: int64(param)},
		{Key: "unique_id", Value: int64(uniqueID)},
		{Key: "created_at", Value: time.Now().Unix()},
	})
}

// dbInsertRankingCommonData persists a real UploadCommonData call.
func dbInsertRankingCommonData(pid uint32, commonData []byte, uniqueID uint64) {
	rankingCommonDataCol.InsertOne(context.Background(), bson.D{
		{Key: "pid", Value: pid},
		{Key: "common_data", Value: commonData},
		{Key: "unique_id", Value: int64(uniqueID)},
		{Key: "created_at", Value: time.Now().Unix()},
	})
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

// cleanupStaleGatherings runs every 10 seconds and reconciles open gatherings against
// connectedPIDs. If the host is gone, the whole gathering is purged (a dead host means
// no one can join or resolve session URLs anyway). Otherwise, any non-host player who
// dropped out of connectedPIDs is pruned individually via dbLeaveGathering, so the
// players/player_count shown on the dashboard stay accurate for joiners too, not just
// hosts. This is a faster/redundant path on top of the PRUDP ping-timeout-driven
// Disconnect handler (see SetPingTimeout in main.go), which still runs as the source
// of truth for connectedPIDs itself.
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
				continue
			}
			players, _ := d["players"].(bson.A)
			for _, p := range players {
				pid := uint32(p.(int64))
				if pid == host {
					continue
				}
				if _, ok := connectedPIDs.Load(pid); !ok {
					dbLeaveGathering(gid, pid)
					fmt.Printf("CleanupGathering: pruned stale player PID=%d from gid=%d\n", pid, gid)
				}
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
	// Byte 3: sport type, Byte 2: sub-mode/round state (must match — e.g. golfers matched
	// together carry the same value here as it climbs in lockstep through holes), Byte 1:
	// region (ignored — confirmed constant per player across sports, e.g. 0x24 for NA
	// accounts vs 0x28 for EU, not tied to game mode), Byte 0: always 0.
	// Match on sport+sub-mode but not region so cross-region play works within the same mode.
	matchKey := int64(gameMode & 0xFFFF0000)
	ctx := context.Background()

	filter := bson.D{
		{Key: "match_key", Value: matchKey},
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

	skippedGids := bson.A{}
	for {
		queryFilter := filter
		if len(skippedGids) > 0 {
			queryFilter = append(bson.D{{Key: "gid", Value: bson.D{{Key: "$nin", Value: skippedGids}}}}, filter...)
		}
		var result bson.M
		err := gatheringsCol.FindOne(ctx, queryFilter).Decode(&result)
		if err != nil {
			return 0
		}
		gid := uint32(result["gid"].(int64))
		host := uint32(result["host"].(int64))
		hostLooksDead := false
		if _, ok := connectedPIDs.Load(host); !ok {
			hostLooksDead = true
		} else if last, ok := lastPacketAt.Load(host); !ok || time.Since(last.(time.Time)) > 2*staleIdleThreshold {
			// connectedPIDs alone is too weak a liveness signal: it's only
			// cleared by an explicit Disconnect/StaleDisconnect, and a host can
			// go completely silent for minutes before either reconnecting on
			// its own (which happens on the HOST's own timing, not ours) or
			// being caught by watchStaleConnections's own ticker. Confirmed
			// 2026-09-15: a host (PID 1626116659) was offered to a new joiner
			// via this function after 7m50s of total silence, moments before
			// reconnecting fresh - the joiner's RequestProbeInitiationExt was
			// delivered to that now-dead session (no error, since
			// currentClient/connectedPIDs both still listed it) and vanished,
			// guaranteeing the match failed. A genuinely-waiting host (not yet
			// in a match) pings every ~5-9s per staleIdleThreshold's own
			// baseline, so 2x that margin catches real staleness without
			// flagging normal jitter.
			hostLooksDead = true
		}
		if hostLooksDead {
			// Host has disconnected, or is stale enough to be effectively dead
			// even though nothing has formally cleaned it up yet — either way,
			// purge the gathering and keep looking rather than offer it.
			res, err := gatheringsCol.DeleteOne(ctx, bson.D{{Key: "gid", Value: gid}})
			if err != nil || res.DeletedCount == 0 {
				return 0
			}
			continue
		}
		if isBlockedFromMatchmaking(host) {
			// Host just failed NAT traversal and is serving out a matchmaking
			// block - see natFailureBlockedUntil's doc comment. Their gathering
			// is still legitimate (don't delete it), just not offered to other
			// searchers for now.
			skippedGids = append(skippedGids, int64(gid))
			continue
		}
		return gid
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
			{Key: "match_key", Value: int64(gameMode & 0xFFFF0000)},
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

// dbRecordMatch snapshots the gathering into match_history when a match starts (CloseParticipation).
func dbFindGatheringForPID(pid uint32) uint32 {
	var result bson.M
	err := gatheringsCol.FindOne(context.Background(),
		bson.D{{Key: "players", Value: pid}}).Decode(&result)
	if err != nil {
		return 0
	}
	return uint32(result["gid"].(int64))
}

func dbCloseGathering(gid uint32) {
	gatheringsCol.UpdateOne(context.Background(),
		bson.D{{Key: "gid", Value: gid}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "open", Value: false}}}})
}

// dbGetGatheringPlayers returns every PID currently listed in a gathering's
// "players" array (host included), for CloseParticipation to clear the
// hole-punch exemption on the whole match at once - see its call site's
// doc comment.
func dbGetGatheringPlayers(gid uint32) []uint32 {
	var g bson.M
	if err := gatheringsCol.FindOne(context.Background(), bson.D{{Key: "gid", Value: int64(gid)}}).Decode(&g); err != nil {
		return nil
	}
	raw, ok := g["players"].(bson.A)
	if !ok {
		return nil
	}
	pids := make([]uint32, 0, len(raw))
	for _, p := range raw {
		switch v := p.(type) {
		case int32:
			pids = append(pids, uint32(v))
		case int64:
			pids = append(pids, uint32(v))
		}
	}
	return pids
}

func dbRecordMatch(gid uint32) {
	ctx := context.Background()
	var g bson.M
	if err := gatheringsCol.FindOne(ctx, bson.D{{Key: "gid", Value: int64(gid)}}).Decode(&g); err != nil {
		return
	}
	matchHistoryCol.InsertOne(ctx, bson.D{
		{Key: "gid", Value: int64(gid)},
		{Key: "sport_type", Value: g["sport_type"]},
		{Key: "game_mode", Value: g["game_mode"]},
		{Key: "host", Value: g["host"]},
		{Key: "players", Value: g["players"]},
		{Key: "player_count", Value: g["player_count"]},
		{Key: "started_at", Value: time.Now().Unix()},
	})
}

// dbGetRecentMatches returns match_history documents from the last 24 hours, newest first.
func dbGetRecentMatches() []bson.M {
	ctx := context.Background()
	since := time.Now().Add(-24 * time.Hour).Unix()
	opts := options.Find().
		SetSort(bson.D{{Key: "started_at", Value: -1}}).
		SetLimit(200)
	cur, err := matchHistoryCol.Find(ctx,
		bson.D{{Key: "started_at", Value: bson.D{{Key: "$gte", Value: since}}}}, opts)
	if err != nil {
		return nil
	}
	var docs []bson.M
	cur.All(ctx, &docs)
	return docs
}

// clubSearchTagRe matches the per-club DataStore search tags WSC issues on
// startup, e.g. "us_016_ave" / "eu_033_vs_record".
var clubSearchTagRe = regexp.MustCompile(`^(us|eu|jp)_(\d{3})_(ave|vs_record)$`)

// dbRecordClubSearch keeps one document per region+club code (_id "us_016")
// counting how often that club's ranking was searched, how many of those found
// nothing, and which players asked. This is how missing clubs get discovered
// over time: any doc with empty_searches > 0 is a club players are using that
// we can't serve data for yet, and "name" is filled once Juxt has a community
// whose app_data matches (see resolveClubName) so unresolved ones stand out.
func dbRecordClubSearch(pid uint32, tags []string, resultCount int) {
	if clubSearchesCol == nil {
		return
	}
	for _, tag := range tags {
		m := clubSearchTagRe.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		region, codeStr := m[1], m[2]
		code, _ := strconv.Atoi(codeStr)
		inc := bson.D{{Key: "searches", Value: 1}}
		if resultCount == 0 {
			inc = append(inc, bson.E{Key: "empty_searches", Value: 1})
			fmt.Printf("ClubSearch: MISSING DATA region=%s club=%03d pid=%d tag=%s\n", region, code, pid, tag)
		}
		set := bson.D{
			{Key: "region", Value: region},
			{Key: "club_code", Value: code},
			{Key: "last_seen", Value: time.Now().Unix()},
			{Key: "last_result_count", Value: resultCount},
		}
		if name, ok := resolveClubName(region, uint32(code)); ok {
			set = append(set, bson.E{Key: "name", Value: name})
		}
		update := bson.D{
			{Key: "$inc", Value: inc},
			{Key: "$addToSet", Value: bson.D{{Key: "players", Value: pid}}},
			{Key: "$set", Value: set},
			{Key: "$setOnInsert", Value: bson.D{{Key: "first_seen", Value: time.Now().Unix()}}},
		}
		clubSearchesCol.UpdateOne(context.Background(), bson.D{{Key: "_id", Value: region + "_" + codeStr}}, update, options.Update().SetUpsert(true))
	}
}
