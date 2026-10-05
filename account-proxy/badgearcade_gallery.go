package main

// Badge Arcade's Miiverse gallery, generated daily from Juxt.
//
// The gallery is part of Badge Arcade's SpotPass package data/data_v131.dat (one
// SARC): post/<postID>.sarc archives (post.xml + Image.jpg 320x240 + Mii.Etc1_a4
// 128x128), listed in Schedule.xml as "Post" items with a date window the game
// checks. Nintendo's archived package only has posts dated 2023, so the game
// shows its built-in bunny posts instead. Confirmed 2026-10-05 on a real 3DS that
// a package we re-encrypt (see boss3ds.go) with current dates and a new
// ns_data_id is accepted and its posts are shown.
//
// Every UTC day this replaces the posts with the Badge Arcade community's
// (badgeArcadeGalleryCommunity) most-yeahed picture posts from the previous UTC
// day, topped up with the most-yeahed older ones, for every region's package.

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Happynico7504/badgearcade"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	badgeArcadeGalleryCommunity = "1162839996" // Juxt's "Nintendo Badge Arcade" community
	badgeArcadeGalleryMax       = 20           // what Nintendo's packages carry
	badgeArcadeGalleryAssetBase = "https://olv-data.sos-de-fra-1.exo.io"
	badgeArcadeGalleryDataFile  = "data_data_v131.dat.boss"
	badgeArcadeGalleryDir       = "generated" // under badgeArcadeBossDataDir
)

// badgeArcadeGalleryPrefixes are the file sets that get a generated gallery
// (one per regional bossAppId, see badgeArcadeBossAppIDPrefix).
var badgeArcadeGalleryPrefixes = []string{"GB_en", "US_en", "JP_ja"}

// badgeArcadeGalleryForEveryone serves the generated package to every console
// (enabled 2026-10-05 after it was confirmed on real hardware). When false, only
// badgeArcadeGalleryTestConsoles get it - useful for trying out package changes.
const badgeArcadeGalleryForEveryone = true

// badgeArcadeTestConsole reports whether the request's SpotPass User-Agent
// ("PBOS-8.0/<console id>-...") is listed in badgeArcadeBossDataDir/test-consoles.txt
// (one ID per line, # comments). Test consoles get the gallery even when it isn't
// served to everyone, and the experimental package variant (with "test"
// deployments) when one exists. A
// file outside the repo keeps console IDs private, and edits apply immediately.
func badgeArcadeTestConsole(r *http.Request) bool {
	id, _, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("User-Agent"), "PBOS-8.0/"), "-")
	if id == "" {
		return false
	}
	raw, err := os.ReadFile(badgeArcadeBossDataDir + "/test-consoles.txt")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line, _, _ = strings.Cut(line, "#")
		if strings.TrimSpace(line) == id {
			return true
		}
	}
	return false
}

func badgeArcadeGalleryEnabledFor(r *http.Request) bool {
	return badgeArcadeGalleryForEveryone || badgeArcadeTestConsole(r)
}

// badgeArcadePackageVariant is one flavour of the generated package.
type badgeArcadePackageVariant struct {
	Dir         string // under badgeArcadeBossDataDir
	IncludeTest bool   // also build "test" deployments (test consoles only)
	NsBase      uint32 // keeps the variants' ns_data_ids apart
}

var (
	badgeArcadeStableVariant       = badgeArcadePackageVariant{Dir: badgeArcadeGalleryDir, NsBase: 1000000}
	badgeArcadeExperimentalVariant = badgeArcadePackageVariant{Dir: badgeArcadeGalleryDir + "/experimental", IncludeTest: true, NsBase: 3000000}
	badgeArcadePackageVariants     = []badgeArcadePackageVariant{badgeArcadeStableVariant, badgeArcadeExperimentalVariant}
)

// badgeArcadeMaxRevision is how often one day's package can be rebuilt
// ("Publish now") while keeping ns_data_ids unique: NsBase + day*10 + rev.
const badgeArcadeMaxRevision = 9

// badgeArcadePackageNsDataID gives every day and revision its own ns_data_id
// (above all of Nintendo's, max 11500), so consoles take it as new data.
func badgeArcadePackageNsDataID(v badgeArcadePackageVariant, day time.Time, rev int) uint32 {
	return v.NsBase + uint32(day.Unix()/86400)*10 + uint32(rev)
}

// badgeArcadeGalleryFile returns the generated package to serve for this
// request, relative to badgeArcadeBossDataDir, or "" for none: the experimental
// variant for test consoles when it exists, otherwise the stable one.
func badgeArcadeGalleryFile(r *http.Request, prefix, fragment string) string {
	if fragment != badgeArcadeGalleryDataFile || !badgeArcadeGalleryEnabledFor(r) {
		return ""
	}
	variants := []badgeArcadePackageVariant{badgeArcadeStableVariant}
	if badgeArcadeTestConsole(r) {
		variants = []badgeArcadePackageVariant{badgeArcadeExperimentalVariant, badgeArcadeStableVariant}
	}
	for _, v := range variants {
		rel := v.Dir + "/" + prefix + "_" + fragment
		if _, err := os.Stat(badgeArcadeBossDataDir + "/" + rel); err == nil {
			return rel
		}
	}
	return ""
}

type badgeArcadeGalleryPost struct {
	ID          string    `bson:"id"`
	Name        string    `bson:"screen_name"`
	PID         int64     `bson:"pid"`
	Screenshot  string    `bson:"screenshot"`
	PaintingImg string    `bson:"painting_img"`
	Yeahs       int       `bson:"empathy_count"`
	CreatedAt   time.Time `bson:"created_at"`
}

// selectBadgeArcadeGalleryPosts picks up to badgeArcadeGalleryMax picture posts:
// the previous UTC day's by yeahs, then older ones by yeahs. Replies, private
// messages, removed posts and spoilers are never included.
func selectBadgeArcadeGalleryPosts(ctx context.Context, dayStart time.Time) ([]badgeArcadeGalleryPost, error) {
	col := mongoDB.Client().Database("juxt").Collection("posts")
	base := bson.M{
		"community_id": badgeArcadeGalleryCommunity,
		"removed":      bson.M{"$ne": true},
		"is_spoiler":   bson.M{"$ne": 1},
		"parent":       bson.M{"$in": bson.A{nil, ""}},
		"$and": bson.A{
			bson.M{"$or": bson.A{bson.M{"message_to_pid": bson.M{"$exists": false}}, bson.M{"message_to_pid": bson.M{"$in": bson.A{nil, ""}}}}},
			bson.M{"$or": bson.A{bson.M{"screenshot": bson.M{"$regex": "^/"}}, bson.M{"painting_img": bson.M{"$regex": "^/"}}}},
		},
	}
	find := func(created bson.M, limit int) ([]badgeArcadeGalleryPost, error) {
		opts := options.Find().SetSort(bson.D{{Key: "empathy_count", Value: -1}, {Key: "created_at", Value: -1}}).SetLimit(int64(limit))
		f := bson.M{"created_at": created}
		for k, v := range base {
			f[k] = v
		}
		cur, err := col.Find(ctx, f, opts)
		if err != nil {
			return nil, err
		}
		var out []badgeArcadeGalleryPost
		return out, cur.All(ctx, &out)
	}
	posts, err := find(bson.M{"$gte": dayStart.Add(-24 * time.Hour), "$lt": dayStart}, badgeArcadeGalleryMax)
	if err != nil {
		return nil, err
	}
	if len(posts) < badgeArcadeGalleryMax {
		older, err := find(bson.M{"$lt": dayStart.Add(-24 * time.Hour)}, badgeArcadeGalleryMax-len(posts))
		if err != nil {
			return nil, err
		}
		posts = append(posts, older...)
	}
	return posts, nil
}

var badgeArcadeGalleryHTTP = &http.Client{Timeout: 20 * time.Second}

func fetchBadgeArcadeGalleryImage(url string) (image.Image, error) {
	resp, err := badgeArcadeGalleryHTTP.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	img, _, err := image.Decode(io.LimitReader(resp.Body, 8<<20))
	return img, err
}

func xmlEscapeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			if r >= 0x20 || r == '\t' {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// buildBadgeArcadeGalleryPost makes one post/<id>.sarc in Nintendo's layout.
func buildBadgeArcadeGalleryPost(p badgeArcadeGalleryPost, index int) ([]byte, error) {
	src := p.Screenshot
	if src == "" {
		src = p.PaintingImg
	}
	pic, err := fetchBadgeArcadeGalleryImage(badgeArcadeGalleryAssetBase + src)
	if err != nil {
		return nil, err
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, badgearcade.FitImage(pic, 320, 240, color.Black), &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	// A missing Mii face shouldn't drop the post; it then shows no Mii.
	face := image.Image(image.NewNRGBA(image.Rect(0, 0, 128, 128)))
	if f, err := fetchBadgeArcadeGalleryImage(fmt.Sprintf("%s/mii/%d/normal_face.png", badgeArcadeGalleryAssetBase, p.PID)); err == nil {
		face = badgearcade.FitImage(f, 128, 128, color.Transparent)
	} else {
		log.Printf("badge arcade gallery: no Mii for post %s: %v", p.ID, err)
	}
	postXML := "<?xml version=\"1.0\" encoding=\"utf-8\"?>\r\n" +
		"<DistributablePost xmlns:xsi=\"http://www.w3.org/2001/XMLSchema-instance\" xmlns:xsd=\"http://www.w3.org/2001/XMLSchema\">\r\n" +
		"  <index>" + strconv.Itoa(index) + "</index>\r\n" +
		"  <Id>" + xmlEscapeText(p.ID) + "</Id>\r\n" +
		"  <Name>" + xmlEscapeText(p.Name) + "</Name>\r\n" +
		"  <PathMii>Mii.Etc1_a4</PathMii>\r\n" +
		"  <PathImage>Image.jpg</PathImage>\r\n" +
		"</DistributablePost>"
	return badgearcade.BuildSARC([]badgearcade.SARCEntry{
		{Name: "post.xml", Data: []byte(postXML)},
		{Name: "Image.jpg", Data: jpg.Bytes()},
		{Name: "Mii.Etc1_a4", Data: badgearcade.EncodeETC1A4(face, 128, 128)},
	}, 128, 128), nil
}

var (
	scheduleItemRe    = regexp.MustCompile(`(?s)<FileItem>.*?</FileItem>`)
	scheduleDateRe    = regexp.MustCompile(`<(DateStartText|DateExpireText)>\d{8}<`)
	scheduleKeyRe     = regexp.MustCompile(`<Key>post/Post\d+\.sarc</Key>`)
	scheduleArgRe     = regexp.MustCompile(`<Arguments>([^<]*)</Arguments>`)
	scheduleCountRe   = regexp.MustCompile(`<ItemsCount>\d+</ItemsCount>`)
	postXMLIndexRe    = regexp.MustCompile(`<index>(\d+)</index>`)
	badgeArcadeGalMux sync.Mutex
)

// redateScheduleItem gives a schedule item a window the game accepts today.
func redateScheduleItem(item string) string {
	return scheduleDateRe.ReplaceAllStringFunc(item, func(m string) string {
		if strings.HasPrefix(m, "<DateStartText>") {
			return "<DateStartText>20260101<"
		}
		return "<DateExpireText>20991231<"
	})
}

// rewriteBadgeArcadeSchedule replaces Schedule.xml's Post items with one per
// post ID (in order), or - when ids is nil - just re-dates the existing ones.
func rewriteBadgeArcadeSchedule(schedule string, ids []string) (string, error) {
	locs := scheduleItemRe.FindAllStringIndex(schedule, -1)
	var postLocs [][]int
	for _, l := range locs {
		if strings.Contains(schedule[l[0]:l[1]], "<RegexSetName>Post</RegexSetName>") {
			postLocs = append(postLocs, l)
		}
	}
	if len(postLocs) == 0 {
		return "", fmt.Errorf("schedule has no Post items")
	}
	template := schedule[postLocs[0][0]:postLocs[0][1]]
	m := scheduleArgRe.FindStringSubmatch(template)
	if m == nil || m[1] == "" {
		return "", fmt.Errorf("Post item has no Arguments")
	}
	oldID := m[1]

	var items []string
	if ids == nil {
		for _, l := range postLocs {
			items = append(items, redateScheduleItem(schedule[l[0]:l[1]]))
		}
	} else {
		for i, id := range ids {
			it := strings.ReplaceAll(template, oldID, id)
			it = scheduleKeyRe.ReplaceAllString(it, fmt.Sprintf("<Key>post/Post%02d.sarc</Key>", i))
			items = append(items, redateScheduleItem(it))
		}
	}
	// Post items are consecutive in Nintendo's files; keep the separator they use.
	sep := "\r\n    "
	if len(postLocs) > 1 {
		sep = schedule[postLocs[0][1]:postLocs[1][0]]
	}
	var b strings.Builder
	b.WriteString(schedule[:postLocs[0][0]])
	b.WriteString(strings.Join(items, sep))
	last := postLocs[len(postLocs)-1][1]
	for i := 1; i < len(postLocs); i++ { // anything between Post items that isn't one
		if gap := schedule[postLocs[i-1][1]:postLocs[i][0]]; strings.TrimSpace(gap) != "" {
			return "", fmt.Errorf("Post items are not consecutive")
		}
	}
	b.WriteString(schedule[last:])
	out := b.String()
	total := len(locs) - len(postLocs) + len(items)
	return scheduleCountRe.ReplaceAllString(out, fmt.Sprintf("<ItemsCount>%d</ItemsCount>", total)), nil
}

// redateCraneSchedule puts a crane lineup on today's schedule: the crane archive
// (PrizeCollection) gets an open window, and of Nintendo's seven daily groups of
// DefaultStage (30 machines) and BonusStage items, the one picked by the day
// number gets today as its window. The other days stay expired.
func redateCraneSchedule(schedule string, dayStart time.Time) string {
	startRe := regexp.MustCompile(`<DateStartText>(\d{8})<`)
	setName := func(item string) string {
		if m := regexp.MustCompile(`<RegexSetName>([^<]*)<`).FindStringSubmatch(item); m != nil {
			return m[1]
		}
		return ""
	}
	days := map[string]bool{}
	for _, it := range scheduleItemRe.FindAllString(schedule, -1) {
		if n := setName(it); n == "DefaultStage" || n == "BonusStage" {
			if m := startRe.FindStringSubmatch(it); m != nil {
				days[m[1]] = true
			}
		}
	}
	var sorted []string
	for d := range days {
		sorted = append(sorted, d)
	}
	sort.Strings(sorted)
	pick := ""
	if len(sorted) > 0 {
		pick = sorted[int(dayStart.Unix()/86400)%len(sorted)]
	}
	today, tomorrow := dayStart.Format("20060102"), dayStart.Add(24*time.Hour).Format("20060102")
	window := func(item, from, to string) string {
		item = regexp.MustCompile(`<DateStartText>\d{8}<`).ReplaceAllString(item, "<DateStartText>"+from+"<")
		return regexp.MustCompile(`<DateExpireText>\d{8}<`).ReplaceAllString(item, "<DateExpireText>"+to+"<")
	}
	return scheduleItemRe.ReplaceAllStringFunc(schedule, func(it string) string {
		switch setName(it) {
		case "PrizeCollection":
			return window(it, "20260101", "20991231")
		case "DefaultStage", "BonusStage":
			if m := startRe.FindStringSubmatch(it); m != nil && m[1] == pick {
				return window(it, today, tomorrow)
			}
		}
		return it
	})
}

var (
	boss3DSKeyOnce sync.Once
	boss3DSKey     []byte
	boss3DSKeyErr  error
)

// loadBoss3DSKey loads the 3DS BOSS key from BOSS3DS_KEY_FILE (default
// /home/nico/boss3ds_key.hex; console-derived, so kept outside the repo).
func loadBoss3DSKey() ([]byte, error) {
	boss3DSKeyOnce.Do(func() {
		path := os.Getenv("BOSS3DS_KEY_FILE")
		if path == "" {
			path = "/home/nico/boss3ds_key.hex"
		}
		boss3DSKey, boss3DSKeyErr = badgearcade.LoadBOSSKey(path)
	})
	return boss3DSKey, boss3DSKeyErr
}

// generateBadgeArcadeGallery builds one file set's package for dayStart (gallery
// posts plus deployments) and returns its bytes plus the posts used.
func generateBadgeArcadeGallery(ctx context.Context, prefix string, dayStart time.Time, posts []badgeArcadeGalleryPost,
	deps []badgeArcadeDeployment, variant badgeArcadePackageVariant, rev int) ([]byte, []string, error) {
	key, err := loadBoss3DSKey()
	if err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(badgeArcadeBossDataDir + "/" + prefix + "_" + badgeArcadeGalleryDataFile)
	if err != nil {
		return nil, nil, err
	}
	serial, payloads, err := badgearcade.ParseBOSS(key, raw)
	if err != nil || len(payloads) != 1 {
		return nil, nil, fmt.Errorf("%s: %v (payloads %d)", prefix, err, len(payloads))
	}
	entries, err := badgearcade.ParseSARC(payloads[0].Content)
	if err != nil {
		return nil, nil, err
	}

	var kept []badgearcade.SARCEntry
	var indexes []int // Nintendo's <index> values, reused in slot order
	var schedule string
	for _, e := range entries {
		switch {
		case e.Name == "Schedule.xml":
			schedule = string(e.Data) // replaced below
		case strings.HasPrefix(e.Name, "post/"):
			if inner, err := badgearcade.ParseSARC(e.Data); err == nil {
				for _, f := range inner {
					if f.Name == "post.xml" {
						if m := postXMLIndexRe.FindSubmatch(f.Data); m != nil {
							n, _ := strconv.Atoi(string(m[1]))
							indexes = append(indexes, n)
						}
					}
				}
			}
			if len(posts) == 0 {
				kept = append(kept, e) // nothing to show: keep Nintendo's posts
			}
		default:
			kept = append(kept, e)
		}
	}
	if schedule == "" || len(indexes) == 0 {
		return nil, nil, fmt.Errorf("%s: package has no Schedule.xml or posts", prefix)
	}

	var ids []string
	for _, p := range posts {
		sarc, err := buildBadgeArcadeGalleryPost(p, indexes[len(ids)%len(indexes)])
		if err != nil {
			log.Printf("badge arcade gallery: skipping post %s: %v", p.ID, err)
			continue
		}
		kept = append(kept, badgearcade.SARCEntry{Name: "post/" + p.ID + ".sarc", Data: sarc})
		ids = append(ids, p.ID)
	}
	if len(posts) > 0 && len(ids) == 0 {
		return nil, nil, fmt.Errorf("%s: none of %d posts could be built", prefix, len(posts))
	}
	var newSchedule string
	if len(ids) == 0 {
		newSchedule, err = rewriteBadgeArcadeSchedule(schedule, nil)
	} else {
		newSchedule, err = rewriteBadgeArcadeSchedule(schedule, ids)
	}
	if err != nil {
		return nil, nil, err
	}
	if len(deps) > 0 {
		newSchedule = redateCraneSchedule(newSchedule, dayStart)
		if newSchedule, err = applyBadgeArcadeDeployments(prefix, kept, newSchedule, dayStart, deps); err != nil {
			return nil, nil, err
		}
	}
	kept = append(kept, badgearcade.SARCEntry{Name: "Schedule.xml", Data: []byte(newSchedule)})

	payloads[0].Content = badgearcade.BuildSARC(kept, 4, 16)
	payloads[0].NsDataID = badgeArcadePackageNsDataID(variant, dayStart, rev)
	payloads[0].Version = 1
	out, err := badgearcade.BuildBOSS(key, serial, payloads, nil)
	return out, ids, err
}

// rebuildBadgeArcadePackages (re)builds every variant's and file set's package
// for the current UTC day: once per day normally, or - with force - again with
// the next revision when that variant's deployments changed ("Publish now";
// unchanged packages keep their ns_data_id so consoles don't re-download
// them). The .day marker holds "<date> <revision> <deployments fingerprint>".
func rebuildBadgeArcadePackages(ctx context.Context, now time.Time, force bool) {
	badgeArcadeGalMux.Lock()
	defer badgeArcadeGalMux.Unlock()
	dayStart := now.UTC().Truncate(24 * time.Hour)
	dayTag := dayStart.Format("2006-01-02")
	var posts []badgeArcadeGalleryPost
	selected := false
	for _, variant := range badgeArcadePackageVariants {
		dir := badgeArcadeBossDataDir + "/" + variant.Dir
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("badge arcade gallery: %v", err)
			return
		}
		deps, err := loadBadgeArcadeDeployments(ctx, dayStart, variant.IncludeTest)
		if err != nil {
			log.Printf("badge arcade gallery: loading deployments: %v", err)
			return
		}
		fingerprint := badgeArcadeDeploymentsFingerprint(deps)
		for _, prefix := range badgeArcadeGalleryPrefixes {
			marker := dir + "/" + prefix + ".day"
			rev := 0
			if b, err := os.ReadFile(marker); err == nil {
				f := strings.Fields(string(b))
				if len(f) > 0 && f[0] == dayTag {
					if !force || (len(f) > 2 && f[2] == fingerprint) {
						continue
					}
					if len(f) > 1 {
						rev, _ = strconv.Atoi(f[1])
					}
					if rev >= badgeArcadeMaxRevision {
						log.Printf("badge arcade gallery: %s/%s already rebuilt %d times today; next build at midnight", variant.Dir, prefix, rev)
						continue
					}
					rev++
				}
			}
			if !selected {
				if posts, err = selectBadgeArcadeGalleryPosts(ctx, dayStart); err != nil {
					log.Printf("badge arcade gallery: selecting posts: %v", err)
					return
				}
				selected = true
			}
			out, ids, err := generateBadgeArcadeGallery(ctx, prefix, dayStart, posts, deps, variant, rev)
			if err != nil {
				log.Printf("badge arcade gallery: %s: %v", prefix, err)
				continue
			}
			final := dir + "/" + prefix + "_" + badgeArcadeGalleryDataFile
			if err := os.WriteFile(final+".tmp", out, 0o644); err != nil || os.Rename(final+".tmp", final) != nil {
				log.Printf("badge arcade gallery: writing %s failed", final)
				continue
			}
			os.WriteFile(marker, []byte(fmt.Sprintf("%s %d %s\n", dayTag, rev, fingerprint)), 0o644)
			log.Printf("badge arcade gallery: built %s/%s for %s rev %d (ns_data_id %d, %d bytes, posts %v, %d deployments)",
				variant.Dir, prefix, dayTag, rev, badgeArcadePackageNsDataID(variant, dayStart, rev), len(out), ids, len(deps))
		}
	}
}

// badgeArcadeGalleryLoop builds the gallery at startup and shortly after every
// UTC midnight; failures retry every 10 minutes.
func badgeArcadeGalleryLoop() {
	for {
		rebuildBadgeArcadePackages(context.Background(), time.Now(), false)
		now := time.Now().UTC()
		next := now.Truncate(24 * time.Hour).Add(24*time.Hour + 2*time.Minute)
		if wait := time.Until(next); wait > 10*time.Minute {
			// Re-check every 10 minutes so a failed build retries soon; a finished
			// day is skipped via its .day marker.
			time.Sleep(10 * time.Minute)
		} else {
			time.Sleep(wait)
		}
	}
}
