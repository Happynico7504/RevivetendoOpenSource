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
		if !ok || len(got) != 1 {
			t.Errorf("block %q: got %d page(s), want exactly 1", name, len(got))
			continue
		}
		if len(got[0]) != wantLen {
			t.Errorf("block %q length = %d, want %d", name, len(got[0]), wantLen)
		}
	}

	thumb := blocks["THUMB2"][0]
	if len(thumb) < 4 || thumb[0] != 0xFF || thumb[1] != 0xD8 {
		t.Errorf("THUMB2 doesn't start with a JPEG SOI marker: %x", thumb[:min(4, len(thumb))])
	}

	thumbs, ok := extractThumbnailsFromSwapdoodleNote(noteBytes)
	if !ok || len(thumbs) != 1 {
		t.Fatalf("extractThumbnailsFromSwapdoodleNote: ok=%v, got %d page(s), want 1", ok, len(thumbs))
	}
	if len(thumbs[0]) != 2653 {
		t.Errorf("extracted thumbnail length = %d, want 2653", len(thumbs[0]))
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

// TestMultiPageSwapdoodleNote verifies parseBPK1Blocks/extractThumbnailsFromSwapdoodleNote
// against a real 4-page note - the specific case that would silently lose
// pages with a plain map[string][]byte (see parseBPK1Blocks' doc comment).
func TestMultiPageSwapdoodleNote(t *testing.T) {
	path := "/nico-pretendo-bridge/boss-capture/s3relayupload_20260825-143138.061.file.bin"
	noteBytes, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("real sample not present: %v", err)
	}

	decompressed, err := lz10Decompress(noteBytes)
	if err != nil {
		t.Fatalf("lz10Decompress: %v", err)
	}
	blocks, err := parseBPK1Blocks(decompressed)
	if err != nil {
		t.Fatalf("parseBPK1Blocks: %v", err)
	}
	if got := len(blocks["SHEET1"]); got != 4 {
		t.Errorf("SHEET1 pages = %d, want 4", got)
	}
	if got := len(blocks["THUMB2"]); got != 4 {
		t.Errorf("THUMB2 pages = %d, want 4", got)
	}
	// MIISTD1 stays sender-level (one block) even on a multi-page note.
	if got := len(blocks["MIISTD1"]); got != 1 {
		t.Errorf("MIISTD1 pages = %d, want 1", got)
	}

	thumbs, ok := extractThumbnailsFromSwapdoodleNote(noteBytes)
	if !ok || len(thumbs) != 4 {
		t.Fatalf("extractThumbnailsFromSwapdoodleNote: ok=%v, got %d page(s), want 4", ok, len(thumbs))
	}
	for i, thumb := range thumbs {
		if len(thumb) < 4 || thumb[0] != 0xFF || thumb[1] != 0xD8 {
			t.Errorf("page %d thumbnail doesn't start with a JPEG SOI marker: %x", i, thumb[:min(4, len(thumb))])
		}
	}
}
