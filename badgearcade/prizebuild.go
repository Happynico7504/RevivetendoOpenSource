package badgearcade

// Building badges from ordinary images, matching what Nintendo's files look
// like (measured over the archived badges):
//   - the 128x128 colour texture is the badge, and the 64x64 / 32x32 images are
//     downscales of it (same framing);
//   - the shadow is a neutral grey (~90) silhouette, blurred wide (two
//     12 px box-blur passes) and shifted ~1 px down;
//   - collision polygons are in 128x128 texture pixels and trace the outline
//     with several convex polygons of up to 8 points.

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"sort"
)

// FitImage scales src (bilinear) to fit inside w x h, centred on bg.
func FitImage(src image.Image, w, h int, bg color.Color) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw == 0 || sh == 0 {
		return dst
	}
	scale := math.Min(float64(w)/float64(sw), float64(h)/float64(sh))
	tw, th := int(float64(sw)*scale+0.5), int(float64(sh)*scale+0.5)
	ox, oy := (w-tw)/2, (h-th)/2
	at := func(x, y int) color.NRGBA {
		x, y = min(x, sw-1), min(y, sh-1)
		return color.NRGBAModel.Convert(src.At(sb.Min.X+x, sb.Min.Y+y)).(color.NRGBA)
	}
	for y := 0; y < th; y++ {
		fy := math.Max((float64(y)+0.5)/scale-0.5, 0)
		y0 := int(fy)
		wy := fy - float64(y0)
		for x := 0; x < tw; x++ {
			fx := math.Max((float64(x)+0.5)/scale-0.5, 0)
			x0 := int(fx)
			wx := fx - float64(x0)
			c00, c10, c01, c11 := at(x0, y0), at(x0+1, y0), at(x0, y0+1), at(x0+1, y0+1)
			// Premultiplied mixing, so transparent pixels don't bleed colour.
			wts := [4]float64{(1 - wx) * (1 - wy), wx * (1 - wy), (1 - wx) * wy, wx * wy}
			var r, g, b, a float64
			for i, c := range [4]color.NRGBA{c00, c10, c01, c11} {
				ca := float64(c.A) * wts[i]
				r += float64(c.R) * ca
				g += float64(c.G) * ca
				b += float64(c.B) * ca
				a += ca
			}
			out := color.NRGBA{A: uint8(a + 0.5)}
			if a > 0 {
				out.R, out.G, out.B = uint8(r/a+0.5), uint8(g/a+0.5), uint8(b/a+0.5)
			}
			dst.SetNRGBA(ox+x, oy+y, out)
		}
	}
	return dst
}

// downscale averages factor x factor blocks (premultiplied).
func downscale(src *image.NRGBA, factor int) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx()/factor, b.Dy()/factor
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, bl, a float64
			for dy := 0; dy < factor; dy++ {
				for dx := 0; dx < factor; dx++ {
					c := src.NRGBAAt(x*factor+dx, y*factor+dy)
					ca := float64(c.A)
					r += float64(c.R) * ca
					g += float64(c.G) * ca
					bl += float64(c.B) * ca
					a += ca
				}
			}
			n := float64(factor * factor)
			out := color.NRGBA{A: uint8(a/n + 0.5)}
			if a > 0 {
				out.R, out.G, out.B = uint8(r/a+0.5), uint8(g/a+0.5), uint8(bl/a+0.5)
			}
			dst.SetNRGBA(x, y, out)
		}
	}
	return dst
}

// badgeShadow makes Nintendo-style shadow from a 128x128 colour texture.
func badgeShadow(col *image.NRGBA) *image.NRGBA {
	return badgeShadowParams(col, shadowRadius, shadowBoost)
}

// Fitted to 30 of Nintendo's shadows (two box-blur passes; TestShadowFit).
var shadowRadius, shadowBoost = 12, 1.0

func badgeShadowParams(col *image.NRGBA, radius int, boost float64) *image.NRGBA {
	const shiftY, grey = 1, 90
	w, h := col.Bounds().Dx(), col.Bounds().Dy()
	alpha := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if sy := y - shiftY; sy >= 0 {
				alpha[y*w+x] = float64(col.NRGBAAt(x, sy).A)
			}
		}
	}
	blur := func(in []float64, horizontal bool) []float64 {
		out := make([]float64, len(in))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				var sum float64
				for k := -radius; k <= radius; k++ {
					xx, yy := x, y
					if horizontal {
						xx += k
					} else {
						yy += k
					}
					if xx >= 0 && xx < w && yy >= 0 && yy < h {
						sum += in[yy*w+xx]
					}
				}
				out[y*w+x] = sum / float64(2*radius+1)
			}
		}
		return out
	}
	alpha = blur(blur(alpha, true), false)
	sh := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i, a := range alpha {
		v := math.Min(255, a*boost)
		sh.Pix[i*4], sh.Pix[i*4+1], sh.Pix[i*4+2], sh.Pix[i*4+3] = grey, grey, grey, uint8(v)
	}
	return sh
}

// AutoCollision traces the opaque area of a 128x128 texture with up to
// maxPolys convex polygons of at most 8 points, one per horizontal band.
func AutoCollision(col *image.NRGBA, maxPolys int) []Polygon {
	w, h := col.Bounds().Dx(), col.Bounds().Dy()
	top, bottom := -1, -1
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if col.NRGBAAt(x, y).A >= 128 {
				if top < 0 {
					top = y
				}
				bottom = y
				break
			}
		}
	}
	if top < 0 {
		return nil
	}
	if maxPolys < 1 {
		maxPolys = 1
	}
	bands := min(maxPolys, (bottom-top)/8+1)
	var polys []Polygon
	for b := 0; b < bands; b++ {
		y0 := top + (bottom-top+1)*b/bands
		y1 := top + (bottom-top+1)*(b+1)/bands // exclusive
		var pts [][2]float32
		for y := y0; y < y1; y++ {
			l, r := -1, -1
			for x := 0; x < w; x++ {
				if col.NRGBAAt(x, y).A >= 128 {
					if l < 0 {
						l = x
					}
					r = x
				}
			}
			if l >= 0 {
				pts = append(pts, [2]float32{float32(l), float32(y)}, [2]float32{float32(r + 1), float32(y)},
					[2]float32{float32(l), float32(y + 1)}, [2]float32{float32(r + 1), float32(y + 1)})
			}
		}
		if hull := reduceHull(convexHull(pts), 8); len(hull) >= 3 {
			polys = append(polys, hull)
		}
	}
	return polys
}

func convexHull(pts [][2]float32) Polygon {
	if len(pts) < 3 {
		return Polygon(pts)
	}
	p := append([][2]float32{}, pts...)
	sort.Slice(p, func(i, j int) bool { return p[i][0] < p[j][0] || p[i][0] == p[j][0] && p[i][1] < p[j][1] })
	cross := func(o, a, b [2]float32) float32 {
		return (a[0]-o[0])*(b[1]-o[1]) - (a[1]-o[1])*(b[0]-o[0])
	}
	var hull [][2]float32
	for pass := 0; pass < 2; pass++ {
		start := len(hull)
		for _, q := range p {
			for len(hull) >= start+2 && cross(hull[len(hull)-2], hull[len(hull)-1], q) <= 0 {
				hull = hull[:len(hull)-1]
			}
			hull = append(hull, q)
		}
		hull = hull[:len(hull)-1]
		for i, j := 0, len(p)-1; i < j; i, j = i+1, j-1 {
			p[i], p[j] = p[j], p[i]
		}
	}
	return Polygon(hull)
}

// reduceHull drops the vertices that change the area least until at most n
// remain (the result still covers the original closely).
func reduceHull(poly Polygon, n int) Polygon {
	p := append(Polygon{}, poly...)
	area := func(a, b, c [2]float32) float32 {
		return float32(math.Abs(float64((b[0]-a[0])*(c[1]-a[1])-(b[1]-a[1])*(c[0]-a[0])))) / 2
	}
	for len(p) > n {
		best, bestArea := 0, float32(math.MaxFloat32)
		for i := range p {
			a := area(p[(i+len(p)-1)%len(p)], p[i], p[(i+1)%len(p)])
			if a < bestArea {
				best, bestArea = i, a
			}
		}
		p = append(p[:best], p[best+1:]...)
	}
	return p
}

// PrizeSpec describes a single-tile badge to build.
type PrizeSpec struct {
	BadgeID  uint32
	FileName string // archive name without extension, e.g. "Pr_Custom_000001"
	Category string
	Names    [DisplayNameLanguages]string
	TitleID  *[8]byte // nil: launches nothing
	Image    image.Image
	// Collision overrides AutoCollision when set (128x128 texture pixels).
	Collision []Polygon
}

// BuildPrize makes a complete single-tile badge from an image.
func BuildPrize(spec PrizeSpec) *Prize {
	col := FitImage(spec.Image, 128, 128, color.Transparent)
	img64 := downscale(col, 2)
	img32 := downscale(col, 4)
	p := &Prize{
		Version: 3,
		BadgeID: spec.BadgeID,
		B0:      0,
		B4:      16,
		TilesW:  1,
		TilesH:  1,
		ScaleW:  1,
		ScaleH:  1,
	}
	p.SetName(spec.FileName)
	p.SetCategory(spec.Category)
	for i := range p.TitleID {
		p.TitleID[i] = 0xff
	}
	if spec.TitleID != nil {
		p.TitleID = *spec.TitleID
	}
	for i, n := range spec.Names {
		p.SetDisplayName(i, n)
	}
	p.Image = append(EncodeRGB565A4(img64, 64, 64), EncodeRGB565A4(img32, 32, 32)...)
	p.Texture = append(EncodeETC1A4(col, 128, 128), EncodeETC1A4(badgeShadow(col), 128, 128)...)
	polys := spec.Collision
	if polys == nil {
		polys = AutoCollision(col, 8)
	}
	p.Collision = EncodeCollision(polys)
	return p
}

// Image64 decodes the badge's 64x64 image.
func (p *Prize) Image64() *image.NRGBA {
	if len(p.Image) < RGB565A4Size(64, 64) {
		return image.NewNRGBA(image.Rect(0, 0, 64, 64))
	}
	return DecodeRGB565A4(p.Image, 64, 64)
}
