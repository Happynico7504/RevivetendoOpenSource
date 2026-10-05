package badgearcade

import (
	"image"
	"image/png"
	"os"
	"strings"
	"testing"
)

// TestBuildPrizeLikeNintendo rebuilds archived badges from their own 128x128
// colour texture and compares the result with Nintendo's file: layout, shadow
// and collision coverage.
func TestBuildPrizeLikeNintendo(t *testing.T) {
	n := 0
	for _, f := range testCraneFiles(t) {
		if !strings.HasSuffix(f.Name, ".prb.szs") || n >= 40 {
			continue
		}
		d, _ := DecompressYaz0(f.Data)
		orig, _ := ParsePrize(d)
		if len(orig.Texture) != PrbTextureSize || orig.TilesW*orig.TilesH != 1 {
			continue
		}
		n++
		col := DecodeETC1A4(orig.Texture[:0x4000], 128, 128)
		built := BuildPrize(PrizeSpec{BadgeID: orig.BadgeID, FileName: orig.Name(), Category: orig.CategoryName(), Image: col})
		back, err := ParsePrize(built.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if len(back.Image) != PrbImageSize || len(back.Texture) != PrbTextureSize || len(back.Tiles) != 0 ||
			back.Name() != orig.Name() || back.BadgeID != orig.BadgeID || back.TilesW != 1 || back.TilesH != 1 {
			t.Fatalf("%s: built badge has the wrong layout", f.Name)
		}

		sa := DecodeETC1A4(orig.Texture[0x4000:], 128, 128)
		sb := DecodeETC1A4(back.Texture[0x4000:], 128, 128)
		var diff float64
		for i := 3; i < len(sa.Pix); i += 4 {
			d := float64(sa.Pix[i]) - float64(sb.Pix[i])
			diff += d * d
		}
		if mse := diff / (128 * 128); mse > 4000 { // cosmetic; TestShadowFit checks the average
			t.Errorf("%s: shadow alpha MSE %.0f vs Nintendo's", f.Name, mse)
		}

		polys, err := back.CollisionPolygons()
		if err != nil || len(polys) == 0 || len(polys) > 8 {
			t.Fatalf("%s: auto collision gave %d polygons (err %v)", f.Name, len(polys), err)
		}
		if uncovered := uncoveredOpaque(col, polys); uncovered > 0.03 {
			t.Errorf("%s: %.1f%% of opaque pixels outside the collision", f.Name, uncovered*100)
		}

		if dir := os.Getenv("PRB_PREVIEW_DIR"); dir != "" && n <= 3 {
			for name, img := range map[string]image.Image{"nintendo_shadow": sa, "our_shadow": sb, "our_64": back.Image64()} {
				if fo, err := os.Create(dir + "/" + orig.Name() + "_" + name + ".png"); err == nil {
					png.Encode(fo, img)
					fo.Close()
				}
			}
		}
	}
	if n == 0 {
		t.Skip("no badges")
	}
}

// uncoveredOpaque returns the share of opaque pixels outside every polygon.
func uncoveredOpaque(img *image.NRGBA, polys []Polygon) float64 {
	inside := func(poly Polygon, x, y float32) bool {
		in := false
		for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
			a, b := poly[i], poly[j]
			if (a[1] > y) != (b[1] > y) && x < (b[0]-a[0])*(y-a[1])/(b[1]-a[1])+a[0] {
				in = !in
			}
		}
		return in
	}
	total, out := 0, 0
	b := img.Bounds()
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			if img.NRGBAAt(x, y).A < 128 {
				continue
			}
			total++
			hit := false
			for _, p := range polys {
				if inside(p, float32(x)+0.5, float32(y)+0.5) {
					hit = true
					break
				}
			}
			if !hit {
				out++
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(out) / float64(total)
}
