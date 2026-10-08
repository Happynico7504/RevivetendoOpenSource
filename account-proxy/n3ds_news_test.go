package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
	"unicode/utf16"

	"github.com/Happynico7504/badgearcade"
)

func readUTF16Z(b []byte) string {
	var u []uint16
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// TestBuildN3DSNewsMatchesNintendo rebuilds Nintendo's own notification from
// Swapdoodle's nt1 ring file and expects identical bytes.
func TestBuildN3DSNewsMatchesNintendo(t *testing.T) {
	key, err := loadBoss3DSKey()
	if err != nil {
		t.Skipf("no 3DS BOSS key: %v", err)
	}
	raw, err := os.ReadFile(swapdoodleRingDataDir + "/nt1.boss")
	if err != nil {
		t.Skipf("no ring data: %v", err)
	}
	_, payloads, err := badgearcade.ParseBOSS(key, raw)
	if err != nil {
		t.Fatal(err)
	}
	var real []byte
	for _, p := range payloads {
		if p.ProgramID == n3dsNewsTitleID {
			if p.ContentDataType != n3dsNewsContentDataType {
				t.Fatalf("content type %#x", p.ContentDataType)
			}
			real = p.Content
		}
	}
	if len(real) != n3dsNewsContentSize {
		t.Fatalf("notification is %d bytes", len(real))
	}
	title, msg := readUTF16Z(real[0x20:0x60]), readUTF16Z(real[0x60:])
	launch := binary.LittleEndian.Uint64(real[0x08:])
	built := buildN3DSNews(title, msg, launch)
	if !bytes.Equal(built, real) {
		for i := range built {
			if built[i] != real[i] {
				t.Fatalf("differs at %#x: built %x, Nintendo %x (title %q, launch %x)", i, built[i:i+8], real[i:i+8], title, launch)
			}
		}
	}
	t.Logf("rebuilt %q (launch %016x) byte-exact", title, launch)

	// And the carrier: nt1 with our notifications appended still parses, with
	// Nintendo's payload first and ours after it.
	extra := badgearcade.BOSSPayload{ProgramID: n3dsNewsTitleID, ContentDataType: n3dsNewsContentDataType, NsDataID: n3dsNewsNsDataIDBase + 1, Version: 1, Content: buildN3DSNews("Test", "Hello", 0)}
	serial, base, _ := badgearcade.ParseBOSS(key, raw)
	out, err := badgearcade.BuildBOSS(key, serial, append(base, extra), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, back, err := badgearcade.ParseBOSS(key, out)
	if err != nil || len(back) != 2 || back[1].NsDataID != extra.NsDataID {
		t.Fatalf("carrier round trip: %v (%d payloads)", err, len(back))
	}
}
