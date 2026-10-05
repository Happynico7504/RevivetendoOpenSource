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
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

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

// badgeArcadeGalleryTestConsoles are console IDs from the SpotPass User-Agent
// ("PBOS-8.0/<id>-...") that get the generated package even when it isn't
// served to everyone.
var badgeArcadeGalleryTestConsoles = map[string]bool{}

func badgeArcadeGalleryEnabledFor(r *http.Request) bool {
	if badgeArcadeGalleryForEveryone {
		return true
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("User-Agent"), "PBOS-8.0/"), "-")
	return badgeArcadeGalleryTestConsoles[id]
}

// badgeArcadeGalleryFile returns the generated package's path relative to
// badgeArcadeBossDataDir, or "" when there is none for this file set.
func badgeArcadeGalleryFile(prefix, fragment string) string {
	if fragment != badgeArcadeGalleryDataFile {
		return ""
	}
	rel := badgeArcadeGalleryDir + "/" + prefix + "_" + fragment
	if _, err := os.Stat(badgeArcadeBossDataDir + "/" + rel); err != nil {
		return ""
	}
	return rel
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

// fitImage scales src (bilinear) to fit inside w x h, centred on bg.
func fitImage(src image.Image, w, h int, bg color.Color) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw == 0 || sh == 0 {
		return dst
	}
	scale := float64(w) / float64(sw)
	if s := float64(h) / float64(sh); s < scale {
		scale = s
	}
	tw, th := int(float64(sw)*scale+0.5), int(float64(sh)*scale+0.5)
	ox, oy := (w-tw)/2, (h-th)/2
	at := func(x, y int) color.NRGBA {
		if x >= sw {
			x = sw - 1
		}
		if y >= sh {
			y = sh - 1
		}
		return color.NRGBAModel.Convert(src.At(sb.Min.X+x, sb.Min.Y+y)).(color.NRGBA)
	}
	for y := 0; y < th; y++ {
		fy := (float64(y)+0.5)/scale - 0.5
		if fy < 0 {
			fy = 0
		}
		y0 := int(fy)
		wy := fy - float64(y0)
		for x := 0; x < tw; x++ {
			fx := (float64(x)+0.5)/scale - 0.5
			if fx < 0 {
				fx = 0
			}
			x0 := int(fx)
			wx := fx - float64(x0)
			c00, c10, c01, c11 := at(x0, y0), at(x0+1, y0), at(x0, y0+1), at(x0+1, y0+1)
			mix := func(a, b, c, d uint8) uint8 {
				top := float64(a)*(1-wx) + float64(b)*wx
				bot := float64(c)*(1-wx) + float64(d)*wx
				return uint8(top*(1-wy) + bot*wy + 0.5)
			}
			dst.SetNRGBA(ox+x, oy+y, color.NRGBA{
				mix(c00.R, c10.R, c01.R, c11.R), mix(c00.G, c10.G, c01.G, c11.G),
				mix(c00.B, c10.B, c01.B, c11.B), mix(c00.A, c10.A, c01.A, c11.A),
			})
		}
	}
	return dst
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
	if err := jpeg.Encode(&jpg, fitImage(pic, 320, 240, color.Black), &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	// A missing Mii face shouldn't drop the post; it then shows no Mii.
	face := image.Image(image.NewNRGBA(image.Rect(0, 0, 128, 128)))
	if f, err := fetchBadgeArcadeGalleryImage(fmt.Sprintf("%s/mii/%d/normal_face.png", badgeArcadeGalleryAssetBase, p.PID)); err == nil {
		face = fitImage(f, 128, 128, color.Transparent)
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
	return buildSARC([]sarcEntry{
		{Name: "post.xml", Data: []byte(postXML)},
		{Name: "Image.jpg", Data: jpg.Bytes()},
		{Name: "Mii.Etc1_a4", Data: encodeETC1A4(face, 128, 128)},
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

// badgeArcadeGalleryNsDataID is unique per UTC day and above every ns_data_id
// Nintendo used for Badge Arcade (max seen: 11500), so consoles always take a
// new day's package as new data.
func badgeArcadeGalleryNsDataID(dayStart time.Time) uint32 {
	return uint32(100000 + dayStart.Unix()/86400)
}

// generateBadgeArcadeGallery builds one file set's package for dayStart and
// returns its bytes plus the posts used.
func generateBadgeArcadeGallery(ctx context.Context, prefix string, dayStart time.Time, posts []badgeArcadeGalleryPost) ([]byte, []string, error) {
	key, err := loadBoss3DSKey()
	if err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(badgeArcadeBossDataDir + "/" + prefix + "_" + badgeArcadeGalleryDataFile)
	if err != nil {
		return nil, nil, err
	}
	serial, payloads, err := parseBoss3DS(key, raw)
	if err != nil || len(payloads) != 1 {
		return nil, nil, fmt.Errorf("%s: %v (payloads %d)", prefix, err, len(payloads))
	}
	entries, err := parseSARC(payloads[0].Content)
	if err != nil {
		return nil, nil, err
	}

	var kept []sarcEntry
	var indexes []int // Nintendo's <index> values, reused in slot order
	var schedule string
	for _, e := range entries {
		switch {
		case e.Name == "Schedule.xml":
			schedule = string(e.Data) // replaced below
		case strings.HasPrefix(e.Name, "post/"):
			if inner, err := parseSARC(e.Data); err == nil {
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
		kept = append(kept, sarcEntry{Name: "post/" + p.ID + ".sarc", Data: sarc})
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
	kept = append(kept, sarcEntry{Name: "Schedule.xml", Data: []byte(newSchedule)})

	payloads[0].Content = buildSARC(kept, 4, 16)
	payloads[0].NsDataID = badgeArcadeGalleryNsDataID(dayStart)
	payloads[0].Version = 1
	out, err := buildBoss3DS(key, serial, payloads, nil)
	return out, ids, err
}

// refreshBadgeArcadeGallery (re)builds every file set's package for the
// current UTC day unless it already exists.
func refreshBadgeArcadeGallery(ctx context.Context, now time.Time) {
	badgeArcadeGalMux.Lock()
	defer badgeArcadeGalMux.Unlock()
	dayStart := now.UTC().Truncate(24 * time.Hour)
	dir := badgeArcadeBossDataDir + "/" + badgeArcadeGalleryDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("badge arcade gallery: %v", err)
		return
	}
	dayTag := dayStart.Format("2006-01-02")
	var posts []badgeArcadeGalleryPost
	selected := false
	for _, prefix := range badgeArcadeGalleryPrefixes {
		marker := dir + "/" + prefix + ".day"
		if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == dayTag {
			continue
		}
		if !selected {
			var err error
			if posts, err = selectBadgeArcadeGalleryPosts(ctx, dayStart); err != nil {
				log.Printf("badge arcade gallery: selecting posts: %v", err)
				return
			}
			selected = true
		}
		out, ids, err := generateBadgeArcadeGallery(ctx, prefix, dayStart, posts)
		if err != nil {
			log.Printf("badge arcade gallery: %s: %v", prefix, err)
			continue
		}
		final := dir + "/" + prefix + "_" + badgeArcadeGalleryDataFile
		if err := os.WriteFile(final+".tmp", out, 0o644); err != nil || os.Rename(final+".tmp", final) != nil {
			log.Printf("badge arcade gallery: writing %s failed", final)
			continue
		}
		os.WriteFile(marker, []byte(dayTag+"\n"), 0o644)
		log.Printf("badge arcade gallery: built %s for %s (ns_data_id %d, %d bytes, posts %v)",
			prefix, dayTag, badgeArcadeGalleryNsDataID(dayStart), len(out), ids)
	}
}

// badgeArcadeGalleryLoop builds the gallery at startup and shortly after every
// UTC midnight; failures retry every 10 minutes.
func badgeArcadeGalleryLoop() {
	for {
		refreshBadgeArcadeGallery(context.Background(), time.Now())
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
