package main

import (
	"context"
	"fmt"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func badgeArcadeTestSchedule(t *testing.T) string {
	_, _, _, payloads := loadBadgeArcadeData(t, "GB_en")
	entries, err := parseSARC(payloads[0].Content)
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
	out, ids, err := generateBadgeArcadeGallery(ctx, "GB_en", day, posts)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := loadBoss3DSKey()
	_, payloads, err := parseBoss3DS(key, out)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("package %d bytes, ns_data_id %d, posts %v", len(out), payloads[0].NsDataID, ids)
	entries, err := parseSARC(payloads[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name, "post/") {
			continue
		}
		inner, err := parseSARC(e.Data)
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
					png.Encode(w, decodeETC1A4(f.Data, 128, 128))
					w.Close()
				}
			}
		}
	}
}
