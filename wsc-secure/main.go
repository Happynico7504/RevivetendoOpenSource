package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	nex "github.com/PretendoNetwork/nex-go"
	"github.com/PretendoNetwork/nex-protocols-go/datastore"
	match_making "github.com/PretendoNetwork/nex-protocols-go/match-making"
	match_making_ext "github.com/PretendoNetwork/nex-protocols-go/match-making-ext"
	matchmake_extension "github.com/PretendoNetwork/nex-protocols-go/matchmake-extension"
	nat_traversal "github.com/PretendoNetwork/nex-protocols-go/nat-traversal"
	ranking "github.com/PretendoNetwork/nex-protocols-go/ranking"
	secure_connection "github.com/PretendoNetwork/nex-protocols-go/secure-connection"
	utility "github.com/PretendoNetwork/nex-protocols-go/utility"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// --- In-memory DataStore ---

type dsObject struct {
	DataID     uint64    `json:"data_id"`
	OwnerPID   uint32    `json:"owner_pid"`
	DataType   uint16    `json:"data_type"`
	MetaBinary []byte    `json:"meta_binary"`
	Tags       []string  `json:"tags"`
	Name       string    `json:"name"`
	Flag       uint32    `json:"flag"`
	Period     uint16    `json:"period"`
	Size       uint32    `json:"size"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`

	// RankCategory/RankGroups identify which ranking slot (see
	// dsAllocRankingScore) this object holds, if any. Only set on objects
	// created by dsAllocRankingScore - used to find-and-update the existing
	// slot on a re-upload instead of piling up duplicate objects that
	// SearchObject's unordered dsStore.Range could return in place of the
	// newest one (the "club info doesn't update its stats" bug, 2026-08-23).
	RankCategory uint32 `json:"rank_category,omitempty"`
	RankGroups   []byte `json:"rank_groups,omitempty"`
}

var (
	dsStore  sync.Map // uint64 → *dsObject
	dsNextID uint64   = 1
)

func dsAlloc(ownerPID uint32, p *datastore.DataStorePreparePostParam) *dsObject {
	id := atomic.AddUint64(&dsNextID, 1) - 1
	now := time.Now()
	obj := &dsObject{
		DataID:     id,
		OwnerPID:   ownerPID,
		DataType:   p.DataType,
		MetaBinary: p.MetaBinary,
		Tags:       p.Tags,
		Name:       p.Name,
		Flag:       p.Flag,
		Period:     p.Period,
		Size:       p.Size,
		Created:    now,
		Updated:    now,
	}
	dsStore.Store(id, obj)
	go dsSave()
	return obj
}

// dsClubRegionPrefix derives the region prefix WSC's SearchObject tags use
// ("eu_"/"us_") from the DataType of the player's own club-selection profile
// object (updated via ChangeMeta) - DataType 2 = US (Americas cart), DataType
// 3 = EU (matches dsSeedProfiles' real recovered-community-server seeds,
// which are explicitly labelled by DataType this same way).
//
// The previous version of this function tried to read a "region byte" at
// offset 0x5b of the 312-byte blob instead, based on a single confirmed NA
// sample cross-referenced against an unrelated matchmaking-region comment.
// That byte turned out NOT to encode region at all: a live-confirmed EU
// account (Nico, via Germany/Bavaria) and a live-confirmed US account (real
// player "nick" - client observed searching "us_032_ave" etc.) both had the
// exact same byte value (0x29) at that offset, while their profile objects'
// DataType correctly differed (3 vs 2 respectively). "nick" was invisible to
// the old byte-based check entirely, since it only ever looked at DataType-3
// objects - his real profile lives under DataType 2, so lookups for his PID
// always fell through to "not found" -> "eu", even though his uploaded
// scores needed "us_" tags to be findable by his own client's searches.
func dsClubRegionPrefix(ownerPID uint32) string {
	prefix := "eu"
	dsStore.Range(func(_, v interface{}) bool {
		obj := v.(*dsObject)
		if obj.OwnerPID != ownerPID {
			return true
		}
		switch obj.DataType {
		case 2:
			prefix = "us"
			return false
		case 3:
			prefix = "eu"
			return false
		}
		return true
	})
	return prefix
}

// juxtMainCommunityByRegion holds the real Miiverse main-community IDs seeded
// from Archiverse (see juxt/apps/miiverse-api/scripts/seedWscAllRegions.ts) -
// fixed once seeded, since they're the real archived GameID values, not
// randomly generated. Used only to scope resolveClubName's lookup to the
// right region's community tree.
var juxtMainCommunityByRegion = map[string]string{
	"us": "14866558073079465304", // America
	"eu": "14866558073079666960", // Europe
	"jp": "14866558073078154134", // Japan
}

// resolveClubName looks up the real-world name for a club code by matching it
// 1:1 against Juxt's communities collection: find the sub-community parented
// under the matching region's real main community whose app_data equals the
// zero-padded 3-digit code (the same "%03d" format WSC's own club-info screen
// matches locally - see the app_data comment on Community.create in
// seedWscAllRegions.ts). Only resolves what's actually been confirmed and
// entered there (currently just GER Hesse = "033") - returns ok=false for
// anything not yet known, rather than guessing.
func resolveClubName(regionPrefix string, code uint32) (string, bool) {
	mainID, ok := juxtMainCommunityByRegion[regionPrefix]
	if !ok || juxtCommunitiesCol == nil {
		return "", false
	}
	var doc struct {
		Name string `bson:"name"`
	}
	err := juxtCommunitiesCol.FindOne(context.Background(), bson.M{
		"parent":   mainID,
		"app_data": fmt.Sprintf("%03d", code),
	}).Decode(&doc)
	if err != nil {
		return "", false
	}
	return doc.Name, true
}

// dsAllocRankingScore registers an UploadScore call as a searchable DataStore
// object. The club-code portion of the tag (groups[0], zero-padded to 3
// digits) is confirmed against real SearchObject calls (e.g. groups=[32,...]
// exactly matches a live "us_032_ave" search) — despite the name, groups[0]
// is NOT a sport code, it's the player's own club/country code (confirmed
// 2026-08-21: Nico's own ranking_scores rows show groups[0] change from 28 to
// 33 exactly when he confirmed his selected club was "GER Hesse", tag 033).
// groups[0]==0xFF (255) is the "no specific club" sentinel — real captures
// show it's used for the club-less "eu_base_rank" search, vs a real club code
// for the per-club "eu_NNN_ave"/"eu_NNN_vs_record" searches. The category→
// stat-type mapping within a family (e.g. which of 2000/2020/2040 is "ave"
// vs "vs_record") isn't confirmed yet, so both real-format tags get applied
// to every score in a per-club batch for now — makes the search actually
// find something rather than being precisely correct about which category
// is which. See handleUploadScore's comment.
func dsAllocRankingScore(ownerPID, category, score uint32, param uint64, groups []byte) *dsObject {
	now := time.Now()
	regionPrefix := dsClubRegionPrefix(ownerPID)
	tags := []string{fmt.Sprintf("category_%d", category)}
	if len(groups) > 0 {
		tags = append(tags, fmt.Sprintf("%s_%03d_category_%d", regionPrefix, groups[0], category))
		if groups[0] == 0xFF {
			tags = append(tags, fmt.Sprintf("%s_base_rank", regionPrefix))
		} else {
			tags = append(tags,
				fmt.Sprintf("%s_%03d_ave", regionPrefix, groups[0]),
				fmt.Sprintf("%s_%03d_vs_record", regionPrefix, groups[0]),
			)
		}
	}
	for _, g := range groups {
		tags = append(tags, fmt.Sprintf("group_%d", g))
	}
	metaBinary := make([]byte, 8)
	binary.LittleEndian.PutUint32(metaBinary[0:], score)
	binary.LittleEndian.PutUint32(metaBinary[4:], uint32(param))

	// Find the existing ranking slot for this owner+category and update it in
	// place rather than inserting a new object every upload - otherwise
	// SearchObject's unordered dsStore.Range can keep returning an older
	// duplicate forever instead of the latest score, which is exactly what
	// made club info look frozen. Matches on category alone (not also
	// groups/club) - confirmed 2026-08-23 that switching clubs still uploads
	// under the same category with different groups, and matching groups too
	// let that slip through and create a second permanent duplicate the exact
	// same way the original bug did (a player has exactly one current record
	// per stat, regardless of which club they're presently representing).
	var existing *dsObject
	dsStore.Range(func(_, v interface{}) bool {
		obj := v.(*dsObject)
		if obj.OwnerPID == ownerPID && obj.RankCategory == category {
			existing = obj
			return false
		}
		return true
	})
	if existing != nil {
		existing.MetaBinary = metaBinary
		existing.Tags = tags
		existing.RankGroups = append([]byte{}, groups...)
		existing.Updated = now
		go dsSave()
		return existing
	}

	id := atomic.AddUint64(&dsNextID, 1) - 1
	obj := &dsObject{
		DataID:       id,
		OwnerPID:     ownerPID,
		DataType:     0xFFFF,
		MetaBinary:   metaBinary,
		Tags:         tags,
		Created:      now,
		Updated:      now,
		RankCategory: category,
		RankGroups:   append([]byte{}, groups...),
	}
	dsStore.Store(id, obj)
	go dsSave()
	return obj
}

func dsMetaInfo(obj *dsObject) *datastore.DataStoreMetaInfo {
	meta := datastore.NewDataStoreMetaInfo()
	meta.DataID = obj.DataID
	meta.OwnerID = obj.OwnerPID
	meta.Size = obj.Size
	meta.DataType = obj.DataType
	meta.Name = obj.Name
	meta.MetaBinary = obj.MetaBinary
	meta.Permission = datastore.NewDataStorePermission()
	meta.DelPermission = datastore.NewDataStorePermission()
	meta.CreatedTime = nex.NewDateTime(nexDateTime(obj.Created))
	meta.UpdatedTime = nex.NewDateTime(nexDateTime(obj.Updated))
	meta.ReferredTime = nex.NewDateTime(0)
	meta.ExpireTime = nex.NewDateTime(0)
	meta.Period = obj.Period
	meta.Flag = obj.Flag
	meta.Tags = obj.Tags
	meta.Ratings = []*datastore.DataStoreRatingInfoWithSlot{}
	return meta
}

func nexDateTime(t time.Time) uint64 {
	// NEX DateTime packs year/month/day/hour/min/sec into a uint64
	return uint64(t.Second()) | uint64(t.Minute())<<6 | uint64(t.Hour())<<12 |
		uint64(t.Day())<<17 | uint64(t.Month())<<22 | uint64(t.Year())<<26
}

const dsStorePath = "datastore.json"

type dsStoreSnapshot struct {
	NextID  uint64      `json:"next_id"`
	Objects []*dsObject `json:"objects"`
}

func dsSave() {
	var objs []*dsObject
	dsStore.Range(func(_, v interface{}) bool {
		objs = append(objs, v.(*dsObject))
		return true
	})
	snap := dsStoreSnapshot{
		NextID:  atomic.LoadUint64(&dsNextID),
		Objects: objs,
	}
	b, err := json.Marshal(snap)
	if err != nil {
		fmt.Println("dsSave marshal:", err)
		return
	}
	if err := os.WriteFile(dsStorePath, b, 0644); err != nil {
		fmt.Println("dsSave write:", err)
	}
}

func dsLoad() {
	b, err := os.ReadFile(dsStorePath)
	if err != nil {
		return
	}
	var snap dsStoreSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		fmt.Println("dsLoad unmarshal:", err)
		return
	}
	for _, obj := range snap.Objects {
		dsStore.Store(obj.DataID, obj)
	}
	if snap.NextID > 1 {
		atomic.StoreUint64(&dsNextID, snap.NextID)
	}
	fmt.Printf("DataStore loaded: %d objects, nextID=%d\n", len(snap.Objects), snap.NextID)
}

// migrateRankingScoreTags backfills the real "eu_base_rank"/"eu_NNN_ave"/
// "eu_NNN_vs_record"-format tags (added to dsAllocRankingScore after the
// fact) onto ranking-score objects stored before that fix, by parsing the
// club code back out of their existing "group_N" debug tags. Without this,
// scores already uploaded tonight would stay permanently unfindable by
// SearchObject until the same player uploads again.
func migrateRankingScoreTags() {
	migrated := 0
	dsStore.Range(func(_, v interface{}) bool {
		obj := v.(*dsObject)
		if obj.DataType != 0xFFFF || len(obj.Tags) == 0 {
			return true
		}
		hasRealTag := false
		var clubCode int = -1
		for _, t := range obj.Tags {
			if strings.HasSuffix(t, "_base_rank") || strings.HasSuffix(t, "_ave") || strings.HasSuffix(t, "_vs_record") {
				hasRealTag = true
			}
			if strings.HasPrefix(t, "group_") {
				if n, err := strconv.Atoi(strings.TrimPrefix(t, "group_")); err == nil && clubCode == -1 {
					clubCode = n
				}
			}
		}
		if hasRealTag || clubCode == -1 {
			return true
		}
		regionPrefix := dsClubRegionPrefix(obj.OwnerPID)
		if clubCode == 0xFF {
			obj.Tags = append(obj.Tags, fmt.Sprintf("%s_base_rank", regionPrefix))
		} else {
			obj.Tags = append(obj.Tags,
				fmt.Sprintf("%s_%03d_ave", regionPrefix, clubCode),
				fmt.Sprintf("%s_%03d_vs_record", regionPrefix, clubCode),
			)
		}
		dsStore.Store(obj.DataID, obj)
		migrated++
		return true
	})
	if migrated > 0 {
		fmt.Printf("migrateRankingScoreTags: backfilled real-format tags on %d object(s)\n", migrated)
		go dsSave()
	}
}

// migrateRankingScoreRegions fixes ranking-score tags computed with the old
// broken region heuristic (see dsClubRegionPrefix's comment) - a score tagged
// with the wrong eu_/us_ prefix is permanently unfindable by the owner's own
// client's SearchObject calls until they upload again, which may never
// happen for a rarely-played sport. Rewrites any mismatched tag in place
// using each object's now-correctly-derived region prefix.
func migrateRankingScoreRegions() {
	fixed := 0
	dsStore.Range(func(_, v interface{}) bool {
		obj := v.(*dsObject)
		if obj.DataType != 0xFFFF || obj.OwnerPID == 0 {
			return true
		}
		correct := dsClubRegionPrefix(obj.OwnerPID)
		wrong := "eu"
		if correct == "eu" {
			wrong = "us"
		}
		changed := false
		for i, t := range obj.Tags {
			if strings.HasPrefix(t, wrong+"_") {
				obj.Tags[i] = correct + "_" + strings.TrimPrefix(t, wrong+"_")
				changed = true
			}
		}
		if changed {
			dsStore.Store(obj.DataID, obj)
			fixed++
		}
		return true
	})
	if fixed > 0 {
		fmt.Printf("migrateRankingScoreRegions: fixed region prefix on %d object(s)\n", fixed)
		go dsSave()
	}
}

// migrateRankingScoreDedup collapses duplicate ranking-score objects created by
// old dsAllocRankingScore calls from before it started updating an existing slot
// in place instead of always inserting a new object (see that function's
// comment - the "club info doesn't update its stats" bug, fixed 2026-08-23:
// every UploadScore piled up another object with the same tags, and
// SearchObject's unordered dsStore.Range could keep returning an older
// duplicate forever instead of the latest score). Reconstructs each object's
// (owner, category) key from its tags, since RankCategory didn't exist on
// objects created before the fix, keeps only the most-recently-updated object
// per key, and deletes the rest. Keyed on category alone, not also groups -
// a player switching clubs still uploads under the same category with
// different groups, and including groups in the key let that duplicate slip
// through this same cleanup (confirmed 2026-08-23 on a real account that had
// switched clubs).
func migrateRankingScoreDedup() {
	type rankKey struct {
		pid      uint32
		category uint32
	}
	best := map[rankKey]*dsObject{}
	var stale []uint64
	dsStore.Range(func(_, v interface{}) bool {
		obj := v.(*dsObject)
		if obj.DataType != 0xFFFF {
			return true
		}
		var category uint32
		var groups []byte
		for _, t := range obj.Tags {
			if n, err := strconv.Atoi(strings.TrimPrefix(t, "category_")); err == nil && strings.HasPrefix(t, "category_") {
				category = uint32(n)
			}
			if n, err := strconv.Atoi(strings.TrimPrefix(t, "group_")); err == nil && strings.HasPrefix(t, "group_") {
				groups = append(groups, byte(n))
			}
		}
		k := rankKey{pid: obj.OwnerPID, category: category}
		cur, ok := best[k]
		if !ok || obj.Updated.After(cur.Updated) {
			if ok {
				stale = append(stale, cur.DataID)
			}
			best[k] = obj
			obj.RankCategory = category
			obj.RankGroups = groups
		} else {
			stale = append(stale, obj.DataID)
		}
		return true
	})
	for _, id := range stale {
		dsStore.Delete(id)
	}
	if len(stale) > 0 {
		fmt.Printf("migrateRankingScoreDedup: removed %d stale duplicate ranking object(s)\n", len(stale))
		go dsSave()
	}
}

// watchRankingScoreRegions re-runs migrateRankingScoreRegions() periodically instead
// of only once at startup. A player's profile object can transiently lose its real
// DataType (2/3) - e.g. a server-side DataStore reset while their console still thinks
// the object exists sends a plain ChangeMeta instead of a fresh create, which recreates
// it with DataType 0 (see project_wsc_ranking_persistence memory). Any scores uploaded
// while that's the case get mistagged. Once the player goes back into the in-game club
// screen and reselects (fixing their DataType), this ticker picks up the correction
// without needing a full service restart.
func watchRankingScoreRegions() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		migrateRankingScoreRegions()
	}
}

// dsSeedProfiles seeds real BPFC player profiles recovered from the old community
// server. Without these, SearchObject for DataType=2/3 returns 0 results and the
// game shows "data could not be obtained" because it cannot display any leaderboard.
// Seeds are only added once (when no type-2 or type-3 objects exist), then persisted.
func dsSeedProfiles() {
	hasUS, hasEU := false, false
	dsStore.Range(func(_, v interface{}) bool {
		obj := v.(*dsObject)
		if obj.DataType == 2 && len(obj.MetaBinary) > 0 {
			hasUS = true
		}
		if obj.DataType == 3 && len(obj.MetaBinary) > 0 {
			hasEU = true
		}
		return !hasUS || !hasEU
	})
	if hasUS && hasEU {
		return
	}

	type seedEntry struct {
		dt  uint16
		hex string
	}
	seeds := []seedEntry{}

	if !hasEU {
		seeds = append(seeds,
			seedEntry{3, "42504643000000010000000000000000000000000001000003000040033040082667D1A2DD71423B3629BF134F6E0000A528450078006F0072006300690073006D0020009201691E00000C041E68441833344614811213660D000029005248506D260000000000000000000000000000000000000000212E000000000000000000000000000000001FA2045F00000000D70A00000000006A000800221E25DC005F2E300000000088000210000015F10000000000C0000000000000000005DC00000000000000000000000000000000000000000000000000000400100005DC00B56AD000B4B4B40000000000000000000000000000000000000000000005DC00000000000000000000000000000000000000000000000000000000000005DC00000000000000000000000000000000000000000000000000"},
			seedEntry{3, "425046430000000100000000000000000000000000010000030100400A86FB85C024F0F0D65DC18435B5A5E2A43C0000862C5700460008E045007800720061007A00650072007F0000B022000268431606232610811017680D0000250752485047006100790050006F0072006E000000000000000000BA1B000000000000000000000000000000001FA9AE4B000000000000000000000001000400100325DC00408000000000001800001000000055000000000080000000000000000005DC00000000000000000000000000000000000000000000000000000000000005DC00B56AD000B4B4B40000000000000000000000000000000000000000000005DC00000000000000000000000000000000000000000000000000000000000005DC00000000000000000000000000000000000000000000000000"},
			seedEntry{3, "42504643000000010000000000000000000000000001000003000040A9B6E3A966E721F2DF02DB51FB3E691993FB0000560376009201C6256D00650065002D00000045042D004040000021010268441820344514461219680D0000290052485076006C00610064000000690075007300000000000000319F000000000000000000000000000000001FA7230F000000000000000000000001000000152C75DC0070B0F000285001C8000128000015860015E28000DC000000000000076145DC0000048FC0005A090000080D5A0034000C0F802000E0000000000000000A55DC00B56AD000B4B4B40000000000124000000000000001000000000000051E25DC00002E000600130032000388A000028000001B0000000B8480000000000005DC00000000000000000000000000000000000000000000000000"},
			seedEntry{3, "42504643000000010000000000000000000000000001000003000010D1CB2CCE8526A3708916351C3D8B85C4C92C000068056D0075006C007400690000000000000000000000614002003901026844182034461A861215680F0000290352485064006100760069006400000000000000000000000000BDEE000000000000000000000000000000001FA93B37000000000000000000000001000000022165DC00552A2174000000A800001000000F720000000000E00000000000000109B5DC000001800000070000000400000002000004C0000000000000000000000055DC00B16AD000B4B4B40000000000000000000000000080000000000000050005DC00002A800E000B001C0005800000038000001C000000048200000000000005DC00000000000000000000000000000000000000000000000000"},
		)
	}
	if !hasUS {
		seeds = append(seeds,
			seedEntry{2, "42504643000000010000000000000000000000000001000003000040E805DAA520C4A0E0DE424E2BCD54BE3BBFED0000751A41007600650072007900000000000000000000007F7F220039000469431830344712811215680D000129B711C34D73006D006100730068005F006800610077006B00000000FB000000000000000000000000000000001FA56002000000000000000000000001000800377985DC008BDDC000834007000016600000A1C900A8C82000DC000000000000022875DC00000500000016800000068000000900000480400000000000000000000005DC00B56AD000B4B4B40000000000000000000000000000000000000000000005DC00000000000000000000000000000000000000000000000000000000000005DC00000000000000000000000000000000000000000000000000"},
			seedEntry{2, "425046430000000100000000000000000000000000010000030100406A86DB25E0C45010DF8EB4CAD995CF3499EA0000AC154D00610067006900630041007300680065007200513C0000280602694418C034461481120E680D000029365248504D006100670069006300410073006800650072000000A092000000000000000000000000000000001FA9ECAA000000000000000000000001000000000005DC00000000000000000000000000000000000000000000000000000000000005DC00000005200000000000000000000000000000000080000000000000000005DC00B56AD000B4B4B40000000000000000000000000000000000000000000005DC00000000000000000000000266000000000000000000000080000000000005DC00000000000000000000000000000000000000000000000000"},
			seedEntry{2, "4250464300000001000000000000000000000000000100000301003088E79B64C144C030879FD0984102266646C80000AA235900540005264A00720042006100670065006C000021820033082C68231813344510AB1006620D0000295152655452006F006D0065006500720000006F006200000000002138000000000000000000000000000000001FA9F2BA0000000000000000000000010000000948C5DC00965DC38C834005B80000E000001F40004D84B000FC0000000000000934E5DC00000691600017AC20000C8AFA0003800404403000E00000000000000100A5DC00B56AD000B4B4B40000000000000000000000000000000000000000062A15DC00002B00140019000A00030000000080000010800000130800000000096AF5DC000FC8000000000500008402B0009E20780074008800000000"},
		)
	}

	now := time.Now()
	for _, s := range seeds {
		b, err := hex.DecodeString(s.hex)
		if err != nil {
			fmt.Println("dsSeedProfiles decode:", err)
			continue
		}
		id := atomic.AddUint64(&dsNextID, 1) - 1
		obj := &dsObject{
			DataID:     id,
			DataType:   s.dt,
			MetaBinary: b,
			Tags:       []string{},
			Flag:       2,
			Period:     365,
			Created:    now,
			Updated:    now,
		}
		dsStore.Store(id, obj)
	}
	fmt.Printf("dsSeedProfiles: added %d profiles (hadUS=%v hadEU=%v)\n", len(seeds), hasUS, hasEU)
	dsSave()
}

// connectedPIDs tracks PIDs with an active PRUDP connection.
// Populated on Connect, cleared on Disconnect — always reflects live state.
var connectedPIDs sync.Map // uint32 → struct{}

// currentClient maps PID → the *nex.Client that last connected.
// Used to detect stale Disconnect events: nex-go v1.0.16 can fire Disconnect twice
// (e.g. two DISC packets, or a reset SYN), and may fire the old Disconnect after a
// fresh Connect has already run. Comparing the client pointer prevents double-cleanup
// and protects the new session's gatherings from being torn down by a stale event.
var currentClient sync.Map // uint32 → *nex.Client

// pidNATm/pidNATf store each player's NAT mapping/filtering type.
// Persisted across reconnects — NAT type is a router property, not a session property.
// Not deleted on Disconnect so that GetSessionURLs can stamp correct values immediately
// on reconnect, before ReportNATProperties fires.
var pidNATm sync.Map // uint32 → uint32
var pidNATf sync.Map // uint32 → uint32

// playerJoinedAt tracks when a player last joined a gathering (as joiner, not host).
// Used to detect quick-exit loops: join → fail → EndParticipation within 30s.
var playerJoinedAt sync.Map // uint32 pid → joinRecord

type joinRecord struct {
	gid  uint32
	when time.Time
}

// gatheringFailCount tracks consecutive quick-exits (< 30s) per gathering.
// After 3 consecutive quick-exits from any joiner, the gathering is closed.
var gatheringFailCount sync.Map // uint32 gid → int

// lastPacketAt tracks, per PID, the last time ANY inbound packet was seen (Data, Ack,
// MultiAck, Ping, Disconnect — every type, same signal client.IncreasePingTimeoutTime
// in vendor/nex-go already resets on). Pure diagnostics, no behavior change: after
// confirming (2026-08-19) that real WSC clients never ack a server-sent PRUDP ping, we
// need real per-session gap data before shrinking SetPingTimeout(3600) with confidence.
// Cleared on Disconnect (main loop below) so a PID's next session starts a fresh
// baseline instead of reporting one bogus multi-hour "gap" across the reconnect.
var lastPacketAt sync.Map // uint32 pid → time.Time

// holePunchingSince tracks PIDs currently mid-NAT-traversal (both the requester and
// every peer it's probing - RequestProbeInitiationExt's target list), so
// watchStaleConnections can exempt them entirely instead of just tolerating a longer
// idle gap. Cleared on ReportNATTraversalResult. The stored time.Time is a safety
// expiry, not a display value: if a client crashes or drops mid-handshake and never
// reports a result, this would otherwise exempt that PID from stale-cleanup forever.
var holePunchingSince sync.Map // uint32 pid → time.Time (when marked)

// holePunchExemptionMax bounds how long a hole-punching exemption can last before
// watchStaleConnections stops trusting it and falls back to normal idle checking -
// covers the case where ReportNATTraversalResult never arrives (client crash,
// dropped connection mid-handshake) so an abandoned session isn't exempted forever.
// Was 60s; confirmed too short on 2026-08-23 - a real natm=1/natf=2 pairing needed
// over 60s (StaleDisconnect fired at idleFor=1m4s/1m50s) and then succeeded almost
// instantly (rtt=165ms) on the very next attempt after being forced to reconnect.
// Raised well past any observed real attempt; a stuck-forever exemption from an
// abandoned handshake is far cheaper than killing one that would've succeeded.
const holePunchExemptionMax = 5 * time.Minute

func markHolePunching(pid uint32) {
	holePunchingSince.Store(pid, time.Now())
}

func clearHolePunching(pid uint32) {
	holePunchingSince.Delete(pid)
}

// isHolePunchExempt reports whether pid is currently within a live (unexpired)
// hole-punching exemption window.
func isHolePunchExempt(pid uint32) bool {
	since, ok := holePunchingSince.Load(pid)
	if !ok {
		return false
	}
	return time.Since(since.(time.Time)) < holePunchExemptionMax
}

// natFailureBlockedUntil holds pids that just failed NAT traversal (typically a
// symmetric NAT/strict firewall that can never hole-punch, not a one-off blip), and
// when their matchmaking block expires. A player behind a NAT that can never
// hole-punch would otherwise keep re-entering matchmaking, pairing with and wasting
// the time of a new real opponent every attempt. Enforced in two places: a blocked
// pid always gets its own solo gathering in handleAutoMatchmakeRaw rather than
// joining a real waiting player, and dbFindGathering skips any candidate gathering
// hosted by a currently-blocked pid - so blocked players are fully isolated from
// real matchmaking in both directions without needing a client-visible rejection.
var natFailureBlockedUntil sync.Map // uint32 pid → time.Time (block expiry)

const natFailureBlockDuration = 5 * time.Minute

func blockFromMatchmaking(pid uint32) {
	natFailureBlockedUntil.Store(pid, time.Now().Add(natFailureBlockDuration))
}

func isBlockedFromMatchmaking(pid uint32) bool {
	until, ok := natFailureBlockedUntil.Load(pid)
	if !ok {
		return false
	}
	if time.Now().After(until.(time.Time)) {
		natFailureBlockedUntil.Delete(pid)
		return false
	}
	return true
}

// stationPIDRe extracts the PID field from a PRUDP station string, e.g.
// "prudp:/address=1.2.3.4;port=1;PID=1037540335;RVCID=11;...".
var stationPIDRe = regexp.MustCompile(`;PID=(\d+)`)

func extractStationPID(station string) (uint32, bool) {
	m := stationPIDRe.FindStringSubmatch(station)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseUint(m[1], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

func packetTypeName(t uint16) string {
	switch t {
	case nex.SynPacket:
		return "Syn"
	case nex.ConnectPacket:
		return "Connect"
	case nex.DataPacket:
		return "Data"
	case nex.DisconnectPacket:
		return "Disconnect"
	case nex.PingPacket:
		return "Ping"
	default:
		return fmt.Sprintf("Type%d", t)
	}
}

func packetFlagsStr(packet *nex.PacketV1) string {
	var flags []string
	if packet.HasFlag(nex.FlagAck) {
		flags = append(flags, "Ack")
	}
	if packet.HasFlag(nex.FlagMultiAck) {
		flags = append(flags, "MultiAck")
	}
	if packet.HasFlag(nex.FlagReliable) {
		flags = append(flags, "Reliable")
	}
	if packet.HasFlag(nex.FlagNeedsAck) {
		flags = append(flags, "NeedsAck")
	}
	if len(flags) == 0 {
		return "-"
	}
	return strings.Join(flags, "+")
}

// logPacketGaps records a PacketGap line whenever a PID goes quiet for a while and then
// sends something — the raw material for figuring out how long a genuinely-connected
// (not just-not-yet-timed-out) WSC client can go silent. Registered on the generic
// "Packet" event, which vendor/nex-go's handleSocketMessage already emits for every
// packet type after updating the same idle timer SetPingTimeout drives.
func logPacketGaps(packet *nex.PacketV1) {
	pid := packet.Sender().PID()
	if pid == 0 {
		return
	}
	now := time.Now()
	prev, hadPrev := lastPacketAt.Load(pid)
	lastPacketAt.Store(pid, now)
	if !hadPrev {
		return
	}
	gap := now.Sub(prev.(time.Time))
	if gap < 5*time.Second {
		return
	}
	fmt.Printf("PacketGap: PID=%d gap=%s type=%s flags=%s\n",
		pid, gap.Round(time.Second), packetTypeName(packet.Type()), packetFlagsStr(packet))
}

// staleIdleThreshold is how long a connected PID can go without ANY inbound packet
// before watchStaleConnections treats it as gone. 15s is 3x the observed real client
// cadence (~5-10s self-initiated Ping, p90=9s, holds even mid-match — see PacketGap
// logs). Validated in shadow mode 2026-08-19 through a full bowling match: the active
// participants held a rock-solid ~5s cadence with zero false flags, while abandoned
// sessions sat idle and climbing with no PacketGap at all.
//
// The matchmaking/NAT hole-punching phase legitimately breaks this cadence (both
// peers go quiet toward the server for 20s+ while probing each other directly -
// confirmed 2026-08-23) but is handled separately via isHolePunchExempt(), not by
// loosening this threshold, so genuinely abandoned non-matchmaking sessions are
// still caught promptly. Loosen this if real-world jitter (bad WiFi, packet loss)
// ever produces false positives outside of hole-punching on genuinely-connected
// players.
const staleIdleThreshold = 15 * time.Second

// watchStaleConnections is the replacement for the vendor ping/kick mechanism, which
// has an unsynchronized data race on pingKickTimer (client.go) that makes short
// SetPingTimeout values unsafe — see feedback_wsc_ping_timeout memory. This checks
// every PID we believe is connected against lastPacketAt on a plain ticker, no
// timer-callback races involved.
//
// On timeout, mirrors the Disconnect handler's cleanup (connectedPIDs/currentClient/
// lastPacketAt deletion, dbLeaveAllGatherings, dbDeleteSession) but deliberately does
// NOT call server.Kick() — no need to touch the live PRUDP session, only our own
// bookkeeping. That asymmetry is the point: a false positive here just means the
// player briefly drops off the dashboard / out of a gathering and self-heals on their
// next packet, rather than actually severing their connection like Kick() would.
func watchStaleConnections() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		connectedPIDs.Range(func(k, _ interface{}) bool {
			pid := k.(uint32)
			last, ok := lastPacketAt.Load(pid)
			if !ok {
				return true
			}
			idleFor := now.Sub(last.(time.Time))
			if idleFor <= staleIdleThreshold {
				return true
			}
			if isHolePunchExempt(pid) {
				return true // mid-NAT-traversal - expected to be quiet toward the server
			}
			// Re-check immediately before acting: a packet may have arrived from
			// this PID between the Range snapshot above and here (packet handling
			// runs concurrently on its own goroutine(s) and updates lastPacketAt
			// independently of this ticker). Without this, a genuinely-connected
			// player whose idle gap lands right at the threshold could have their
			// gathering/session wiped by a decision that was already stale by the
			// time it executed - a real TOCTOU race, not just a loose threshold.
			recheck, ok := lastPacketAt.Load(pid)
			if !ok || !recheck.(time.Time).Equal(last.(time.Time)) {
				return true // a new packet landed - no longer stale, skip this cycle
			}
			fmt.Printf("StaleDisconnect: PID=%d idleFor=%s — cleaning up gatherings and session (PRUDP session itself untouched)\n", pid, idleFor.Round(time.Second))
			currentClient.Delete(pid)
			connectedPIDs.Delete(pid)
			lastPacketAt.Delete(pid)
			dbLeaveAllGatherings(pid)
			dbDeleteSession(pid)
			return true
		})
	}
}

// --- Internal HTTP status endpoint (127.0.0.1:9015) for relay-admin dashboard ---

type wscSessionInfo struct {
	PID  int64 `json:"pid"`
	NATm int64 `json:"natm"`
}

type wscMatchInfo struct {
	GID         int64   `json:"gid"`
	SportType   int64   `json:"sport_type"`
	Host        int64   `json:"host"`
	Players     []int64 `json:"players"`
	PlayerCount int64   `json:"player_count"`
	StartedAt   int64   `json:"started_at"`
}

type wscNatShameInfo struct {
	PID          int64 `json:"pid"`
	BlockedUntil int64 `json:"blocked_until"` // unix seconds
}

type wscGatheringInfo struct {
	GID         int64   `json:"gid"`
	Host        int64   `json:"host"`
	GameMode    int64   `json:"game_mode"`
	SportType   int64   `json:"sport_type"`
	MaxPlayers  int64   `json:"max_players"`
	PlayerCount int64   `json:"player_count"`
	Players     []int64 `json:"players"`
	Open        bool    `json:"open"`
}

func bsonInt(v interface{}) int64 {
	switch x := v.(type) {
	case int32:
		return int64(x)
	case int64:
		return x
	default:
		return 0
	}
}

func startStatusServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()

		var sessions []wscSessionInfo
		connectedPIDs.Range(func(k, _ interface{}) bool {
			s := wscSessionInfo{PID: int64(k.(uint32))}
			if v, ok := pidNATm.Load(k.(uint32)); ok {
				s.NATm = int64(v.(uint32))
			}
			sessions = append(sessions, s)
			return true
		})
		if sessions == nil {
			sessions = []wscSessionInfo{}
		}

		var gatherings []wscGatheringInfo
		if cur, err := gatheringsCol.Find(ctx, bson.D{}); err == nil {
			var docs []bson.M
			cur.All(ctx, &docs)
			for _, d := range docs {
				g := wscGatheringInfo{
					GID:         bsonInt(d["gid"]),
					Host:        bsonInt(d["host"]),
					GameMode:    bsonInt(d["game_mode"]),
					SportType:   bsonInt(d["sport_type"]),
					MaxPlayers:  bsonInt(d["max_players"]),
					PlayerCount: bsonInt(d["player_count"]),
				}
				g.Open, _ = d["open"].(bool)
				if raw, ok := d["players"].(bson.A); ok {
					for _, p := range raw {
						g.Players = append(g.Players, bsonInt(p))
					}
				}
				gatherings = append(gatherings, g)
			}
		}
		if gatherings == nil {
			gatherings = []wscGatheringInfo{}
		}

		var matches []wscMatchInfo
		for _, d := range dbGetRecentMatches() {
			m := wscMatchInfo{
				GID:         bsonInt(d["gid"]),
				SportType:   bsonInt(d["sport_type"]),
				Host:        bsonInt(d["host"]),
				PlayerCount: bsonInt(d["player_count"]),
				StartedAt:   bsonInt(d["started_at"]),
			}
			if raw, ok := d["players"].(bson.A); ok {
				for _, p := range raw {
					m.Players = append(m.Players, bsonInt(p))
				}
			}
			matches = append(matches, m)
		}
		if matches == nil {
			matches = []wscMatchInfo{}
		}

		now := time.Now()
		var natShame []wscNatShameInfo
		natFailureBlockedUntil.Range(func(k, v interface{}) bool {
			until := v.(time.Time)
			if now.After(until) {
				natFailureBlockedUntil.Delete(k)
				return true
			}
			natShame = append(natShame, wscNatShameInfo{PID: int64(k.(uint32)), BlockedUntil: until.Unix()})
			return true
		})
		if natShame == nil {
			natShame = []wscNatShameInfo{}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"sessions":          sessions,
			"gatherings":        gatherings,
			"matches":           matches,
			"nat_hall_of_shame": natShame,
		})
	})
	if err := http.ListenAndServe("127.0.0.1:9015", mux); err != nil {
		fmt.Println("wsc status server:", err)
	}
}

var nexServer *nex.Server

func main() {
	dsLoad()
	migrateRankingScoreTags()
	migrateRankingScoreRegions()
	migrateRankingScoreDedup()
	dsSeedProfiles()
	connectDB()
	loadRankingStoreFromMongo()
	dsReplayRankingScoresFromMongo()
	go startStatusServer()
	go cleanupStaleGatherings()
	go watchStaleConnections()
	go watchRankingScoreRegions()

	nexServer = nex.NewServer()
	nexServer.SetPRUDPVersion(1)
	nexServer.SetPRUDPProtocolMinorVersion(3)
	nexServer.SetDefaultNEXVersion(&nex.NEXVersion{Major: 3, Minor: 4, Patch: 0})
	nexServer.SetMatchMakingProtocolVersion(&nex.NEXVersion{Major: 3, Minor: 4, Patch: 0})
	nexServer.SetKerberosPassword(os.Getenv("KERBEROS_PASSWORD"))
	nexServer.SetAccessKey("4d324052")
	// 3600s: CONFIRMED (2026-08-19) that real WSC clients never answer a server-initiated
	// PRUDP ping — SendPing/Kick was tried at a 10s timeout and kicked every connected
	// player after ~20s of no outbound traffic, live-connected or not. So this timeout
	// is NOT a liveness check here, just a very-loose safety net; do not shorten it
	// without evidence the client actually acks pings. A 3-player 9-hole golf round can
	// take 90 minutes, hence the size. For abrupt disconnects (crash, power-off, dead
	// WiFi) there is currently no fast signal at all — cleanup only happens once this
	// 2-hour timer finally kicks the client. Clean quits (explicit DISC packet) are
	// detected immediately via the Disconnect handler below, independent of this timer.
	nexServer.SetPingTimeout(3600)

	nexServer.On("Connect", func(packet *nex.PacketV1) {
		payload := packet.Payload()
		stream := nex.NewStreamIn(payload, nexServer)

		ticketBytes, _ := stream.ReadBuffer()
		serverKey := nex.DeriveKerberosKey(2, []byte(nexServer.KerberosPassword()))
		ticketCipher := nex.NewKerberosEncryption(serverKey)
		decryptedInternal := ticketCipher.Decrypt(ticketBytes)

		internalStream := nex.NewStreamIn(decryptedInternal, nexServer)
		_ = internalStream.ReadDateTime()
		_ = internalStream.ReadUInt32LE()
		sessionKey := internalStream.ReadBytesNext(int64(nexServer.KerberosKeySize()))

		checkData, _ := stream.ReadBuffer()
		checkCipher := nex.NewKerberosEncryption(sessionKey)
		checkDataDecrypted := checkCipher.Decrypt(checkData)

		checkStream := nex.NewStreamIn(checkDataDecrypted, nexServer)
		userPID := checkStream.ReadUInt32LE()
		_ = checkStream.ReadUInt32LE()
		responseCheck := checkStream.ReadUInt32LE()

		packet.Sender().SetPID(userPID)
		connectedPIDs.Store(userPID, struct{}{})
		currentClient.Store(userPID, packet.Sender())
		dbLeaveAllGatherings(userPID) // clear any stale gatherings from a previous session

		responseValueStream := nex.NewStreamOut(nexServer)
		responseValueStream.WriteUInt32LE(responseCheck + 1)
		responseValueBufferStream := nex.NewStreamOut(nexServer)
		responseValueBufferStream.WriteBuffer(responseValueStream.Bytes())

		nexServer.AcknowledgePacket(packet, responseValueBufferStream.Bytes())

		packet.Sender().UpdateRC4Key(sessionKey)
		packet.Sender().SetSessionKey(sessionKey)

		fmt.Printf("Connect: PID=%d\n", userPID)
	})

	nexServer.On("Disconnect", func(packet *nex.PacketV1) {
		pid := packet.Sender().PID()
		if pid == 0 {
			return
		}
		// Guard against stale disconnects: nex-go can fire Disconnect twice for the same
		// connection, or fire an old Disconnect after a new Connect has already run.
		// Only proceed if this client pointer is still the current one for this PID.
		stored, ok := currentClient.Load(pid)
		if !ok || stored.(*nex.Client) != packet.Sender() {
			fmt.Printf("Disconnect: PID=%d — stale event, skipping cleanup\n", pid)
			return
		}
		currentClient.Delete(pid)
		fmt.Printf("Disconnect: PID=%d — cleaning up gatherings and session\n", pid)
		connectedPIDs.Delete(pid)
		// pidNATm/pidNATf intentionally kept — NAT type is stable across reconnects
		lastPacketAt.Delete(pid) // reset the gap-logging baseline for this PID's next session
		dbLeaveAllGatherings(pid)
		dbDeleteSession(pid)
	})

	nexServer.On("Packet", logPacketGaps)

	nexServer.On("Data", func(packet *nex.PacketV1) {
		request := packet.RMCRequest()
		fmt.Printf("==WSC Secure== proto=%#v method=%#v\n", request.ProtocolID(), request.MethodID())
		// Handle AutoMatchmake manually — the library's parser panics on WSC packets
		// because WSC sends VacantParticipants (uint16) in each MatchmakeSessionSearchCriteria,
		// which the library only reads for MatchMakingProtocolVersion >= 3.5.
		if request.ProtocolID() == matchmake_extension.ProtocolID {
			switch request.MethodID() {
			case matchmake_extension.MethodAutoMatchmakeWithSearchCriteria_Postpone:
				go handleAutoMatchmakeRaw(packet)
			case matchmake_extension.MethodOpenParticipation:
				go handleOpenParticipation(packet)
			case matchmake_extension.MethodCloseParticipation:
				go handleCloseParticipation(packet)
			default:
				// Return empty list for any unimplemented MatchmakeExtension method so
				// the client doesn't hang waiting (e.g. method=0x4 BrowseMatchmakeSession).
				callID := request.CallID()
				methodID := request.MethodID()
				client := packet.Sender()
				go func() {
					fmt.Printf("MatchmakeExtension stub: PID=%d method=0x%x\n", client.PID(), methodID)
					out := nex.NewStreamOut(nexServer)
					out.WriteUInt32LE(0) // empty list
					sendResponse(client, matchmake_extension.ProtocolID, callID, methodID, out.Bytes())
				}()
			}
		}
		// Handle all Ranking methods here to avoid the library returning NotImplemented
		// for UploadScore (0x1) and others that WSC calls during matchmaking.
		if request.ProtocolID() == ranking.ProtocolID {
			switch request.MethodID() {
			case ranking.MethodUploadScore:
				client := packet.Sender()
				callID := request.CallID()
				params := request.Parameters()
				go func() {
					handleUploadScore(client, callID, params)
				}()
			case ranking.MethodUploadCommonData:
				client := packet.Sender()
				callID := request.CallID()
				params := request.Parameters()
				go func() {
					handleUploadCommonData(client, callID, params)
				}()
			case ranking.MethodGetRanking:
				client := packet.Sender()
				callID := request.CallID()
				params := request.Parameters()
				go func() {
					handleGetRanking(client, callID, params)
				}()
			default:
				go sendResponse(packet.Sender(), ranking.ProtocolID, request.CallID(), request.MethodID(), []byte{})
			}
		}

		// Stub out any protocol that isn't handled by a registered protocol object.
		// Without this the nex-go library returns Core::NotImplemented, which causes
		// WSC to show "data could not be obtained" for things like its custom club
		// protocol (0x83) and game-stats protocol (0x15).
		knownProtocols := map[uint8]bool{
			secure_connection.ProtocolID:   true, // 0x0A
			match_making.ProtocolID:        true, // 0x0B
			match_making_ext.ProtocolID:    true, // 0x32
			matchmake_extension.ProtocolID: true, // 0x6D
			nat_traversal.ProtocolID:       true, // 0x03
			utility.ProtocolID:             true, // 0x6E
			datastore.ProtocolID:           true, // 0x73
			ranking.ProtocolID:             true, // 0x70
		}
		protoID := request.ProtocolID()
		if !knownProtocols[protoID] {
			callID := request.CallID()
			methodID := request.MethodID()
			params := request.Parameters()
			client := packet.Sender()
			// Proto 0x77 method 0xc appears to be a US-region DataStore SearchObject variant.
			// Try parsing it as DataStoreSearchParam; if it parses cleanly, run the same
			// search logic so these connections don't silently fail and cause a core timeout.
			if protoID == 0x77 && methodID == datastore.MethodSearchObject {
				go func() {
					fmt.Printf("Proto77 SearchObject: PID=%d params=%x\n", client.PID(), params)
					result := datastore.NewDataStoreSearchResult()
					result.TotalCount = 0
					result.TotalCountType = 1
					result.Result = []*datastore.DataStoreMetaInfo{}
					out := nex.NewStreamOut(nexServer)
					out.WriteStructure(result)
					sendResponse(client, 0x77, callID, datastore.MethodSearchObject, out.Bytes())
				}()
				return
			}
			go func() {
				fmt.Printf("UnknownProto stub: PID=%d proto=0x%x method=0x%x params=%x\n", client.PID(), protoID, methodID, params)
				sendResponse(client, protoID, callID, methodID, []byte{})
			}()
		}
	})

	secureProto := secure_connection.NewSecureConnectionProtocol(nexServer)
	secureProto.Register(register)
	secureProto.ReplaceURL(replaceURL)
	secureProto.TestConnectivity(testConnectivity)
	secureProto.SendReport(sendReport)

	dsProto := datastore.NewDataStoreProtocol(nexServer)
	dsProto.SearchObject(searchObject)
	dsProto.PostMetaBinary(postMetaBinary)
	dsProto.PrepareGetObject(prepareGetObject)
	dsProto.ChangeMeta(changeMeta)
	dsProto.GetMeta(getMeta)
	dsProto.DeleteObject(deleteObject)
	dsProto.CompletePostObject(completePostObject)

	// AutoMatchmake is handled manually in the "Data" event above — WSC sends
	// VacantParticipants per criterion which the library doesn't parse at 3.4.0.
	// Registering the protocol (even without a handler) would cause it to fire its
	// own panic-prone parser, so we skip the protocol registration entirely.

	mmProto := match_making.NewMatchMakingProtocol(nexServer)
	mmProto.GetSessionURLs(getSessionURLs)
	mmProto.UpdateSessionHostV1(updateSessionHostV1)
	mmProto.UnregisterGathering(unregisterGathering)

	mmExtProto2 := match_making_ext.NewMatchMakingExtProtocol(nexServer)
	mmExtProto2.EndParticipation(endParticipation)

	natProto := nat_traversal.NewNATTraversalProtocol(nexServer)
	natProto.RequestProbeInitiationExt(requestProbeInitiationExt)
	natProto.ReportNATProperties(reportNATProperties)
	natProto.ReportNATTraversalResult(reportNATTraversalResult)

	utilProto := utility.NewUtilityProtocol(nexServer)
	utilProto.AcquireNexUniqueID(acquireNexUniqueID)

	nexServer.Listen(":60015")
}

// searchObject77 handles DataStore SearchObject calls on protocol 0x77.
// The US region WSC client uses 0x77 instead of 0x73 for certain sport login flows.
func searchObject77(client *nex.Client, callID uint32, param *datastore.DataStoreSearchParam) {
	ownerFilter := map[uint32]bool{}
	for _, pid := range param.OwnerIds {
		ownerFilter[pid] = true
	}
	filterByOwner := len(ownerFilter) > 0
	filterByType := param.DataType != 0xFFFF

	var metas []*datastore.DataStoreMetaInfo
	dsStore.Range(func(_, v interface{}) bool {
		obj := v.(*dsObject)
		if filterByOwner && !ownerFilter[obj.OwnerPID] {
			return true
		}
		if filterByType && obj.DataType != param.DataType {
			return true
		}
		for _, tag := range param.Tags {
			found := false
			for _, t := range obj.Tags {
				if t == tag {
					found = true
					break
				}
			}
			if !found {
				return true
			}
		}
		metas = append(metas, dsMetaInfo(obj))
		return true
	})

	offset := int(param.ResultRange.Offset)
	length := int(param.ResultRange.Length)
	if offset > len(metas) {
		offset = len(metas)
	}
	metas = metas[offset:]
	if length > 0 && len(metas) > length {
		metas = metas[:length]
	}

	fmt.Printf("Proto77 SearchObject: PID=%d dataType=0x%x tags=%v ownerIds=%v → %d result(s)\n", client.PID(), param.DataType, param.Tags, param.OwnerIds, len(metas))

	result := datastore.NewDataStoreSearchResult()
	result.TotalCount = uint32(len(metas))
	result.TotalCountType = 1
	result.Result = metas

	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteStructure(result)

	sendResponse(client, 0x77, callID, datastore.MethodSearchObject, rmcResponseStream.Bytes())
}

func sendResponse(client *nex.Client, protocolID uint8, callID uint32, methodID uint32, payload []byte) {
	rmcResponse := nex.NewRMCResponse(protocolID, callID)
	rmcResponse.SetSuccess(methodID, payload)
	pkt, _ := nex.NewPacketV1(client, nil)
	pkt.SetVersion(1)
	pkt.SetSource(0xA1)
	pkt.SetDestination(0xAF)
	pkt.SetType(nex.DataPacket)
	pkt.SetPayload(rmcResponse.Bytes())
	pkt.AddFlag(nex.FlagNeedsAck)
	pkt.AddFlag(nex.FlagReliable)
	nexServer.Send(pkt)
}

func register(err error, client *nex.Client, callID uint32, stationUrls []*nex.StationURL) {
	if err != nil {
		fmt.Println("Register error:", err)
		return
	}

	localStation := stationUrls[0]

	connectionID := uint32(nexServer.ConnectionIDCounter().Increment())
	client.SetConnectionID(connectionID)

	// Always stamp PID and RVCID onto the URL — some clients omit them, which breaks P2P.
	localStation.SetPID(strconv.FormatUint(uint64(client.PID()), 10))
	localStation.SetRVCID(strconv.FormatUint(uint64(connectionID), 10))

	localStationURL := localStation.EncodeToString()
	client.SetLocalStationURL(localStationURL)

	address := client.Address().IP.String()
	port := strconv.Itoa(client.Address().Port)

	localStation.SetAddress(address)
	localStation.SetPort(port)
	localStation.SetNatf("0")
	localStation.SetNatm("0")
	localStation.SetType("3")

	globalStationURL := localStation.EncodeToString()

	dbUpsertSession(client.PID(), []string{localStationURL, globalStationURL}, address, port)

	fmt.Printf("Register: PID=%d addr=%s:%s connID=%d\n", client.PID(), address, port, connectionID)

	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteUInt32LE(0x10001)
	rmcResponseStream.WriteUInt32LE(connectionID)
	rmcResponseStream.WriteString(globalStationURL)

	sendResponse(client, secure_connection.ProtocolID, callID, secure_connection.MethodRegister, rmcResponseStream.Bytes())
}

func replaceURL(err error, client *nex.Client, callID uint32, oldStation *nex.StationURL, newStation *nex.StationURL) {
	if err != nil {
		fmt.Println("ReplaceURL error:", err)
		return
	}

	address := client.Address().IP.String()
	port := strconv.Itoa(client.Address().Port)

	newStation.SetAddress(address)
	newStation.SetPort(port)
	newStation.SetNatf("0")
	newStation.SetNatm("0")
	newStation.SetType("3")

	newURL := newStation.EncodeToString()
	dbUpdateSessionURL(client.PID(), oldStation.EncodeToString(), newURL)
	client.SetLocalStationURL(newURL)

	fmt.Printf("ReplaceURL: PID=%d addr=%s:%s\n", client.PID(), address, port)

	sendResponse(client, secure_connection.ProtocolID, callID, secure_connection.MethodReplaceURL, []byte{})
}

func searchObject(err error, client *nex.Client, callID uint32, param *datastore.DataStoreSearchParam) {
	if err != nil {
		fmt.Println("SearchObject error:", err)
		return
	}

	ownerFilter := map[uint32]bool{}
	for _, pid := range param.OwnerIds {
		ownerFilter[pid] = true
	}
	filterByOwner := len(ownerFilter) > 0
	filterByType := param.DataType != 0xFFFF

	var metas []*datastore.DataStoreMetaInfo
	dsStore.Range(func(_, v interface{}) bool {
		obj := v.(*dsObject)
		if filterByOwner && !ownerFilter[obj.OwnerPID] {
			return true
		}
		if filterByType && obj.DataType != param.DataType {
			return true
		}
		for _, tag := range param.Tags {
			found := false
			for _, t := range obj.Tags {
				if t == tag {
					found = true
					break
				}
			}
			if !found {
				return true
			}
		}
		metas = append(metas, dsMetaInfo(obj))
		return true
	})

	offset := int(param.ResultRange.Offset)
	length := int(param.ResultRange.Length)
	if offset > len(metas) {
		offset = len(metas)
	}
	metas = metas[offset:]
	if length > 0 && len(metas) > length {
		metas = metas[:length]
	}

	fmt.Printf("SearchObject: PID=%d dataType=0x%x tags=%v ownerIds=%v → %d result(s)\n", client.PID(), param.DataType, param.Tags, param.OwnerIds, len(metas))

	result := datastore.NewDataStoreSearchResult()
	result.TotalCount = uint32(len(metas))
	result.TotalCountType = 1
	result.Result = metas

	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteStructure(result)

	sendResponse(client, datastore.ProtocolID, callID, datastore.MethodSearchObject, rmcResponseStream.Bytes())
}

func postMetaBinary(err error, client *nex.Client, callID uint32, param *datastore.DataStorePreparePostParam) {
	if err != nil {
		return
	}
	obj := dsAlloc(client.PID(), param)
	fmt.Printf("PostMetaBinary: PID=%d dataType=0x%x tags=%v → DataID=%d\n", client.PID(), param.DataType, param.Tags, obj.DataID)
	info := datastore.NewDataStoreReqPostInfoV1()
	info.DataID = uint32(obj.DataID)
	info.Url = ""
	info.RequestHeaders = []*datastore.DataStoreKeyValue{}
	info.FormFields = []*datastore.DataStoreKeyValue{}
	info.RootCaCert = []byte{}
	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteStructure(info)
	sendResponse(client, datastore.ProtocolID, callID, datastore.MethodPostMetaBinary, rmcResponseStream.Bytes())
}

// handleUploadScore parses a real Ranking::UploadScore call (RankingScoreData,
// wire format per NintendoClients' nintendo.nex.ranking — no structure-header
// wrapper, matching this server's nex.struct_header=False setup):
//
//	category:    u32 LE
//	score:       u32 LE
//	order:       u8
//	update_mode: u8
//	groups:      u32 LE count, then N x u8
//	param:       u64 LE
//	unique_id:   u64 LE  (top-level field after the structure)
//
// Persists it to Mongo (ranking_scores) and also registers it as a DataStore
// object so live SearchObject calls can find it. The tag used for that is a
// first-pass guess (category-derived) — real WSC search calls use tags like
// "eu_base_rank"/"eu_033_ave" that we haven't reverse-engineered the mapping
// for yet. Once real category/groups values are captured from actual
// gameplay, correlate them against the SearchObject tags requested right
// after and fix the mapping here.
func handleUploadScore(client *nex.Client, callID uint32, params []byte) {
	pid := client.PID()
	if len(params) < 18 {
		fmt.Printf("UploadScore: PID=%d params too short (%d bytes), acking anyway\n", pid, len(params))
		sendResponse(client, ranking.ProtocolID, callID, ranking.MethodUploadScore, []byte{})
		return
	}
	off := 0
	category := binary.LittleEndian.Uint32(params[off:])
	off += 4
	score := binary.LittleEndian.Uint32(params[off:])
	off += 4
	order := params[off]
	off++
	updateMode := params[off]
	off++
	groupCount := binary.LittleEndian.Uint32(params[off:])
	off += 4
	if off+int(groupCount) > len(params) {
		fmt.Printf("UploadScore: PID=%d malformed groups (count=%d), acking anyway\n", pid, groupCount)
		sendResponse(client, ranking.ProtocolID, callID, ranking.MethodUploadScore, []byte{})
		return
	}
	groups := append([]byte{}, params[off:off+int(groupCount)]...)
	off += int(groupCount)
	if off+16 > len(params) {
		fmt.Printf("UploadScore: PID=%d missing param/unique_id, acking anyway\n", pid)
		sendResponse(client, ranking.ProtocolID, callID, ranking.MethodUploadScore, []byte{})
		return
	}
	param := binary.LittleEndian.Uint64(params[off:])
	off += 8
	uniqueID := binary.LittleEndian.Uint64(params[off:])

	fmt.Printf("UploadScore: PID=%d category=%d score=%d order=%d updateMode=%d groups=%v param=%d uniqueID=%d\n",
		pid, category, score, order, updateMode, groups, param, uniqueID)

	dbInsertRankingScore(pid, category, score, order, updateMode, groups, param, uniqueID)

	obj := dsAllocRankingScore(pid, category, score, param, groups)
	if len(groups) > 0 && groups[0] != 0xFF {
		if name, ok := resolveClubName(dsClubRegionPrefix(pid), uint32(groups[0])); ok {
			fmt.Printf("UploadScore: PID=%d stored as DataStore DataID=%d tags=%v (club=%q)\n", pid, obj.DataID, obj.Tags, name)
		} else {
			fmt.Printf("UploadScore: PID=%d stored as DataStore DataID=%d tags=%v\n", pid, obj.DataID, obj.Tags)
		}
	} else {
		fmt.Printf("UploadScore: PID=%d stored as DataStore DataID=%d tags=%v\n", pid, obj.DataID, obj.Tags)
	}

	rankingAdd(pid, category, score, groups, param, uniqueID)

	sendResponse(client, ranking.ProtocolID, callID, ranking.MethodUploadScore, []byte{})
}

// handleUploadCommonData parses a real Ranking::UploadCommonData call:
// commonData is a length-prefixed buffer (u32 LE size + bytes), followed by
// a u64 LE unique_id.
func handleUploadCommonData(client *nex.Client, callID uint32, params []byte) {
	pid := client.PID()
	if len(params) < 4 {
		fmt.Printf("UploadCommonData: PID=%d params too short, acking anyway\n", pid)
		sendResponse(client, ranking.ProtocolID, callID, ranking.MethodUploadCommonData, []byte{})
		return
	}
	size := binary.LittleEndian.Uint32(params[0:])
	if 4+int(size)+8 > len(params) {
		fmt.Printf("UploadCommonData: PID=%d malformed buffer (size=%d), acking anyway\n", pid, size)
		sendResponse(client, ranking.ProtocolID, callID, ranking.MethodUploadCommonData, []byte{})
		return
	}
	commonData := append([]byte{}, params[4:4+size]...)
	uniqueID := binary.LittleEndian.Uint64(params[4+size:])

	fmt.Printf("UploadCommonData: PID=%d size=%d uniqueID=%d data=%x\n", pid, size, uniqueID, commonData)

	dbInsertRankingCommonData(pid, commonData, uniqueID)

	sendResponse(client, ranking.ProtocolID, callID, ranking.MethodUploadCommonData, []byte{})
}

// --- In-memory leaderboard (Ranking::GetRanking) ---
//
// Separate from dsStore/dsObject (which exists for DataStore::SearchObject —
// the club-stats unlock, see dsAllocRankingScore) because GetRanking needs a
// clean, directly-queryable/sortable record per pid+category+groups, not
// something reconstructed by re-parsing debug tags. Backed by Mongo
// (ranking_scores, via dbInsertRankingScore) as the durable copy; this is
// just the fast in-memory view rebuilt from it at startup.

type rankingEntry struct {
	OwnerPID uint32
	UniqueID uint64
	Category uint32
	Score    uint32
	Groups   []byte
	Param    uint64
	Updated  time.Time
}

var (
	rankingMu    sync.Mutex
	rankingStore []rankingEntry
)

// Matches on (pid, category) alone, not also groups - handleGetRanking's own
// leaderboard query never filters by groups either, so a player switching
// clubs previously ended up with two entries for the same category and would
// show up twice in the same leaderboard (confirmed 2026-08-23, same root
// cause as dsAllocRankingScore's identical bug).
func rankingAdd(pid uint32, category, score uint32, groups []byte, param, uniqueID uint64) {
	rankingMu.Lock()
	defer rankingMu.Unlock()
	for i := range rankingStore {
		e := &rankingStore[i]
		if e.OwnerPID == pid && e.Category == category {
			e.Score = score
			e.Groups = append([]byte{}, groups...)
			e.Param = param
			e.UniqueID = uniqueID
			e.Updated = time.Now()
			return
		}
	}
	rankingStore = append(rankingStore, rankingEntry{
		OwnerPID: pid,
		UniqueID: uniqueID,
		Category: category,
		Score:    score,
		Groups:   append([]byte{}, groups...),
		Param:    param,
		Updated:  time.Now(),
	})
}

// loadRankingStoreFromMongo rebuilds rankingStore from the durable copy in
// ranking_scores at startup (mirrors dsLoad()'s role for dsStore).
func loadRankingStoreFromMongo() {
	// Sorted ascending by creation time - rankingAdd now overwrites the existing
	// (pid, category) entry on replay rather than keying on groups too, so
	// replay order determines which score "wins"; this guarantees it's always
	// the most recent one, not whatever order Mongo's cursor happens to return.
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})
	cursor, err := rankingScoresCol.Find(context.Background(), bson.D{}, opts)
	if err != nil {
		fmt.Println("loadRankingStoreFromMongo:", err)
		return
	}
	var docs []bson.M
	if err := cursor.All(context.Background(), &docs); err != nil {
		fmt.Println("loadRankingStoreFromMongo:", err)
		return
	}
	for _, d := range docs {
		pid := uint32(bsonInt(d["pid"]))
		category := uint32(bsonInt(d["category"]))
		score := uint32(bsonInt(d["score"]))
		param := uint64(bsonInt(d["param"]))
		uniqueID := uint64(bsonInt(d["unique_id"]))
		groupsRaw, _ := d["groups"].(bson.A)
		groups := make([]byte, len(groupsRaw))
		for i, g := range groupsRaw {
			groups[i] = byte(bsonInt(g))
		}
		rankingAdd(pid, category, score, groups, param, uniqueID)
	}
	fmt.Printf("loadRankingStoreFromMongo: loaded %d ranking entries\n", len(rankingStore))
}

// dsReplayRankingScoresFromMongo re-registers ranking scores from the durable
// ranking_scores collection as searchable DataStore objects (dsAllocRankingScore),
// but only if the DataStore appears to have no ranking-score objects at all yet -
// i.e. only after a wipe/loss of datastore.json, not on a normal restart where
// dsLoad() already restored everything from the snapshot file. Without this, a
// DataStore reset permanently loses SearchObject-findability for any score that
// isn't freshly re-uploaded by the client, even though the score itself survives
// safely in Mongo. This is exactly what caused a live "eu_base_rank" search to
// return 0 results after the 2026-08-21 full datastore.json reset, despite that
// base_rank score still being present in ranking_scores the whole time.
func dsReplayRankingScoresFromMongo() {
	hasAny := false
	dsStore.Range(func(_, v interface{}) bool {
		if v.(*dsObject).DataType == 0xFFFF {
			hasAny = true
			return false
		}
		return true
	})
	if hasAny {
		return
	}

	cursor, err := rankingScoresCol.Find(context.Background(), bson.D{})
	if err != nil {
		fmt.Println("dsReplayRankingScoresFromMongo:", err)
		return
	}
	var docs []bson.M
	if err := cursor.All(context.Background(), &docs); err != nil {
		fmt.Println("dsReplayRankingScoresFromMongo:", err)
		return
	}
	for _, d := range docs {
		pid := uint32(bsonInt(d["pid"]))
		category := uint32(bsonInt(d["category"]))
		score := uint32(bsonInt(d["score"]))
		param := uint64(bsonInt(d["param"]))
		groupsRaw, _ := d["groups"].(bson.A)
		groups := make([]byte, len(groupsRaw))
		for i, g := range groupsRaw {
			groups[i] = byte(bsonInt(g))
		}
		dsAllocRankingScore(pid, category, score, param, groups)
	}
	fmt.Printf("dsReplayRankingScoresFromMongo: re-registered %d ranking score(s) as DataStore objects\n", len(docs))
	go dsSave()
}

// handleGetRanking parses a real Ranking::GetRanking call and returns an
// actual RankingResult built from rankingStore, instead of the generic
// empty-ack every other unhandled Ranking method still falls through to.
// Wire format per NintendoClients' nintendo.nex.ranking (no structure-header
// wrapper): mode u8, category u32 LE, then RankingOrderParam (order_calc u8,
// group_index u8, group_num u8, time_scope u8, offset u32 LE, count u8),
// then unique_id u64 LE, pid u32 LE.
//
// group_index/group_num filtering and time_scope aren't applied yet (V1) —
// results are filtered by exact category match only, which is enough for
// the common case (group_index defaults to 255 = "no group filter" on the
// client). Response omits update_time (only present for nex.version>=40000;
// WSC runs an older version) and common_data (not tracked per-entry here —
// UploadCommonData's blob is stored separately, unassociated with a specific
// ranking entry).
func handleGetRanking(client *nex.Client, callID uint32, params []byte) {
	pid := client.PID()
	if len(params) < 26 {
		fmt.Printf("GetRanking: PID=%d params too short (%d bytes), acking empty\n", pid, len(params))
		sendResponse(client, ranking.ProtocolID, callID, ranking.MethodGetRanking, []byte{})
		return
	}
	off := 0
	mode := params[off]
	off++
	category := binary.LittleEndian.Uint32(params[off:])
	off += 4
	off++ // order_calc — unused in V1
	off++ // group_index — unused in V1
	off++ // group_num — unused in V1
	off++ // time_scope — unused in V1
	orderOffset := binary.LittleEndian.Uint32(params[off:])
	off += 4
	orderCount := params[off]
	off++
	off += 8 // unique_id — unused in V1
	reqPID := binary.LittleEndian.Uint32(params[off:])

	rankingMu.Lock()
	var matches []rankingEntry
	for _, e := range rankingStore {
		if e.Category != category {
			continue
		}
		if mode == ranking_ModeSelf && e.OwnerPID != pid && e.OwnerPID != reqPID {
			continue
		}
		matches = append(matches, e)
	}
	rankingMu.Unlock()

	sort.Slice(matches, func(i, j int) bool { return matches[i].Score > matches[j].Score })

	start := int(orderOffset)
	if start > len(matches) {
		start = len(matches)
	}
	end := len(matches)
	if orderCount > 0 && start+int(orderCount) < end {
		end = start + int(orderCount)
	}
	page := matches[start:end]

	fmt.Printf("GetRanking: PID=%d mode=%d category=%d -> %d/%d result(s)\n", pid, mode, category, len(page), len(matches))

	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, uint32(len(page)))
	for i, e := range page {
		binary.Write(buf, binary.LittleEndian, e.OwnerPID)
		binary.Write(buf, binary.LittleEndian, e.UniqueID)
		binary.Write(buf, binary.LittleEndian, uint32(start+i+1)) // rank, 1-based
		binary.Write(buf, binary.LittleEndian, e.Category)
		binary.Write(buf, binary.LittleEndian, e.Score)
		binary.Write(buf, binary.LittleEndian, uint32(len(e.Groups)))
		buf.Write(e.Groups)
		binary.Write(buf, binary.LittleEndian, e.Param)
		binary.Write(buf, binary.LittleEndian, uint32(0)) // common_data length
	}
	binary.Write(buf, binary.LittleEndian, uint32(len(matches))) // total
	binary.Write(buf, binary.LittleEndian, nexDateTime(time.Now()))

	sendResponse(client, ranking.ProtocolID, callID, ranking.MethodGetRanking, buf.Bytes())
}

// ranking_ModeSelf mirrors RankingMode.SELF from NintendoClients (not
// exported by the vendored Go ranking package, which only defines method
// IDs, not the mode enum).
const ranking_ModeSelf = 4

func prepareGetObject(err error, client *nex.Client, callID uint32, param *datastore.DataStorePrepareGetParam) {
	if err != nil {
		return
	}
	fmt.Printf("PrepareGetObject: PID=%d\n", client.PID())
	info := datastore.NewDataStoreReqGetInfoV1()
	info.Url = ""
	info.RequestHeaders = []*datastore.DataStoreKeyValue{}
	info.Size = 0
	info.RootCaCert = []byte{}
	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteStructure(info)
	sendResponse(client, datastore.ProtocolID, callID, datastore.MethodPrepareGetObject, rmcResponseStream.Bytes())
}

func changeMeta(err error, client *nex.Client, callID uint32, param *datastore.DataStoreChangeMetaParam) {
	if err != nil {
		return
	}
	if v, ok := dsStore.Load(param.DataID); ok {
		obj := v.(*dsObject)
		if len(param.MetaBinary) > 0 {
			obj.MetaBinary = param.MetaBinary
		}
		if len(param.Tags) > 0 {
			obj.Tags = param.Tags
		}
		if param.Name != "" {
			obj.Name = param.Name
		}
		obj.Updated = time.Now()
		dsStore.Store(param.DataID, obj)
		fmt.Printf("ChangeMeta: PID=%d DataID=%d updated\n", client.PID(), param.DataID)
		go dsSave()
	} else {
		// Object doesn't exist — create it from the change params
		id := param.DataID
		now := time.Now()
		obj := &dsObject{
			DataID:     id,
			OwnerPID:   client.PID(),
			DataType:   param.DataType,
			MetaBinary: param.MetaBinary,
			Tags:       param.Tags,
			Name:       param.Name,
			Period:     param.Period,
			Created:    now,
			Updated:    now,
		}
		if id >= dsNextID {
			atomic.StoreUint64(&dsNextID, id+1)
		}
		dsStore.Store(id, obj)
		fmt.Printf("ChangeMeta: PID=%d DataID=%d dataType=0x%x tags=%v created\n", client.PID(), param.DataID, param.DataType, param.Tags)
		go dsSave()
	}
	sendResponse(client, datastore.ProtocolID, callID, datastore.MethodChangeMeta, []byte{})
}

func getMeta(err error, client *nex.Client, callID uint32, param *datastore.DataStoreGetMetaParam) {
	if err != nil {
		return
	}
	var meta *datastore.DataStoreMetaInfo
	if v, ok := dsStore.Load(param.DataID); ok {
		meta = dsMetaInfo(v.(*dsObject))
		fmt.Printf("GetMeta: PID=%d DataID=%d found\n", client.PID(), param.DataID)
	} else {
		meta = datastore.NewDataStoreMetaInfo()
		meta.Permission = datastore.NewDataStorePermission()
		meta.DelPermission = datastore.NewDataStorePermission()
		meta.CreatedTime = nex.NewDateTime(0)
		meta.UpdatedTime = nex.NewDateTime(0)
		meta.ReferredTime = nex.NewDateTime(0)
		meta.ExpireTime = nex.NewDateTime(0)
		meta.Tags = []string{}
		meta.Ratings = []*datastore.DataStoreRatingInfoWithSlot{}
		fmt.Printf("GetMeta: PID=%d DataID=%d not found\n", client.PID(), param.DataID)
	}
	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteStructure(meta)
	sendResponse(client, datastore.ProtocolID, callID, datastore.MethodGetMeta, rmcResponseStream.Bytes())
}

func deleteObject(err error, client *nex.Client, callID uint32, param *datastore.DataStoreDeleteParam) {
	if err != nil {
		return
	}
	dsStore.Delete(param.DataID)
	fmt.Printf("DeleteObject: PID=%d DataID=%d\n", client.PID(), param.DataID)
	go dsSave()
	sendResponse(client, datastore.ProtocolID, callID, datastore.MethodDeleteObject, []byte{})
}

func completePostObject(err error, client *nex.Client, callID uint32, param *datastore.DataStoreCompletePostParam) {
	if err != nil {
		return
	}
	fmt.Printf("CompletePostObject: PID=%d\n", client.PID())
	sendResponse(client, datastore.ProtocolID, callID, datastore.MethodCompletePostObject, []byte{})
}

func getSessionURLs(err error, client *nex.Client, callID uint32, gatheringID uint32) {
	if err != nil {
		fmt.Println("GetSessionURLs error:", err)
		return
	}

	hostPID := dbGetGatheringHost(gatheringID)
	urls := dbGetPlayerURLs(hostPID)
	if urls == nil {
		urls = []string{}
	}

	// Stamp the host's known natm/natf onto the external (type=3) URL on the fly.
	// The DB may still have natm=0 if ReportNATProperties hasn't fired yet on this session
	// (e.g. immediately after a reconnect). Using the persisted pidNATm/pidNATf values
	// ensures joiners always get accurate NAT info for hole-punching.
	if natm, ok := pidNATm.Load(hostPID); ok {
		natf := uint32(0)
		if v, ok2 := pidNATf.Load(hostPID); ok2 {
			natf = v.(uint32)
		}
		natmStr := strconv.FormatUint(uint64(natm.(uint32)), 10)
		natfStr := strconv.FormatUint(uint64(natf), 10)
		for i, urlStr := range urls {
			u := nex.NewStationURL(urlStr)
			if u.Type() == "3" {
				u.SetNatm(natmStr)
				u.SetNatf(natfStr)
				urls[i] = u.EncodeToString()
			}
		}
	}

	fmt.Printf("GetSessionURLs: gid=%d hostPID=%d urls=%v\n", gatheringID, hostPID, urls)

	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteListString(urls)

	sendResponse(client, match_making.ProtocolID, callID, match_making.MethodGetSessionURLs, rmcResponseStream.Bytes())
}

func updateSessionHostV1(err error, client *nex.Client, callID uint32, gid uint32) {
	if err != nil {
		fmt.Println("UpdateSessionHostV1 error:", err)
		return
	}

	dbUpdateGatheringHost(gid, client.PID())
	fmt.Printf("UpdateSessionHostV1: gid=%d newHost=%d\n", gid, client.PID())

	sendResponse(client, match_making.ProtocolID, callID, match_making.MethodUpdateSessionHostV1, nil)
}

func unregisterGathering(err error, client *nex.Client, callID uint32, gid uint32) {
	if err != nil {
		return
	}
	dbLeaveGathering(gid, client.PID())
	fmt.Printf("UnregisterGathering: PID=%d gid=%d\n", client.PID(), gid)
	sendResponse(client, match_making.ProtocolID, callID, match_making.MethodUnregisterGathering, []byte{0x1})
}

func endParticipation(err error, client *nex.Client, callID uint32, gid uint32, message string) {
	if err != nil {
		fmt.Println("EndParticipation error:", err)
		return
	}

	dbLeaveGathering(gid, client.PID())
	fmt.Printf("EndParticipation: PID=%d gid=%d\n", client.PID(), gid)

	// Detect quick-exit loop: if this player joined as a joiner (not host) and left within
	// 30s, count it as a NAT failure against this gathering. After 3 failures, close the
	// gathering so it doesn't trap new joiners with an unreachable host.
	if rec, ok := playerJoinedAt.LoadAndDelete(client.PID()); ok {
		jr := rec.(joinRecord)
		if jr.gid == gid && time.Since(jr.when) < 30*time.Second {
			n := 0
			if v, loaded := gatheringFailCount.Load(gid); loaded {
				n = v.(int)
			}
			n++
			if n >= 3 {
				fmt.Printf("EndParticipation: gid=%d had %d consecutive quick-exits, closing\n", gid, n)
				dbCloseGathering(gid)
				gatheringFailCount.Delete(gid)
			} else {
				gatheringFailCount.Store(gid, n)
			}
		}
	}

	sendResponse(client, match_making_ext.ProtocolID, callID, match_making_ext.MethodEndParticipation, []byte{0x1})
}

func requestProbeInitiationExt(err error, client *nex.Client, callID uint32, targetList []string, stationToProbe string) {
	if err != nil {
		fmt.Println("RequestProbeInitiationExt error:", err)
		return
	}

	fmt.Printf("RequestProbeInitiationExt: PID=%d targets=%v probe=%s\n", client.PID(), targetList, stationToProbe)

	// Exempt this client and every peer it's probing from stale-disconnect cleanup
	// until ReportNATTraversalResult clears it (or holePunchExemptionMax expires) -
	// both sides naturally go quiet toward the server during hole-punching, which
	// previously got misread as a disconnect and wiped the gathering mid-handshake.
	markHolePunching(client.PID())
	for _, target := range targetList {
		if pid, ok := extractStationPID(target); ok {
			markHolePunching(pid)
		}
	}
	if pid, ok := extractStationPID(stationToProbe); ok {
		markHolePunching(pid)
	}

	sendResponse(client, nat_traversal.ProtocolID, callID, nat_traversal.MethodRequestProbeInitiationExt, nil)

	// Forward InitiateProbe to each target
	rmcMessage := nex.RMCRequest{}
	rmcMessage.SetProtocolID(nat_traversal.ProtocolID)
	rmcMessage.SetCallID(0xffff0000 + callID)
	rmcMessage.SetMethodID(nat_traversal.MethodInitiateProbe)
	probeStream := nex.NewStreamOut(nexServer)
	probeStream.WriteString(stationToProbe)
	rmcMessage.SetParameters(probeStream.Bytes())

	for _, target := range targetList {
		targetURL := nex.NewStationURL(target)
		rvcID, _ := strconv.Atoi(targetURL.RVCID())
		targetClient := nexServer.FindClientFromConnectionID(uint32(rvcID))
		if targetClient != nil {
			msgPkt, _ := nex.NewPacketV1(targetClient, nil)
			msgPkt.SetVersion(1)
			msgPkt.SetSource(0xA1)
			msgPkt.SetDestination(0xAF)
			msgPkt.SetType(nex.DataPacket)
			msgPkt.SetPayload(rmcMessage.Bytes())
			msgPkt.AddFlag(nex.FlagNeedsAck)
			msgPkt.AddFlag(nex.FlagReliable)
			nexServer.Send(msgPkt)
		}
	}
}

// handleAutoMatchmakeRaw manually parses AutoMatchmakeWithSearchCriteria_Postpone.
// The library's handler panics because it doesn't read the VacantParticipants uint16
// that WSC appends to every MatchmakeSessionSearchCriteria (normally a >= 3.5 field).
func handleAutoMatchmakeRaw(packet *nex.PacketV1) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("AutoMatchmake panic (bug): %v\n", r)
		}
	}()

	client := packet.Sender()
	request := packet.RMCRequest()
	callID := request.CallID()
	params := request.Parameters()
	stream := nex.NewStreamIn(params, nexServer)

	var gameMode uint32
	var maxPlayers uint32 = 2

	criteriaCount := int(stream.ReadUInt32LE())
	for ci := 0; ci < criteriaCount; ci++ {
		// attribs (list of NEX strings)
		attribCount := int(stream.ReadUInt32LE())
		for j := 0; j < attribCount; j++ {
			length := stream.ReadUInt16LE()
			stream.ReadBytesNext(int64(length))
		}
		// GameMode string
		gmLen := stream.ReadUInt16LE()
		gmBytes := stream.ReadBytesNext(int64(gmLen))
		if ci == 0 {
			gmStr := string(gmBytes[:len(gmBytes)-1]) // strip null
			gm64, _ := strconv.ParseUint(gmStr, 10, 32)
			gameMode = uint32(gm64)
		}
		// MinParticipants string
		l := stream.ReadUInt16LE()
		stream.ReadBytesNext(int64(l))
		// MaxParticipants string
		mpLen := stream.ReadUInt16LE()
		mpBytes := stream.ReadBytesNext(int64(mpLen))
		if ci == 0 {
			mpStr := string(mpBytes[:len(mpBytes)-1])
			mp64, _ := strconv.ParseUint(mpStr, 10, 32)
			if mp64 > 0 {
				maxPlayers = uint32(mp64)
			}
		}
		// MatchmakeSystemType string
		l = stream.ReadUInt16LE()
		stream.ReadBytesNext(int64(l))
		// VacantOnly, ExcludeLocked, ExcludeNonHostPid bools
		stream.ReadBool()
		stream.ReadBool()
		stream.ReadBool()
		// SelectionMethod uint32
		stream.ReadUInt32LE()
		// VacantParticipants uint16 — WSC always includes this
		stream.ReadUInt16LE()
	}

	var natm uint32
	if v, ok := pidNATm.Load(client.PID()); ok {
		natm = v.(uint32)
	}
	fmt.Printf("AutoMatchmakeRaw: PID=%d gameMode=%d (sport=0x%02x) maxPlayers=%d natm=%d\n", client.PID(), gameMode, gameMode>>24, maxPlayers, natm)

	// A player who just failed NAT traversal always gets a fresh solo gathering
	// instead of searching for a real one to join - see natFailureBlockedUntil's
	// doc comment. dbFindGathering separately skips gatherings hosted BY a blocked
	// pid, so this covers both directions.
	var gid uint32
	if !isBlockedFromMatchmaking(client.PID()) {
		gid = dbFindGathering(gameMode, natm)
	}
	if gid == 0 {
		gid = dbNewGathering(client.PID(), gameMode, maxPlayers, natm)
		fmt.Printf("AutoMatchmake: PID=%d created gathering gid=%d gameMode=%d (sport=0x%02x) natm=%d\n", client.PID(), gid, gameMode, gameMode>>24, natm)
	} else {
		dbJoinGathering(gid, client.PID())
		fmt.Printf("AutoMatchmake: PID=%d joined gathering gid=%d gameMode=%d (sport=0x%02x)\n", client.PID(), gid, gameMode, gameMode>>24)
		playerJoinedAt.Store(client.PID(), joinRecord{gid: gid, when: time.Now()})
	}

	hostPID := dbGetGatheringHost(gid)

	session := match_making.NewMatchmakeSession()
	session.GameMode = gameMode
	session.Gathering.ID = gid
	session.Gathering.OwnerPID = hostPID
	session.Gathering.HostPID = hostPID
	session.Gathering.MinimumParticipants = 1
	session.Gathering.MaximumParticipants = uint16(maxPlayers)
	session.SessionKey = make([]byte, 0)

	contentStream := nex.NewStreamOut(nexServer)
	contentStream.WriteStructure(session.Gathering)
	contentStream.WriteStructure(session)
	content := contentStream.Bytes()

	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteString("MatchmakeSession")
	rmcResponseStream.WriteUInt32LE(uint32(len(content)) + 4)
	rmcResponseStream.WriteUInt32LE(uint32(len(content)))
	rmcResponseStream.Grow(int64(len(content)))
	rmcResponseStream.WriteBytesNext(content)

	sendResponse(client, matchmake_extension.ProtocolID, callID, matchmake_extension.MethodAutoMatchmakeWithSearchCriteria_Postpone, rmcResponseStream.Bytes())
}

func handleOpenParticipation(packet *nex.PacketV1) {
	client := packet.Sender()
	request := packet.RMCRequest()
	stream := nex.NewStreamIn(request.Parameters(), nexServer)
	gid := stream.ReadUInt32LE()
	fmt.Printf("OpenParticipation: PID=%d gid=%d\n", client.PID(), gid)
	sendResponse(client, matchmake_extension.ProtocolID, request.CallID(), matchmake_extension.MethodOpenParticipation, nil)
}

func handleCloseParticipation(packet *nex.PacketV1) {
	client := packet.Sender()
	request := packet.RMCRequest()
	stream := nex.NewStreamIn(request.Parameters(), nexServer)
	gid := stream.ReadUInt32LE()
	fmt.Printf("CloseParticipation: PID=%d gid=%d\n", client.PID(), gid)
	dbCloseGathering(gid)
	go dbRecordMatch(gid)
	sendResponse(client, matchmake_extension.ProtocolID, request.CallID(), matchmake_extension.MethodCloseParticipation, nil)
}

func reportNATTraversalResult(err error, client *nex.Client, callID uint32, cid uint32, result bool, rtt uint32) {
	if err != nil {
		return
	}
	fmt.Printf("ReportNATTraversalResult: PID=%d cid=%d result=%v rtt=%d\n", client.PID(), cid, result, rtt)
	clearHolePunching(client.PID())
	sendResponse(client, nat_traversal.ProtocolID, callID, nat_traversal.MethodReportNATTraversalResult, []byte{})
	if result {
		// Don't close here — CloseParticipation is the actual game-start signal.
		// Just clear the quick-exit fail counter so a successful pair doesn't
		// penalise the gathering.
		if gid := dbFindGatheringForPID(client.PID()); gid != 0 {
			gatheringFailCount.Delete(gid)
		}
	} else {
		fmt.Printf("NAT traversal FAILED: PID=%d — blocking from matchmaking for %s\n", client.PID(), natFailureBlockDuration)
		blockFromMatchmaking(client.PID())
	}
}

func acquireNexUniqueID(err error, client *nex.Client, callID uint32) {
	if err != nil {
		fmt.Println("AcquireNexUniqueID error:", err)
		return
	}

	uniqueID := uint64(client.PID())<<32 | uint64(client.ConnectionID())
	fmt.Printf("AcquireNexUniqueID: PID=%d uniqueID=%d\n", client.PID(), uniqueID)

	rmcResponseStream := nex.NewStreamOut(nexServer)
	rmcResponseStream.WriteUInt64LE(uniqueID)
	sendResponse(client, utility.ProtocolID, callID, utility.MethodAcquireNexUniqueID, rmcResponseStream.Bytes())
}

func testConnectivity(err error, client *nex.Client, callID uint32) {
	if err != nil {
		return
	}
	sendResponse(client, secure_connection.ProtocolID, callID, secure_connection.MethodTestConnectivity, []byte{})
}

func sendReport(err error, client *nex.Client, callID uint32, reportID uint32, report []byte) {
	if err != nil {
		return
	}
	sendResponse(client, secure_connection.ProtocolID, callID, secure_connection.MethodSendReport, []byte{})
}

func reportNATProperties(err error, client *nex.Client, callID uint32, natm uint32, natf uint32, rtt uint32) {
	if err != nil {
		fmt.Println("ReportNATProperties error:", err)
		return
	}

	fmt.Printf("ReportNATProperties: PID=%d natm=%d natf=%d\n", client.PID(), natm, natf)
	pidNATm.Store(client.PID(), natm)
	pidNATf.Store(client.PID(), natf)

	urls := dbGetPlayerURLs(client.PID())
	pid := strconv.FormatUint(uint64(client.PID()), 10)
	rvcid := strconv.FormatUint(uint64(client.ConnectionID()), 10)

	for _, urlStr := range urls {
		u := nex.NewStationURL(urlStr)
		if u.Type() == "3" {
			u.SetNatm(strconv.FormatUint(uint64(natm), 10))
			u.SetNatf(strconv.FormatUint(uint64(natf), 10))
		}
		u.SetPID(pid)
		u.SetRVCID(rvcid)
		dbUpdateSessionURL(client.PID(), urlStr, u.EncodeToString())
	}

	sendResponse(client, nat_traversal.ProtocolID, callID, nat_traversal.MethodReportNATProperties, nil)
}
