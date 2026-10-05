package main

import (
	"bytes"
	"image/png"
	"os"
	"strings"
	"testing"
)

// These tests use Nintendo's archived Badge Arcade content from
// badgeArcadeBossDataDir (kept out of the repo) and skip when it isn't there.

func loadBadgeArcadeData(t *testing.T, prefix string) (key, raw []byte, serial uint64, payloads []boss3DSPayload) {
	t.Helper()
	key, err := loadBoss3DSKey()
	if err != nil {
		t.Skipf("no 3DS BOSS key: %v", err)
	}
	raw, err = os.ReadFile(badgeArcadeBossDataDir + "/" + prefix + "_data_data_v131.dat.boss")
	if err != nil {
		t.Skipf("no archived content: %v", err)
	}
	serial, payloads, err = parseBoss3DS(key, raw)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestBoss3DSRebuildMatchesNintendo(t *testing.T) {
	key, raw, serial, payloads := loadBadgeArcadeData(t, "GB_en")
	out, err := buildBoss3DS(key, serial, payloads, raw[28:40])
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(raw) {
		t.Fatalf("size %d, want %d", len(out), len(raw))
	}
	// Identical except the two zeroed RSA signatures (content header 50..306,
	// payload header 60..316) and content header byte 0 (0x80 vs Nintendo's 0x00).
	key2, _ := loadBoss3DSKey()
	_, again, err := parseBoss3DS(key2, out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again[0].Content, payloads[0].Content) || again[0].NsDataID != payloads[0].NsDataID {
		t.Fatal("payload changed in rebuild")
	}
}

func TestSARCRebuildMatchesNintendo(t *testing.T) {
	_, _, _, payloads := loadBadgeArcadeData(t, "GB_en")
	content := payloads[0].Content
	entries, err := parseSARC(content)
	if err != nil {
		t.Fatal(err)
	}
	if got := buildSARC(entries, 4, 16); !bytes.Equal(got, content) {
		t.Fatalf("outer archive rebuild differs (%d vs %d bytes)", len(got), len(content))
	}
	posts := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name, "post/") {
			continue
		}
		posts++
		inner, err := parseSARC(e.Data)
		if err != nil {
			t.Fatal(err)
		}
		if got := buildSARC(inner, 128, 128); !bytes.Equal(got, e.Data) {
			t.Fatalf("%s rebuild differs", e.Name)
		}
	}
	if posts != 20 {
		t.Fatalf("found %d posts, want 20", posts)
	}
}

func TestETC1A4RoundTrip(t *testing.T) {
	_, _, _, payloads := loadBadgeArcadeData(t, "GB_en")
	entries, _ := parseSARC(payloads[0].Content)
	var mii []byte
	for _, e := range entries {
		if e.Name == "post/AYMHAAADAAB2V0fYaAyBYg.sarc" { // "Arthur"
			inner, _ := parseSARC(e.Data)
			for _, f := range inner {
				if f.Name == "Mii.Etc1_a4" {
					mii = f.Data
				}
			}
		}
	}
	if len(mii) != 128*128 {
		t.Fatalf("Mii texture %d bytes", len(mii))
	}
	img := decodeETC1A4(mii, 128, 128)
	if out := os.Getenv("ETC1A4_DUMP"); out != "" {
		var buf bytes.Buffer
		png.Encode(&buf, img)
		os.WriteFile(out, buf.Bytes(), 0o644)
	}
	back := decodeETC1A4(encodeETC1A4(img, 128, 128), 128, 128)
	var sum float64
	for i := range img.Pix {
		d := float64(img.Pix[i]) - float64(back.Pix[i])
		sum += d * d
	}
	if mse := sum / float64(len(img.Pix)); mse > 60 {
		t.Fatalf("round-trip MSE %.1f too high", mse)
	}
}
