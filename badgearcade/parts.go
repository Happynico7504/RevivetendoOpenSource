package badgearcade

// Machine parts a .cib refers to besides badges and its stage. Reversed from
// Nintendo's archived files (227 obstacles, 31 attachments, 995 icons); every
// one parses and serialises byte-identically.
//
// Fixed objects (.fob, "FOBS" v3), the obstacles of a machine:
//
//	0x00  "FOBS", 3, size; section offsets: name 0x24, 0x5C, texture 0x80,
//	      texture end, collision (= texture end), end
//	0x24  name, 0x30 bytes (e.g. "FxOb_Slip_Slope02C")
//	0x54  width, height (powers of two, 16-512)
//	0x5C  zero up to 0x80
//	0x80  ETC1A4 texture, width x height
//	      collision polygons (as in badges), in texture pixels, origin top-left
//
// Attachments (.atb, "ATBS" v3), the parts that hang off or ride on things
// (bars, hooks, seesaws, swings):
//
//	0x00  "ATBS", 3, size; section offsets: name 0x2C, 0x64, colour 0x80,
//	      colour end, shadow, shadow end, collision, end
//	0x2C  name, 0x30 bytes
//	0x5C  width, height
//	0x64  zero up to 0x80
//	0x80  ETC1A4 colour texture, then an A8 shadow (blurred silhouette) of the
//	      same size, then collision polygons in texture pixels
//
// Crane icons (.icb, "ICBS" v3), the machine's picture in the hall:
//
//	0x00  "ICBS", 3, size (0x2100); section offsets: name 0x1C, 0xCC,
//	      image 0x100, end
//	0x1C  name, 0x30 bytes (e.g. "CraneIcon_Amiibo_000"); zero up to 0x100
//	0x100 64x64 RGB565, tiled (no alpha)

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
)

// FixedObjectPath, AttachmentPath and CraneIconPath are where the game looks
// for a part.
func FixedObjectPath(name string) string { return "pc/rt/FO/" + name + ".fob.szs" }
func AttachmentPath(name string) string  { return "pc/rt/At/" + name + ".atb.szs" }
func CraneIconPath(name string) string   { return "pc/rt/CI/" + name + ".icb.szs" }

// partHeader checks a part file's magic, version, size field and section
// offset table (n offsets from 0x0C, the last being the file size), and
// returns the offsets.
func partHeader(d []byte, magic string, n int) ([]int, error) {
	if len(d) < 0x0C+4*n || string(d[:4]) != magic {
		return nil, fmt.Errorf("badgearcade: not a %s file", magic)
	}
	u32 := func(o int) int { return int(binary.LittleEndian.Uint32(d[o:])) }
	if v := u32(4); v != 3 {
		return nil, fmt.Errorf("badgearcade: %s version %d", magic, v)
	}
	offs := make([]int, n)
	for i := range offs {
		offs[i] = u32(0x0C + 4*i)
		if offs[i] > len(d) || (i > 0 && offs[i] < offs[i-1]) {
			return nil, fmt.Errorf("badgearcade: bad %s section offsets", magic)
		}
	}
	if u32(8) != len(d) || offs[n-1] != len(d) {
		return nil, fmt.Errorf("badgearcade: %s size doesn't match (%d bytes)", magic, len(d))
	}
	return offs, nil
}

// putPartHeader writes magic, version, size and the section offset table.
func putPartHeader(d []byte, magic string, offs ...int) {
	copy(d, magic)
	binary.LittleEndian.PutUint32(d[4:], 3)
	binary.LittleEndian.PutUint32(d[8:], uint32(len(d)))
	for i, o := range offs {
		binary.LittleEndian.PutUint32(d[0x0C+4*i:], uint32(o))
	}
}

// FixedObject is a parsed .fob file.
type FixedObject struct {
	Name          string
	Width, Height int
	Texture       []byte // ETC1A4
	Collision     []byte // see Polygons
}

// ParseFixedObject parses a decompressed .fob file.
func ParseFixedObject(d []byte) (*FixedObject, error) {
	offs, err := partHeader(d, "FOBS", 6)
	if err != nil {
		return nil, err
	}
	f := &FixedObject{
		Name:  cString(d[0x24:0x54]),
		Width: int(binary.LittleEndian.Uint32(d[0x54:])), Height: int(binary.LittleEndian.Uint32(d[0x58:])),
	}
	if offs[0] != 0x24 || offs[2] != 0x80 || offs[3] != offs[4] || offs[3]-offs[2] != f.Width*f.Height {
		return nil, fmt.Errorf("badgearcade: FOBS %s: unexpected layout", f.Name)
	}
	f.Texture = append([]byte(nil), d[offs[2]:offs[3]]...)
	f.Collision = append([]byte(nil), d[offs[4]:]...)
	return f, nil
}

// ParseFixedObjectFile reads a Yaz0-compressed .fob as stored in the archives.
func ParseFixedObjectFile(szs []byte) (*FixedObject, error) {
	d, err := DecompressYaz0(szs)
	if err != nil {
		return nil, err
	}
	return ParseFixedObject(d)
}

// Bytes serialises the obstacle (uncompressed).
func (f *FixedObject) Bytes() []byte {
	te := 0x80 + len(f.Texture)
	d := make([]byte, te+len(f.Collision))
	putPartHeader(d, "FOBS", 0x24, 0x5C, 0x80, te, te, len(d))
	setCString(d[0x24:0x54], f.Name)
	binary.LittleEndian.PutUint32(d[0x54:], uint32(f.Width))
	binary.LittleEndian.PutUint32(d[0x58:], uint32(f.Height))
	copy(d[0x80:], f.Texture)
	copy(d[te:], f.Collision)
	return d
}

// Image decodes the obstacle's texture.
func (f *FixedObject) Image() *image.NRGBA { return DecodeETC1A4(f.Texture, f.Width, f.Height) }

// Polygons decodes the collision shapes (texture pixels, origin top-left).
func (f *FixedObject) Polygons() ([]Polygon, error) { return decodeCollision(f.Collision) }

// Attachment is a parsed .atb file.
type Attachment struct {
	Name          string
	Width, Height int
	Texture       []byte // ETC1A4
	Shadow        []byte // A8, tiled
	Collision     []byte
}

// ParseAttachment parses a decompressed .atb file.
func ParseAttachment(d []byte) (*Attachment, error) {
	offs, err := partHeader(d, "ATBS", 8)
	if err != nil {
		return nil, err
	}
	a := &Attachment{
		Name:  cString(d[0x2C:0x5C]),
		Width: int(binary.LittleEndian.Uint32(d[0x5C:])), Height: int(binary.LittleEndian.Uint32(d[0x60:])),
	}
	n := a.Width * a.Height
	if offs[0] != 0x2C || offs[2] != 0x80 || offs[3] != offs[4] || offs[5] != offs[6] || offs[3]-offs[2] != n || offs[5]-offs[4] != n {
		return nil, fmt.Errorf("badgearcade: ATBS %s: unexpected layout", a.Name)
	}
	a.Texture = append([]byte(nil), d[offs[2]:offs[3]]...)
	a.Shadow = append([]byte(nil), d[offs[4]:offs[5]]...)
	a.Collision = append([]byte(nil), d[offs[6]:]...)
	return a, nil
}

// ParseAttachmentFile reads a Yaz0-compressed .atb as stored in the archives.
func ParseAttachmentFile(szs []byte) (*Attachment, error) {
	d, err := DecompressYaz0(szs)
	if err != nil {
		return nil, err
	}
	return ParseAttachment(d)
}

// Bytes serialises the attachment (uncompressed).
func (a *Attachment) Bytes() []byte {
	te := 0x80 + len(a.Texture)
	se := te + len(a.Shadow)
	d := make([]byte, se+len(a.Collision))
	putPartHeader(d, "ATBS", 0x2C, 0x64, 0x80, te, te, se, se, len(d))
	setCString(d[0x2C:0x5C], a.Name)
	binary.LittleEndian.PutUint32(d[0x5C:], uint32(a.Width))
	binary.LittleEndian.PutUint32(d[0x60:], uint32(a.Height))
	copy(d[0x80:], a.Texture)
	copy(d[te:], a.Shadow)
	copy(d[se:], a.Collision)
	return d
}

// Image decodes the attachment's colour texture.
func (a *Attachment) Image() *image.NRGBA { return DecodeETC1A4(a.Texture, a.Width, a.Height) }

// ShadowImage decodes the shadow as black with the A8 alpha.
func (a *Attachment) ShadowImage() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, a.Width, a.Height))
	for i, p := range tiledPixels(a.Width, a.Height) {
		img.SetNRGBA(p[0], p[1], color.NRGBA{0, 0, 0, a.Shadow[i]})
	}
	return img
}

// Polygons decodes the collision shapes (texture pixels, origin top-left).
func (a *Attachment) Polygons() ([]Polygon, error) { return decodeCollision(a.Collision) }

const icbSize = 0x2100

// CraneIcon is a parsed .icb file.
type CraneIcon struct {
	Name  string
	Pixel []byte // 64x64 RGB565, tiled
}

// ParseCraneIcon parses a decompressed .icb file.
func ParseCraneIcon(d []byte) (*CraneIcon, error) {
	offs, err := partHeader(d, "ICBS", 4)
	if err != nil {
		return nil, err
	}
	if len(d) != icbSize || offs[0] != 0x1C || offs[2] != 0x100 {
		return nil, fmt.Errorf("badgearcade: ICBS: unexpected layout")
	}
	return &CraneIcon{Name: cString(d[0x1C:0x4C]), Pixel: append([]byte(nil), d[0x100:]...)}, nil
}

// ParseCraneIconFile reads a Yaz0-compressed .icb as stored in the archives.
func ParseCraneIconFile(szs []byte) (*CraneIcon, error) {
	d, err := DecompressYaz0(szs)
	if err != nil {
		return nil, err
	}
	return ParseCraneIcon(d)
}

// Bytes serialises the icon (uncompressed).
func (c *CraneIcon) Bytes() []byte {
	d := make([]byte, icbSize)
	putPartHeader(d, "ICBS", 0x1C, 0xCC, 0x100, icbSize)
	setCString(d[0x1C:0x4C], c.Name)
	copy(d[0x100:], c.Pixel)
	return d
}

// Image decodes the 64x64 icon.
func (c *CraneIcon) Image() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for i, p := range tiledPixels(64, 64) {
		v := binary.LittleEndian.Uint16(c.Pixel[i*2:])
		img.SetNRGBA(p[0], p[1], color.NRGBA{
			uint8(int(v>>11&31) * 255 / 31), uint8(int(v>>5&63) * 255 / 63), uint8(int(v&31) * 255 / 31), 255,
		})
	}
	return img
}
