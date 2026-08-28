package main

import (
	"os"
	"strings"
	"testing"
)

// TestExtractMiiFromRealSwapdoodleNote verifies the LZ10/BPK1 decoder against
// a real captured note object (see extractMiiFromSwapdoodleNote's doc
// comment) - skips if the boss-capture sample isn't present (e.g. CI).
func TestExtractMiiFromRealSwapdoodleNote(t *testing.T) {
	path := "/nico-pretendo-bridge/boss-capture/s3relayupload_20260828-060415.158.file.bin"
	noteBytes, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("real sample not present: %v", err)
	}

	decompressed, err := lz10Decompress(noteBytes)
	if err != nil {
		t.Fatalf("lz10Decompress: %v", err)
	}
	if len(decompressed) != 57608 {
		t.Fatalf("decompressed length = %d, want 57608", len(decompressed))
	}

	blocks, err := parseBPK1Blocks(decompressed)
	if err != nil {
		t.Fatalf("parseBPK1Blocks: %v", err)
	}
	wantBlocks := map[string]int{
		"SHEET1": 1720, "COLSLT1": 396, "THUMB2": 2653, "STATIN1": 51876,
		"COMMON1": 64, "MIISTD1": 128, "DSTINF1": 512, "RCVINF1": 32,
	}
	for name, wantLen := range wantBlocks {
		got, ok := blocks[name]
		if !ok {
			t.Errorf("missing block %q", name)
			continue
		}
		if len(got) != wantLen {
			t.Errorf("block %q length = %d, want %d", name, len(got), wantLen)
		}
	}

	thumb := blocks["THUMB2"]
	if len(thumb) < 4 || thumb[0] != 0xFF || thumb[1] != 0xD8 {
		t.Errorf("THUMB2 doesn't start with a JPEG SOI marker: %x", thumb[:min(4, len(thumb))])
	}

	mii, ok := extractMiiFromSwapdoodleNote(noteBytes)
	if !ok {
		t.Fatal("extractMiiFromSwapdoodleNote returned ok=false")
	}
	if len(mii) != 96 {
		t.Fatalf("mii length = %d, want 96 (trimmed from MIISTD1's 128)", len(mii))
	}
	// The sender's PNID name is UTF-16LE-encoded inside MIISTD1 - confirmed
	// live against this exact sample (see extractMiiFromSwapdoodleNote doc).
	if !strings.Contains(string(mii), "N\x00i\x00c\x00o\x00") {
		t.Error("expected UTF-16LE 'Nico' not found in MIISTD1 block")
	}
}
