package badgearcade

// Drawing a machine (.cib) the way the game lays it out, for editor and review
// previews. Coordinates, worked out by rendering Nintendo's machines (badges
// rest on floors, crates sit on ledges): a placement's X/Y is the centre of
// the object in top-screen pixels, origin top-left, y down; Rotation is in
// degrees, clockwise; ScaleW/ScaleH scale the object's texture (badges from a
// 128-pixel base per tile, obstacles and attachments from their own texture
// size). Placements may lie partly or fully off-screen.

import (
	"image"
	"image/color"
	"image/draw"
	"math"
)

// MachineLayer says what a drawn object is, for editors that make the
// objects selectable.
type MachineLayer int

const (
	LayerFixed MachineLayer = iota
	LayerAttachment
	LayerPrize
)

// MachineObject is one placed object with its decoded image.
type MachineObject struct {
	Layer     MachineLayer
	Name      string
	Placement Placement
	Image     *image.NRGBA
	Polygons  []Polygon // collision, in Image pixels
	BaseScale float64   // multiply ScaleW/ScaleH by this to get pixels per Image pixel
}

// MachineObjects decodes every placed object of a machine, back to front
// (obstacles, attachments, badges). Parts src can't provide are skipped.
func MachineObjects(c *CraneInstance, src FileLookup) []MachineObject {
	var out []MachineObject
	for _, p := range c.MachineFixed {
		if int(p.Index) >= len(c.FixedObjects) {
			continue
		}
		name := c.FixedObjects[p.Index]
		if d, ok := src(FixedObjectPath(name)); ok {
			if f, err := ParseFixedObjectFile(d); err == nil {
				polys, _ := f.Polygons()
				out = append(out, MachineObject{Layer: LayerFixed, Name: name, Placement: p, Image: f.Image(), Polygons: polys, BaseScale: 1})
			}
		}
	}
	for _, p := range c.MachineAttachments {
		if int(p.Index) >= len(c.Attachments) {
			continue
		}
		name := c.Attachments[p.Index]
		if d, ok := src(AttachmentPath(name)); ok {
			if a, err := ParseAttachmentFile(d); err == nil {
				polys, _ := a.Polygons()
				out = append(out, MachineObject{Layer: LayerAttachment, Name: name, Placement: p, Image: a.Image(), Polygons: polys, BaseScale: 1})
			}
		}
	}
	for _, p := range c.MachinePrizes {
		if int(p.Index) >= len(c.Prizes) {
			continue
		}
		name := c.Prizes[p.Index]
		d, ok := src(PrizePath(name))
		if !ok {
			continue
		}
		pr, err := ParsePrizeFile(d)
		if err != nil {
			continue
		}
		img := pr.Image64()
		if len(pr.Texture) >= 0x4000 {
			img = DecodeETC1A4(pr.Texture[:0x4000], 128, 128)
		}
		tiles := float64(max(pr.TilesW, 1))
		polys, _ := pr.CollisionPolygons()
		out = append(out, MachineObject{Layer: LayerPrize, Name: name, Placement: p, Image: img, Polygons: polys,
			BaseScale: 128 * tiles / float64(img.Bounds().Dx())})
	}
	return out
}

// RenderMachine draws a machine on its stage as the 400x240 top screen. A
// missing stage leaves a grey background.
func RenderMachine(c *CraneInstance, src FileLookup) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, StageScreenW, StageScreenH))
	draw.Draw(out, out.Bounds(), &image.Uniform{color.NRGBA{0xd0, 0xd4, 0xdc, 0xff}}, image.Point{}, draw.Src)
	if d, ok := src(CraneStagePath(c.Crane)); ok {
		if st, err := ParseCraneStageFile(d); err == nil {
			draw.Draw(out, out.Bounds(), st.Screen(), image.Point{}, draw.Src)
		}
	}
	for _, o := range MachineObjects(c, src) {
		p := o.Placement
		DrawPlaced(out, o.Image, float64(p.X), float64(p.Y), float64(p.ScaleW)*o.BaseScale, float64(p.ScaleH)*o.BaseScale, float64(p.Rotation))
	}
	return out
}

// DrawPlaced draws src onto dst centred at (cx, cy), scaled by (sx, sy) and
// rotated clockwise by deg degrees, blending by alpha (nearest-neighbour).
func DrawPlaced(dst *image.NRGBA, src *image.NRGBA, cx, cy, sx, sy, deg float64) {
	if sx == 0 || sy == 0 {
		return
	}
	w, h := float64(src.Bounds().Dx()), float64(src.Bounds().Dy())
	a := deg * math.Pi / 180
	cs, sn := math.Cos(a), math.Sin(a)
	r := math.Hypot(w*math.Abs(sx), h*math.Abs(sy))/2 + 1
	b := dst.Bounds()
	for y := max(int(cy-r), b.Min.Y); y <= min(int(cy+r), b.Max.Y-1); y++ {
		for x := max(int(cx-r), b.Min.X); x <= min(int(cx+r), b.Max.X-1); x++ {
			dx, dy := float64(x)+.5-cx, float64(y)+.5-cy
			u := (dx*cs+dy*sn)/sx + w/2
			v := (-dx*sn+dy*cs)/sy + h/2
			if u < 0 || v < 0 || u >= w || v >= h {
				continue
			}
			c := src.NRGBAAt(src.Bounds().Min.X+int(u), src.Bounds().Min.Y+int(v))
			if c.A == 0 {
				continue
			}
			o := dst.NRGBAAt(x, y)
			al := float64(c.A) / 255
			mix := func(s, d uint8) uint8 { return uint8(float64(s)*al + float64(d)*(1-al) + .5) }
			dst.SetNRGBA(x, y, color.NRGBA{mix(c.R, o.R), mix(c.G, o.G), mix(c.B, o.B), max(o.A, c.A)})
		}
	}
}
