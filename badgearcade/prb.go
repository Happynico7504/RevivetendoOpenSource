package badgearcade

// Badge Arcade prize/badge files (.prb, "PRBS" version 3), after Yaz0
// decompression. Layout worked out from Nintendo's files and Brewtendo's
// badgetool (github.com/BrewtendoNetwork/badgetool, GPL-3.0):
//
//	0x000  header (magic, version, sizes, section offsets, badge fields)
//	0x0E0  display names: 16 languages x 0x100 bytes UTF-16LE
//	0x10E0 0x20 bytes (unknown, kept)
//	0x1100 image: 64x64 RGB565+A4, then 32x32 RGB565+A4
//	       per-tile images (same layout) for multi-tile badges
//	       textures: 128x128 ETC1A4 colour, 128x128 ETC1A4 shadow
//	       collision: polygons of up to 8 float32 points
//
// The size fields at 0x08 and 0x38 leave out the per-tile images and the
// texture section. Multi-tile badges ("_Sep") have tile images instead of
// textures and collision.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf16"
)

const (
	prbHeaderEnd   = 0xE0
	prbNamesSize   = 0x1000
	prbImagesStart = 0x1100
	// PrbImageSize is one 64x64 + 32x32 image pair.
	PrbImageSize = 0x2800 + 0xA00
	// PrbTextureSize is the 128x128 colour + shadow ETC1A4 pair.
	PrbTextureSize = 2 * 0x4000
)

// Prize is a parsed .prb file. Fields not understood yet are kept verbatim so
// Bytes() reproduces Nintendo's files exactly.
type Prize struct {
	Version   uint32
	BadgeID   uint32
	Unk40     uint32
	FileName  [0x30]byte // e.g. "Pr_PokeDot00_0048_000_00"; see Name/SetName
	Category  [0x30]byte // e.g. "PokeDot00"
	TitleID   [8]byte    // title the badge launches; all 0xFF for none
	UnkAC     uint32
	B0, B4    int32
	TilesW    uint32 // badge size in 64x64 tiles
	TilesH    uint32
	UnkC0     [16]byte
	ScaleW    float32
	ScaleH    float32
	UnkD8     [8]byte
	Names     [prbNamesSize]byte // see DisplayName/SetDisplayName
	Unk10E0   [0x20]byte
	Image     []byte // 64x64 + 32x32 of the whole badge
	Tiles     []byte // per-tile image pairs (multi-tile badges only)
	Texture   []byte // 128x128 colour + shadow ETC1A4
	Collision []byte // see CollisionPolygons
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func setCString(dst []byte, s string) {
	for i := range dst {
		dst[i] = 0
	}
	copy(dst[:len(dst)-1], s)
}

func (p *Prize) Name() string         { return cString(p.FileName[:]) }
func (p *Prize) SetName(s string)     { setCString(p.FileName[:], s) }
func (p *Prize) CategoryName() string { return cString(p.Category[:]) }
func (p *Prize) SetCategory(s string) { setCString(p.Category[:], s) }

// DisplayNameLanguages is the number of display name slots.
const DisplayNameLanguages = 16

func displayName(names []byte, i int) string {
	slot := names[i*0x100 : (i+1)*0x100]
	var u []uint16
	for j := 0; j+1 < len(slot); j += 2 {
		c := binary.LittleEndian.Uint16(slot[j:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

func setDisplayName(names []byte, i int, s string) {
	slot := names[i*0x100 : (i+1)*0x100]
	for j := range slot {
		slot[j] = 0
	}
	u := utf16.Encode([]rune(s))
	if len(u) > 0x7f {
		u = u[:0x7f]
	}
	for j, c := range u {
		binary.LittleEndian.PutUint16(slot[j*2:], c)
	}
}

// DisplayName returns the badge's name in language slot i (0..15).
func (p *Prize) DisplayName(i int) string { return displayName(p.Names[:], i) }

// SetDisplayName sets language slot i (at most 127 UTF-16 units).
func (p *Prize) SetDisplayName(i int, s string) { setDisplayName(p.Names[:], i, s) }

// Polygon is a collision polygon of up to 8 points.
type Polygon [][2]float32

// CollisionPolygons decodes the collision section: a polygon count, then per
// polygon a point count and 8 float32 (x, y) pairs. There is one list for the
// whole badge, also for multi-tile badges; some badges have none.
func (p *Prize) CollisionPolygons() ([]Polygon, error) {
	d := p.Collision
	if len(d) == 0 {
		return nil, nil
	}
	if len(d) < 4 {
		return nil, errors.New("badgearcade: truncated collision data")
	}
	n := int(binary.LittleEndian.Uint32(d))
	if len(d) != 4+n*(4+0x40) {
		return nil, fmt.Errorf("badgearcade: collision section is %d bytes for %d polygons", len(d), n)
	}
	polys := make([]Polygon, 0, n)
	for k := 0; k < n; k++ {
		off := 4 + k*(4+0x40)
		nv := int(binary.LittleEndian.Uint32(d[off:]))
		if nv > 8 {
			return nil, fmt.Errorf("badgearcade: collision polygon with %d points", nv)
		}
		poly := make(Polygon, nv)
		for v := range poly {
			poly[v] = [2]float32{
				math.Float32frombits(binary.LittleEndian.Uint32(d[off+4+v*8:])),
				math.Float32frombits(binary.LittleEndian.Uint32(d[off+8+v*8:])),
			}
		}
		polys = append(polys, poly)
	}
	return polys, nil
}

// EncodeCollision builds a collision section (at most 8 points per polygon).
func EncodeCollision(polys []Polygon) []byte {
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(polys)))
	for _, poly := range polys {
		if len(poly) > 8 {
			poly = poly[:8]
		}
		out = binary.LittleEndian.AppendUint32(out, uint32(len(poly)))
		coords := make([]byte, 0x40)
		for i, pt := range poly {
			binary.LittleEndian.PutUint32(coords[i*8:], math.Float32bits(pt[0]))
			binary.LittleEndian.PutUint32(coords[i*8+4:], math.Float32bits(pt[1]))
		}
		out = append(out, coords...)
	}
	return out
}

// ParsePrize parses a decompressed .prb file.
func ParsePrize(d []byte) (*Prize, error) {
	if len(d) < prbImagesStart || string(d[:4]) != "PRBS" {
		return nil, errors.New("badgearcade: not a PRBS file")
	}
	u32 := func(o int) uint32 { return binary.LittleEndian.Uint32(d[o:]) }
	section := func(o int) ([]byte, error) {
		s, e := int(u32(o)), int(u32(o+4))
		if s > e || e > len(d) {
			return nil, fmt.Errorf("badgearcade: bad PRBS section at 0x%x (0x%x..0x%x)", o, s, e)
		}
		return append([]byte(nil), d[s:e]...), nil
	}
	p := &Prize{
		Version: u32(0x04),
		BadgeID: u32(0x3C),
		Unk40:   u32(0x40),
		UnkAC:   u32(0xAC),
		B0:      int32(u32(0xB0)),
		B4:      int32(u32(0xB4)),
		TilesW:  u32(0xB8),
		TilesH:  u32(0xBC),
		ScaleW:  math.Float32frombits(u32(0xD0)),
		ScaleH:  math.Float32frombits(u32(0xD4)),
	}
	copy(p.FileName[:], d[0x44:])
	copy(p.Category[:], d[0x74:])
	copy(p.TitleID[:], d[0xA4:])
	copy(p.UnkC0[:], d[0xC0:])
	copy(p.UnkD8[:], d[0xD8:])
	copy(p.Names[:], d[prbHeaderEnd:])
	copy(p.Unk10E0[:], d[prbHeaderEnd+prbNamesSize:])
	var err error
	if p.Image, err = section(0x1C); err != nil {
		return nil, err
	}
	if p.Tiles, err = section(0x24); err != nil {
		return nil, err
	}
	if p.Texture, err = section(0x2C); err != nil {
		return nil, err
	}
	// The collision section runs to the end of the file (its end field, like
	// the size at 0x08, excludes the texture section).
	cs := int(u32(0x34))
	if cs > len(d) {
		return nil, errors.New("badgearcade: bad PRBS collision offset")
	}
	p.Collision = append([]byte(nil), d[cs:]...)
	return p, nil
}

// Bytes serialises the badge (uncompressed).
func (p *Prize) Bytes() []byte {
	is := prbImagesStart
	ie := is + len(p.Image)
	oe := ie + len(p.Tiles)
	ee := oe + len(p.Texture)
	ce := ee + len(p.Collision)
	out := make([]byte, ce)
	put := func(o int, v uint32) { binary.LittleEndian.PutUint32(out[o:], v) }
	copy(out, "PRBS")
	put(0x04, p.Version)
	size := uint32(ce - len(p.Tiles) - len(p.Texture))
	put(0x08, size)
	for i, v := range []int{0x3C, prbHeaderEnd, prbHeaderEnd, prbHeaderEnd + prbNamesSize, is, ie, ie, oe, oe, ee, ee} {
		put(0x0C+i*4, uint32(v))
	}
	put(0x38, size)
	put(0x3C, p.BadgeID)
	put(0x40, p.Unk40)
	copy(out[0x44:], p.FileName[:])
	copy(out[0x74:], p.Category[:])
	copy(out[0xA4:], p.TitleID[:])
	put(0xAC, p.UnkAC)
	put(0xB0, uint32(p.B0))
	put(0xB4, uint32(p.B4))
	put(0xB8, p.TilesW)
	put(0xBC, p.TilesH)
	copy(out[0xC0:], p.UnkC0[:])
	put(0xD0, math.Float32bits(p.ScaleW))
	put(0xD4, math.Float32bits(p.ScaleH))
	copy(out[0xD8:], p.UnkD8[:])
	copy(out[prbHeaderEnd:], p.Names[:])
	copy(out[prbHeaderEnd+prbNamesSize:], p.Unk10E0[:])
	copy(out[is:], p.Image)
	copy(out[ie:], p.Tiles)
	copy(out[oe:], p.Texture)
	copy(out[ee:], p.Collision)
	return out
}
