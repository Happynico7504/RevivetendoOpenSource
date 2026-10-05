package badgearcade

import (
	"strings"
	"testing"
)

// TestShadowFit checks badgeShadow's parameters against Nintendo's shadows:
// they must beat every other radius/boost tried here.
func TestShadowFit(t *testing.T) {
	var cols, shs [][]byte
	for _, f := range testCraneFiles(t) {
		if !strings.HasSuffix(f.Name, ".prb.szs") || len(cols) >= 30 {
			continue
		}
		d, _ := DecompressYaz0(f.Data)
		p, _ := ParsePrize(d)
		if len(p.Texture) == PrbTextureSize && p.TilesW*p.TilesH == 1 {
			cols = append(cols, p.Texture[:0x4000])
			shs = append(shs, p.Texture[0x4000:])
		}
	}
	mse := func(radius int, boost float64) float64 {
		var sum float64
		for i := range cols {
			ours := badgeShadowParams(DecodeETC1A4(cols[i], 128, 128), radius, boost)
			ref := DecodeETC1A4(shs[i], 128, 128)
			for k := 3; k < len(ref.Pix); k += 4 {
				d := float64(ref.Pix[k]) - float64(ours.Pix[k])
				sum += d * d
			}
		}
		return sum / float64(len(cols)*128*128)
	}
	chosen := mse(shadowRadius, shadowBoost)
	for _, r := range []int{4, 8, 16, 20} {
		for _, b := range []float64{0.9, 1.0, 1.2, 1.6} {
			if m := mse(r, b); m < chosen {
				t.Errorf("radius %d boost %.1f fits better (%.0f < %.0f)", r, b, m, chosen)
			}
		}
	}
	t.Logf("shadow alpha MSE vs Nintendo: %.0f", chosen)
}
