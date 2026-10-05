package main

import (
	"context"
	"fmt"
	"image/png"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Happynico7504/badgearcade"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// loadBadgeArcadeData decrypts one of Nintendo's archived packages from
// badgeArcadeBossDataDir (kept out of the repo); skips when unavailable.
func loadBadgeArcadeData(t *testing.T, prefix string) (key, raw []byte, serial uint64, payloads []badgearcade.BOSSPayload) {
	t.Helper()
	key, err := loadBoss3DSKey()
	if err != nil {
		t.Skipf("no 3DS BOSS key: %v", err)
	}
	raw, err = os.ReadFile(badgeArcadeBossDataDir + "/" + prefix + "_data_data_v131.dat.boss")
	if err != nil {
		t.Skipf("no archived content: %v", err)
	}
	serial, payloads, err = badgearcade.ParseBOSS(key, raw)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func badgeArcadeTestSchedule(t *testing.T) string {
	_, _, _, payloads := loadBadgeArcadeData(t, "GB_en")
	entries, err := badgearcade.ParseSARC(payloads[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "Schedule.xml" {
			return string(e.Data)
		}
	}
	t.Fatal("no Schedule.xml")
	return ""
}

func TestRewriteBadgeArcadeSchedule(t *testing.T) {
	orig := badgeArcadeTestSchedule(t)
	ids := []string{"J6qpTZV9MpdenxllVvsNV", "QbI9ACH9wG9a2TTQ00twb"}
	out, err := rewriteBadgeArcadeSchedule(orig, ids)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "<RegexSetName>Post</RegexSetName>"); n != len(ids) {
		t.Fatalf("%d Post items, want %d", n, len(ids))
	}
	if strings.Contains(out, "AYMHAAADAAB2V0f") {
		t.Fatal("a Nintendo post ID is still referenced")
	}
	for i, id := range ids {
		for _, want := range []string{
			"<Value>post/" + id + ".sarc</Value>",
			"<Arguments>" + id + "</Arguments>",
			fmt.Sprintf("<Key>post/Post%02d.sarc</Key>", i),
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("missing %s", want)
			}
		}
	}
	items := strings.Count(out, "<FileItem>")
	if !strings.Contains(out, fmt.Sprintf("<ItemsCount>%d</ItemsCount>", items)) {
		t.Fatalf("ItemsCount not updated to %d", items)
	}
	if strings.Count(out, "<DateStartText>20260101<") != len(ids) {
		t.Fatal("posts not re-dated")
	}
	if strings.Count(orig, "<FileItem>")-20+len(ids) != items {
		t.Fatal("non-Post items changed")
	}

	redated, err := rewriteBadgeArcadeSchedule(orig, nil)
	if err != nil || strings.Count(redated, "<DateStartText>20260101<") != 20 || len(redated) != len(orig) {
		t.Fatalf("re-date only: err=%v len %d vs %d", err, len(redated), len(orig))
	}
}

// TestBadgeArcadeGalleryLive builds today's EUR package from the real Juxt
// database and writes each post's image, Mii and post.xml to the directory in
// BADGE_ARCADE_GALLERY_LIVE.
func TestBadgeArcadeGalleryLive(t *testing.T) {
	dir := os.Getenv("BADGE_ARCADE_GALLERY_LIVE")
	if dir == "" {
		t.Skip("set BADGE_ARCADE_GALLERY_LIVE=<dir> to run")
	}
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017/"))
	if err != nil {
		t.Fatal(err)
	}
	mongoDB = client.Database("pretendo")
	day := time.Now().UTC().Truncate(24 * time.Hour)
	posts, err := selectBadgeArcadeGalleryPosts(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range posts {
		t.Logf("post %s by %s, %d yeahs, %s", p.ID, p.Name, p.Yeahs, p.CreatedAt.Format(time.RFC3339))
	}
	out, ids, err := generateBadgeArcadeGallery(ctx, "GB_en", day, posts, badgeArcadeStableVariant)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := loadBoss3DSKey()
	_, payloads, err := badgearcade.ParseBOSS(key, out)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("package %d bytes, ns_data_id %d, posts %v", len(out), payloads[0].NsDataID, ids)
	entries, err := badgearcade.ParseSARC(payloads[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name, "post/") {
			continue
		}
		inner, err := badgearcade.ParseSARC(e.Data)
		if err != nil {
			t.Fatal(err)
		}
		base := dir + "/" + strings.TrimSuffix(strings.TrimPrefix(e.Name, "post/"), ".sarc")
		for _, f := range inner {
			switch f.Name {
			case "Image.jpg":
				os.WriteFile(base+".jpg", f.Data, 0o644)
			case "post.xml":
				os.WriteFile(base+".xml", f.Data, 0o644)
			case "Mii.Etc1_a4":
				if w, err := os.Create(base + "_mii.png"); err == nil {
					png.Encode(w, badgearcade.DecodeETC1A4(f.Data, 128, 128))
					w.Close()
				}
			}
		}
	}
}

func TestRedateCraneSchedule(t *testing.T) {
	orig := badgeArcadeTestSchedule(t)
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	out := redateCraneSchedule(orig, day)
	if n := strings.Count(out, "<DateStartText>20261005<"); n != 31 { // 30 DefaultStage + 1 BonusStage
		t.Fatalf("%d items dated today, want 31", n)
	}
	if strings.Count(out, "<DateExpireText>20261006<") != 31 {
		t.Fatal("today's items should expire tomorrow")
	}
	if !strings.Contains(out, "<RegexSetName>PrizeCollection</RegexSetName>") || strings.Count(out, "<DateExpireText>20991231<") != 1 {
		t.Fatal("PrizeCollection not opened")
	}
	if len(out) != len(orig) {
		t.Fatalf("length changed %d -> %d", len(orig), len(out))
	}
	if next := redateCraneSchedule(orig, day.Add(24*time.Hour)); next == strings.ReplaceAll(out, "20261005", "20261006") {
		t.Fatal("consecutive days should pick different lineups")
	}
}

func TestBadgeArcadeCustomTest(t *testing.T) {
	if _, err := os.Stat(badgeArcadeBossDataDir + "/custom/test-badge.png"); err != nil {
		t.Skip("no custom test badge")
	}
	loadBadgeArcadeData(t, "GB_en") // skips without key/content
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	out, _, err := generateBadgeArcadeGallery(context.Background(), "GB_en", day, nil, badgeArcadeExperimentalVariant)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := loadBoss3DSKey()
	_, payloads, err := badgearcade.ParseBOSS(key, out)
	if err != nil {
		t.Fatal(err)
	}
	if payloads[0].NsDataID != badgeArcadeGalleryNsDataID(day)+badgeArcadeExperimentalVariant.NsOffset {
		t.Fatal("wrong ns_data_id")
	}
	entries, _ := badgearcade.ParseSARC(payloads[0].Content)
	var schedule string
	var weekly []badgearcade.SARCEntry
	for _, e := range entries {
		if e.Name == "Schedule.xml" {
			schedule = string(e.Data)
		}
		if strings.HasPrefix(e.Name, "sharc/") {
			weekly, _ = badgearcade.ParseSARC(e.Data)
		}
	}
	found := map[string]bool{}
	for _, w := range weekly {
		found[w.Name] = true
		if w.Name == badgearcade.CraneInstancePath(badgeArcadeCustomMachine) {
			c, err := badgearcade.ParseCraneInstanceFile(w.Data)
			if err != nil || c.Prizes[0] != badgeArcadeCustomBadge || c.ID != badgeArcadeCustomCraneID {
				t.Fatalf("custom machine wrong: %v", err)
			}
			t.Logf("machine %s: stage %s, %d prize spots, category from template", c.Name, c.Crane, len(c.MachinePrizes))
		}
		if w.Name == badgearcade.PrizePath(badgeArcadeCustomBadge) {
			p, err := badgearcade.ParsePrizeFile(w.Data)
			if err != nil || p.BadgeID != badgeArcadeCustomBadgeID {
				t.Fatalf("custom badge wrong: %v", err)
			}
			polys, _ := p.CollisionPolygons()
			t.Logf("badge %s id %d category %s name %q, %d collision polygons", p.Name(), p.BadgeID, p.CategoryName(), p.DisplayName(1), len(polys))
			if dir := os.Getenv("BADGE_ARCADE_GALLERY_LIVE"); dir != "" {
				if w, err := os.Create(dir + "/custom_badge_64.png"); err == nil {
					png.Encode(w, p.Image64())
					w.Close()
				}
			}
		}
	}
	if !found[badgearcade.PrizePath(badgeArcadeCustomBadge)] || !found[badgearcade.CraneInstancePath(badgeArcadeCustomMachine)] {
		t.Fatal("custom files missing from the crane archive")
	}
	if !regexp.MustCompile(`(?s)<DateStartText>20261005<.{0,900}<RegexSetName>BonusStage</RegexSetName>.{0,900}<Value>` + badgeArcadeCustomMachine + `</Value>`).MatchString(schedule) {
		t.Fatal("today's BonusStage does not point at the custom machine")
	}
	if n := strings.Count(schedule, "<Value>"+badgeArcadeCustomMachine+"</Value>"); n != 2 {
		t.Fatalf("custom machine scheduled %d times, want 2 (training + hall)", n)
	}
}
