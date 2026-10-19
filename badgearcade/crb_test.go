package badgearcade

import (
	"bytes"
	"image"
	"image/color"
	"strings"
	"testing"
)

// Every Nintendo stage parses and serialises byte-identically.
func TestCraneStageCorpus(t *testing.T) {
	n := 0
	for _, e := range testCraneFiles(t) {
		if !strings.HasSuffix(e.Name, ".crb.szs") {
			continue
		}
		raw, err := DecompressYaz0(e.Data)
		if err != nil {
			t.Fatal(err)
		}
		s, err := ParseCraneStage(raw)
		if err != nil {
			t.Fatalf("%s: %v", e.Name, err)
		}
		if CraneStagePath(s.Name) != e.Name {
			t.Errorf("%s: name %q", e.Name, s.Name)
		}
		back, err := s.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(back, raw) {
			t.Errorf("%s: rebuild differs", e.Name)
		}
		n++
	}
	if n < 300 {
		t.Fatalf("only %d stages", n)
	}
}

func TestBuildCraneStage(t *testing.T) {
	// Left half red, right half blue, 800x480 (scaled down by 2).
	src := image.NewNRGBA(image.Rect(0, 0, 800, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 800; x++ {
			c := color.NRGBA{200, 30, 30, 255}
			if x >= 400 {
				c = color.NRGBA{30, 30, 200, 255}
			}
			src.SetNRGBA(x, y, c)
		}
	}
	s, err := BuildCraneStage("CrSt_Rvt_Test", src, color.NRGBA{0, 160, 0, 255})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 0x10080 {
		t.Fatalf("size %#x", len(raw))
	}
	back, err := ParseCraneStage(raw)
	if err != nil || back.Name != "CrSt_Rvt_Test" {
		t.Fatalf("reparse: %v %q", err, back.Name)
	}
	near := func(c color.NRGBA, r, g, b int) bool {
		d := func(a uint8, b int) int { return max(int(a)-b, b-int(a)) }
		return d(c.R, r) < 24 && d(c.G, g) < 24 && d(c.B, b) < 24
	}
	scr := back.Screen()
	if c := scr.NRGBAAt(50, 120); !near(c, 200, 30, 30) {
		t.Errorf("left of screen = %v", c)
	}
	if c := scr.NRGBAAt(350, 120); !near(c, 30, 30, 200) {
		t.Errorf("right of screen = %v", c)
	}
	atlas := back.Image()
	if c := atlas.NRGBAAt(StageScreenX-2, 0); !near(c, 200, 30, 30) {
		t.Errorf("margin = %v", c)
	}
	if c := atlas.NRGBAAt(60, 100); !near(c, 255, 255, 255) {
		t.Errorf("unused area = %v", c)
	}
	if c := atlas.NRGBAAt(8, 100); c.G < 120 || c.R > 40 {
		t.Errorf("strip = %v", c)
	}
}
