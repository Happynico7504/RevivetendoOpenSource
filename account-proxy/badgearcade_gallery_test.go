package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
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
	out, ids, err := generateBadgeArcadeGallery(ctx, "GB_en", day, posts, nil, badgeArcadeStableVariant, 0)
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

func TestBadgeArcadeDeployments(t *testing.T) {
	loadBadgeArcadeData(t, "GB_en") // skips without key/content
	img := image.NewNRGBA(image.Rect(0, 0, 64, 96))
	for y := 8; y < 88; y++ {
		for x := 8; x < 56; x++ {
			img.SetNRGBA(x, y, color.NRGBA{40, 120, 200, 255})
		}
	}
	var art bytes.Buffer
	png.Encode(&art, img)
	badge := func(id int64, name string) badgeArcadeDeploymentBadge {
		b := badgeArcadeDeploymentBadge{CreationID: id, Art: art.Bytes()}
		b.Spec.Names[1] = name
		return b
	}
	deps := []badgeArcadeDeployment{
		{ID: 1, Template: "Pokemon_091", Slot: "hall", Badges: []badgeArcadeDeploymentBadge{badge(11, "One"), badge(12, "Two")}},
		{ID: 2, Template: "Pokemon_091", Slot: "training", Badges: []badgeArcadeDeploymentBadge{badge(11, "One")}},
		{ID: 3, Template: "NoSuchTemplate", Slot: "hall", Badges: []badgeArcadeDeploymentBadge{badge(13, "Three")}},
	}
	// A Friday: deployments only show Friday to Sunday.
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	out, _, err := generateBadgeArcadeGallery(context.Background(), "GB_en", day, nil, deps, badgeArcadeStableVariant, 2)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := loadBoss3DSKey()
	_, payloads, err := badgearcade.ParseBOSS(key, out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := payloads[0].NsDataID, badgeArcadePackageNsDataID(badgeArcadeStableVariant, day, 2); got != want {
		t.Fatalf("ns_data_id %d, want %d", got, want)
	}
	entries, _ := badgearcade.ParseSARC(payloads[0].Content)
	wi, _ := badgearcade.WeeklyArchiveIndex(entries)
	weekly, _ := badgearcade.ParseSARC(entries[wi].Data)
	files := map[string][]byte{}
	for _, w := range weekly {
		files[w.Name] = w.Data
	}
	for _, id := range []int64{11, 12} {
		p, err := badgearcade.ParsePrizeFile(files[badgearcade.PrizePath(badgeArcadeDeployedBadgeName(id))])
		if err != nil || p.BadgeID != uint32(badgeArcadeCustomBadgeBase+id) {
			t.Fatalf("badge %d: %v", id, err)
		}
	}
	if _, ok := files[badgearcade.PrizePath(badgeArcadeDeployedBadgeName(13))]; ok {
		t.Fatal("badge of the unbuildable deployment was added")
	}
	hall, err := badgearcade.ParseCraneInstanceFile(files[badgearcade.CraneInstancePath(badgeArcadeDeployedMachineName(1))])
	if err != nil || len(hall.Prizes) != 2 {
		t.Fatalf("hall machine: %v", err)
	}
	var schedule string
	for _, e := range entries {
		if e.Name == "Schedule.xml" {
			schedule = string(e.Data)
		}
	}
	if strings.Count(schedule, "<Value>"+badgeArcadeDeployedMachineName(1)+"</Value>") != 1 ||
		strings.Count(schedule, "<Value>"+badgeArcadeDeployedMachineName(2)+"</Value>") != 1 {
		t.Fatal("deployments not scheduled exactly once each")
	}
	if !regexp.MustCompile(`(?s)<DateStartText>20261009<.{0,900}<RegexSetName>BonusStage</RegexSetName>.{0,900}<Value>` + badgeArcadeDeployedMachineName(2) + `</Value>`).MatchString(schedule) {
		t.Fatal("training deployment is not today's BonusStage")
	}
}

// TestBadgeArcadeDeploymentCrossRegion builds the JPN package with a
// deployment whose template only exists in the EUR archive.
func TestBadgeArcadeDeploymentCrossRegion(t *testing.T) {
	loadBadgeArcadeData(t, "JP_ja") // skips without key/content
	jp, err := loadBadgeArcadeRegion("JP_ja")
	if err != nil {
		t.Fatal(err)
	}
	const tmpl = "Animal_050" // EUR only; JPN lacks 4 of its 6 parts
	if jp.hasWeekly(badgearcade.CraneInstancePath(tmpl)) {
		t.Skip("template exists in JPN")
	}
	img := image.NewNRGBA(image.Rect(0, 0, 48, 48))
	for i := range img.Pix {
		img.Pix[i] = 230
	}
	var art bytes.Buffer
	png.Encode(&art, img)
	b := badgeArcadeDeploymentBadge{CreationID: 21, Art: art.Bytes()}
	b.Spec.Names[1] = "Cross"
	deps := []badgeArcadeDeployment{{ID: 7, Template: tmpl, Slot: "hall", Badges: []badgeArcadeDeploymentBadge{b}}}
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	out, _, err := generateBadgeArcadeGallery(context.Background(), "JP_ja", day, nil, deps, badgeArcadeStableVariant, 0)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := loadBoss3DSKey()
	_, payloads, err := badgearcade.ParseBOSS(key, out)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := badgearcade.ParseSARC(payloads[0].Content)
	wi, _ := badgearcade.WeeklyArchiveIndex(entries)
	weekly, _ := badgearcade.ParseSARC(entries[wi].Data)
	files := map[string]bool{}
	for _, w := range weekly {
		files[w.Name] = true
	}
	m, err := func() (*badgearcade.CraneInstance, error) {
		for _, w := range weekly {
			if w.Name == badgearcade.CraneInstancePath(badgeArcadeDeployedMachineName(7)) {
				return badgearcade.ParseCraneInstanceFile(w.Data)
			}
		}
		return nil, fmt.Errorf("machine missing in the JPN archive")
	}()
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range badgearcade.MachinePartPaths(m) {
		if !files[part.Path] && !jp.has(part.Path) {
			t.Fatalf("part %s neither copied nor already in JPN", part.Path)
		}
	}
	var schedule string
	for _, e := range entries {
		if e.Name == "Schedule.xml" {
			schedule = string(e.Data)
		}
	}
	if !strings.Contains(schedule, "<Value>"+badgeArcadeDeployedMachineName(7)+"</Value>") {
		t.Fatal("machine not scheduled in JPN")
	}
}

// todaysLineup returns the hall and training machines a package schedules for
// day, and the package's weekly archive file names.
func todaysLineup(t *testing.T, pkg []byte, day time.Time) (hall []string, training string, weekly map[string]bool) {
	t.Helper()
	key, _ := loadBoss3DSKey()
	_, payloads, err := badgearcade.ParseBOSS(key, pkg)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := badgearcade.ParseSARC(payloads[0].Content)
	wi, _ := badgearcade.WeeklyArchiveIndex(entries)
	files, _ := badgearcade.ParseSARC(entries[wi].Data)
	weekly = map[string]bool{}
	for _, f := range files {
		weekly[f.Name] = true
	}
	valueRe := regexp.MustCompile(`<Value>([^<]*)</Value>`)
	for _, e := range entries {
		if e.Name != "Schedule.xml" {
			continue
		}
		for _, it := range scheduleItemRe.FindAllString(string(e.Data), -1) {
			if !strings.Contains(it, "<DateStartText>"+day.Format("20060102")+"<") {
				continue
			}
			v := valueRe.FindStringSubmatch(it)[1]
			switch {
			case strings.Contains(it, ">DefaultStage<"):
				hall = append(hall, v)
			case strings.Contains(it, ">BonusStage<"):
				training = v
			}
		}
	}
	return
}

// TestBadgeArcadeWeeklyLineup checks the weekly lineup: Monday to Thursday 30
// official machines a day, no repeats within the week, the same in every
// region and present in each package; Friday to Sunday only deployments.
func TestBadgeArcadeWeeklyLineup(t *testing.T) {
	loadBadgeArcadeData(t, "GB_en") // skips without key/content
	pool, err := badgeArcadeOfficialPool()
	if err != nil {
		t.Fatal(err)
	}
	monday := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for d := 0; d < 7; d++ {
		hall, training, err := badgeArcadeOfficialForDay(monday.AddDate(0, 0, d))
		if err != nil {
			t.Fatal(err)
		}
		if len(hall) != badgeArcadeHallSlots {
			t.Fatalf("day %d: %d hall machines", d, len(hall))
		}
		for _, m := range append(hall, training) {
			if seen[m.Name] {
				t.Fatalf("day %d: %s repeats within the week", d, m.Name)
			}
			seen[m.Name] = true
		}
	}
	nextHall, _, _ := badgeArcadeOfficialForDay(monday.AddDate(0, 0, 7))
	if thisHall, _, _ := badgeArcadeOfficialForDay(monday); nextHall[0].Name == thisHall[0].Name {
		t.Fatal("next week has the same lineup")
	}
	t.Logf("official pool: %d machines", len(pool))

	wednesday, friday := monday.AddDate(0, 0, 2), monday.AddDate(0, 0, 4)
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 8; y < 56; y++ {
		for x := 8; x < 56; x++ {
			img.SetNRGBA(x, y, color.NRGBA{200, 40, 40, 255})
		}
	}
	var art bytes.Buffer
	png.Encode(&art, img)
	b := badgeArcadeDeploymentBadge{CreationID: 21, Art: art.Bytes()}
	b.Spec.Names[1] = "Lineup"
	deps := []badgeArcadeDeployment{{ID: 5, Template: "Pokemon_091", Slot: "hall", Badges: []badgeArcadeDeploymentBadge{b}}}
	var first []string
	for _, prefix := range badgeArcadeGalleryPrefixes {
		target, _ := loadBadgeArcadeRegion(prefix)
		pkg, _, err := generateBadgeArcadeGallery(context.Background(), prefix, wednesday, nil, deps, badgeArcadeExperimentalVariant, 0)
		if err != nil {
			t.Fatal(err)
		}
		hall, training, weekly := todaysLineup(t, pkg, wednesday)
		if len(hall) != badgeArcadeHallSlots || training == "" {
			t.Fatalf("%s Wednesday: %d hall machines, training %q", prefix, len(hall), training)
		}
		if first == nil {
			first = append(hall, training)
		} else if strings.Join(append(hall, training), ",") != strings.Join(first, ",") {
			t.Fatalf("%s: lineup differs from %s", prefix, badgeArcadeGalleryPrefixes[0])
		}
		for _, n := range append(hall, training) {
			if strings.HasPrefix(n, "Rvt_") {
				t.Fatalf("%s: deployment %s scheduled on a Wednesday", prefix, n)
			}
			if !target.has(badgearcade.CraneInstancePath(n)) && !weekly[badgearcade.CraneInstancePath(n)] {
				t.Fatalf("%s: %s scheduled but not in the package", prefix, n)
			}
		}
		t.Logf("%s Wednesday: %d bytes", prefix, len(pkg))

		pkg, _, err = generateBadgeArcadeGallery(context.Background(), prefix, friday, nil, deps, badgeArcadeExperimentalVariant, 0)
		if err != nil {
			t.Fatal(err)
		}
		hall, training, _ = todaysLineup(t, pkg, friday)
		if len(hall) != 1 || hall[0] != badgeArcadeDeployedMachineName(5) || training == "" {
			t.Fatalf("%s Friday: hall %v, training %q", prefix, hall, training)
		}
		if hall, _, _ := todaysLineup(t, mustGallery(t, prefix, friday), friday); len(hall) != badgeArcadeHallSlots {
			t.Fatalf("%s Friday without deployments: %d hall machines, want official ones", prefix, len(hall))
		}
	}
}

func mustGallery(t *testing.T, prefix string, day time.Time) []byte {
	t.Helper()
	pkg, _, err := generateBadgeArcadeGallery(context.Background(), prefix, day, nil, nil, badgeArcadeExperimentalVariant, 0)
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}
