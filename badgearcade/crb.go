package badgearcade

// Badge Arcade crane stages (.crb, "CRBS" version 3): the background of a
// machine, referenced by name from a .cib's Crane field. Reversed from
// Nintendo's 390 archived stages:
//
//	0x00  "CRBS", version 3, file size (0x10080)
//	0x0C  section offsets: name 0x1C, (empty) 0x4C, texture 0x80, end
//	0x1C  stage name, 0x30 bytes NUL-padded (e.g. "CrSt_PokeDot_Wp07")
//	0x4C  0x34 zero bytes in every file
//	0x80  ETC1 texture, 512 wide; height = size/256 (256 in every real stage,
//	      512 only in the CrSt_Invalid_Invalid placeholder)
//
// The 512x256 texture is an atlas. x 0-14 hold a vertical strip shaded like a
// cylinder in the stage's colour (column 15 is white); x 108-511, y 0-243 hold
// the backdrop: the 400x240 top screen at (110, 2) with a 2-pixel margin
// around it (Nintendo's art bleeds into it). Everything else is white. The
// image is stored top-down (text in Nintendo's stages reads correctly).

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
)

const (
	crbHeaderSize = 0x80
	crbNameOff    = 0x1C
	crbTexWidth   = 512
)

// Stage atlas geometry.
const (
	StageScreenW = 400
	StageScreenH = 240
	StageScreenX = 110 // top-left of the visible screen in the texture
	StageScreenY = 2
	stageMargin  = 2
	stageStripW  = 15
)

// stageStripProfile is the strip's brightness per column relative to its
// mean, averaged over Nintendo's stages: a rounded rail lit from the left.
var stageStripProfile = [stageStripW]float64{1.067, 1.046, 1.080, 1.080, 0.648, 0.784, 0.890, 0.979, 1.007, 0.987, 0.896, 0.782, 0.644, 1.075, 1.071}

// CraneStagePath is where the game looks for a stage.
func CraneStagePath(name string) string { return "pc/rt/Cr/" + name + ".crb.szs" }

// CraneStage is a parsed .crb file.
type CraneStage struct {
	Name    string
	Height  int    // texture height (width is always 512)
	Texture []byte // ETC1
}

// ParseCraneStage parses a decompressed .crb file.
func ParseCraneStage(d []byte) (*CraneStage, error) {
	if len(d) < crbHeaderSize || string(d[:4]) != "CRBS" {
		return nil, fmt.Errorf("badgearcade: not a CRBS file")
	}
	u32 := func(o int) int { return int(binary.LittleEndian.Uint32(d[o:])) }
	if v := u32(4); v != 3 {
		return nil, fmt.Errorf("badgearcade: CRBS version %d", v)
	}
	size := u32(8)
	if size != len(d) || u32(0x0C) != crbNameOff || u32(0x14) != crbHeaderSize || u32(0x18) != size {
		return nil, fmt.Errorf("badgearcade: CRBS size/offsets don't match (%d bytes)", len(d))
	}
	tex := d[crbHeaderSize:]
	h := len(tex) * 2 / crbTexWidth
	if h == 0 || h%8 != 0 || len(tex) != crbTexWidth*h/2 {
		return nil, fmt.Errorf("badgearcade: CRBS texture of %d bytes", len(tex))
	}
	return &CraneStage{Name: cString(d[crbNameOff : crbNameOff+0x30]), Height: h, Texture: append([]byte(nil), tex...)}, nil
}

// ParseCraneStageFile reads a Yaz0-compressed stage as stored in the archives.
func ParseCraneStageFile(szs []byte) (*CraneStage, error) {
	d, err := DecompressYaz0(szs)
	if err != nil {
		return nil, err
	}
	return ParseCraneStage(d)
}

// Bytes serialises the stage (uncompressed), byte-identical to Nintendo's files.
func (s *CraneStage) Bytes() ([]byte, error) {
	if len(s.Name) >= 0x30 {
		return nil, fmt.Errorf("badgearcade: stage name %q too long", s.Name)
	}
	if len(s.Texture) != crbTexWidth*s.Height/2 {
		return nil, fmt.Errorf("badgearcade: stage texture is %d bytes, want %d", len(s.Texture), crbTexWidth*s.Height/2)
	}
	size := crbHeaderSize + len(s.Texture)
	d := make([]byte, size)
	copy(d, "CRBS")
	for i, v := range []int{3, size, crbNameOff, 0x4C, crbHeaderSize, size} {
		binary.LittleEndian.PutUint32(d[4+i*4:], uint32(v))
	}
	setCString(d[crbNameOff:crbNameOff+0x30], s.Name)
	copy(d[crbHeaderSize:], s.Texture)
	return d, nil
}

// Image decodes the whole texture atlas.
func (s *CraneStage) Image() *image.NRGBA { return DecodeETC1(s.Texture, crbTexWidth, s.Height) }

// Screen returns the 400x240 backdrop as the top screen shows it.
func (s *CraneStage) Screen() *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, StageScreenW, StageScreenH))
	draw.Draw(out, out.Bounds(), s.Image(), image.Pt(StageScreenX, StageScreenY), draw.Src)
	return out
}

// Strip returns the 15-pixel rail strip.
func (s *CraneStage) Strip() *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, stageStripW, s.Height))
	draw.Draw(out, out.Bounds(), s.Image(), image.Point{}, draw.Src)
	return out
}

// BuildCraneStage makes a stage from a backdrop (scaled to cover 400x240,
// centred) and the strip colour, in Nintendo's atlas layout.
func BuildCraneStage(name string, backdrop image.Image, tint color.Color) (*CraneStage, error) {
	if len(name) >= 0x30 {
		return nil, fmt.Errorf("badgearcade: stage name %q too long", name)
	}
	const h = 256
	atlas := image.NewNRGBA(image.Rect(0, 0, crbTexWidth, h))
	draw.Draw(atlas, atlas.Bounds(), image.White, image.Point{}, draw.Src)

	t := color.NRGBAModel.Convert(tint).(color.NRGBA)
	for x, k := range stageStripProfile {
		c := color.NRGBA{clamp255(int(float64(t.R)*k + .5)), clamp255(int(float64(t.G)*k + .5)), clamp255(int(float64(t.B)*k + .5)), 255}
		for y := 0; y < h; y++ {
			atlas.SetNRGBA(x, y, c)
		}
	}

	screen := CoverImage(backdrop, StageScreenW, StageScreenH)
	// The screen plus its margin, edge pixels repeated, like Nintendo's bleed.
	for y := -stageMargin; y < StageScreenH+stageMargin; y++ {
		for x := -stageMargin; x < StageScreenW+stageMargin; x++ {
			sx, sy := min(max(x, 0), StageScreenW-1), min(max(y, 0), StageScreenH-1)
			c := screen.NRGBAAt(sx, sy)
			c.A = 255
			atlas.SetNRGBA(StageScreenX+x, StageScreenY+y, c)
		}
	}
	return &CraneStage{Name: name, Height: h, Texture: EncodeETC1(atlas, crbTexWidth, h)}, nil
}

// CoverImage scales src to fill w x h exactly (cropping the overflow, centred),
// flattening transparency onto white.
func CoverImage(src image.Image, w, h int) *image.NRGBA {
	b := src.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(out, out.Bounds(), image.White, image.Point{}, draw.Src)
	if b.Dx() == 0 || b.Dy() == 0 {
		return out
	}
	scale := max(float64(w)/float64(b.Dx()), float64(h)/float64(b.Dy()))
	// Box-filter each destination pixel over the source pixels it covers.
	offX := (float64(b.Dx())*scale - float64(w)) / 2
	offY := (float64(b.Dy())*scale - float64(h)) / 2
	for y := 0; y < h; y++ {
		y0 := (float64(y) + offY) / scale
		y1 := (float64(y+1) + offY) / scale
		for x := 0; x < w; x++ {
			x0 := (float64(x) + offX) / scale
			x1 := (float64(x+1) + offX) / scale
			var r, g, bl, a, n float64
			for sy := int(y0); sy < max(int(y0)+1, int(y1+0.999)); sy++ {
				for sx := int(x0); sx < max(int(x0)+1, int(x1+0.999)); sx++ {
					c := color.NRGBAModel.Convert(src.At(b.Min.X+min(sx, b.Dx()-1), b.Min.Y+min(sy, b.Dy()-1))).(color.NRGBA)
					al := float64(c.A) / 255
					r += float64(c.R) * al
					g += float64(c.G) * al
					bl += float64(c.B) * al
					a += al
					n++
				}
			}
			// Composite over white.
			wht := 255 * (n - a)
			out.SetNRGBA(x, y, color.NRGBA{uint8((r + wht) / n), uint8((g + wht) / n), uint8((bl + wht) / n), 255})
		}
	}
	return out
}
