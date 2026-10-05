package badgearcade

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// These tests use Nintendo's archived Badge Arcade content, which is kept out
// of the repo: BADGE_ARCADE_DATA (default /home/nico/badgearcade-boss-data) and
// the key in BOSS3DS_KEY_FILE (default /home/nico/boss3ds_key.hex). They skip
// when either is missing.

func testKey(t *testing.T) []byte {
	t.Helper()
	path := os.Getenv("BOSS3DS_KEY_FILE")
	if path == "" {
		path = "/home/nico/boss3ds_key.hex"
	}
	key, err := LoadBOSSKey(path)
	if err != nil {
		t.Skipf("no 3DS BOSS key: %v", err)
	}
	return key
}

func testDataDir() string {
	if d := os.Getenv("BADGE_ARCADE_DATA"); d != "" {
		return d
	}
	return "/home/nico/badgearcade-boss-data"
}

// testArchive decrypts one of Nintendo's packages and returns its SARC entries.
func testArchive(t *testing.T, file string) (raw []byte, serial uint64, payloads []BOSSPayload, entries []SARCEntry) {
	t.Helper()
	key := testKey(t)
	raw, err := os.ReadFile(testDataDir() + "/" + file)
	if err != nil {
		t.Skipf("no archived content: %v", err)
	}
	serial, payloads, err = ParseBOSS(key, raw)
	if err != nil {
		t.Fatal(err)
	}
	entries, err = ParseSARC(payloads[0].Content)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// testCraneFiles returns every .szs crane-archive file from the base library
// (allbadge) and the weekly archive inside data_v131.
func testCraneFiles(t *testing.T) []SARCEntry {
	_, _, _, all := testArchive(t, "GB_en_data_allbadge_v131.dat.boss")
	files := append([]SARCEntry{}, all...)
	_, _, _, data := testArchive(t, "GB_en_data_data_v131.dat.boss")
	for _, e := range data {
		if strings.HasPrefix(e.Name, "sharc/") {
			week, err := ParseSARC(e.Data)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, week...)
		}
	}
	return files
}

func TestBOSSRebuild(t *testing.T) {
	key := testKey(t)
	raw, _, payloads, _ := testArchive(t, "GB_en_data_data_v131.dat.boss")
	serial, _, _ := ParseBOSS(key, raw)
	out, err := BuildBOSS(key, serial, payloads, raw[28:40])
	if err != nil {
		t.Fatal(err)
	}
	_, again, err := ParseBOSS(key, out)
	if err != nil || len(out) != len(raw) || !bytes.Equal(again[0].Content, payloads[0].Content) {
		t.Fatalf("rebuild differs: err=%v len %d vs %d", err, len(out), len(raw))
	}
}

func TestSARCRebuild(t *testing.T) {
	_, _, payloads, entries := testArchive(t, "GB_en_data_data_v131.dat.boss")
	if got := BuildSARC(entries, 4, 16); !bytes.Equal(got, payloads[0].Content) {
		t.Fatal("outer archive rebuild differs")
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name, "post/") {
			inner, _ := ParseSARC(e.Data)
			if !bytes.Equal(BuildSARC(inner, 128, 128), e.Data) {
				t.Fatalf("%s rebuild differs", e.Name)
			}
		}
	}
}

func TestPrizeCorpus(t *testing.T) {
	n := 0
	for _, f := range testCraneFiles(t) {
		if !strings.HasSuffix(f.Name, ".prb.szs") {
			continue
		}
		d, err := DecompressYaz0(f.Data)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		p, err := ParsePrize(d)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if got := p.Bytes(); !bytes.Equal(got, d) {
			for i := range got {
				if i >= len(d) || got[i] != d[i] {
					t.Fatalf("%s: rebuild differs at 0x%x (len %d vs %d)", f.Name, i, len(got), len(d))
				}
			}
			t.Fatalf("%s: rebuild length %d vs %d", f.Name, len(got), len(d))
		}
		polys, err := p.CollisionPolygons()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if len(p.Collision) > 0 && !bytes.Equal(EncodeCollision(polys), p.Collision) {
			t.Fatalf("%s: collision re-encode differs", f.Name)
		}
		n++
	}
	t.Logf("%d badges rebuilt byte-exact", n)
	if n < 5000 {
		t.Fatalf("only %d badges found", n)
	}
}

func TestCraneInstanceCorpus(t *testing.T) {
	n := 0
	for _, f := range testCraneFiles(t) {
		if !strings.HasSuffix(f.Name, ".cib.szs") {
			continue
		}
		d, err := DecompressYaz0(f.Data)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		c, err := ParseCraneInstance(d)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		got, err := c.Bytes()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		if !bytes.Equal(got, d) {
			for i := range got {
				if got[i] != d[i] {
					t.Fatalf("%s: rebuild differs at 0x%x", f.Name, i)
				}
			}
		}
		n++
	}
	t.Logf("%d crane machines rebuilt byte-exact", n)
	if n < 900 {
		t.Fatalf("only %d crane machines found", n)
	}
}

func TestYaz0RoundTrip(t *testing.T) {
	files := testCraneFiles(t)
	for i, f := range files {
		if i%25 != 0 || !strings.HasSuffix(f.Name, ".szs") {
			continue
		}
		d, err := DecompressYaz0(f.Data)
		if err != nil {
			t.Fatal(err)
		}
		back, err := DecompressYaz0(CompressYaz0(d))
		if err != nil || !bytes.Equal(back, d) {
			t.Fatalf("%s: Yaz0 round trip failed: %v", f.Name, err)
		}
	}
}
