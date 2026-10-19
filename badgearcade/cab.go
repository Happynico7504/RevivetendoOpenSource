package badgearcade

// Collection books (.cab, "CABS" v3): the badge sets in the collection. A
// badge joins a set through its category name (Prize.Category = the book's
// Name); every category used by Nintendo's badges has a book. Books are
// grouped under a parent "Book" entry (e.g. MroMkr..MroMkr5 under MroMkrBook,
// ID 104), which is itself a .cab with no badges. Reversed from Nintendo's 130
// archived books; every one parses and serialises byte-identically.
//
//	0x00  "CABS", 3, size (0x4080); section offsets 0x24, 0x68, 0x68, 0x1068,
//	      0x2080, end
//	0x24  category ID (unique; also the display order: Pikmin 25 .. Badge
//	      Arcade 9905)
//	0x28  unknown: 0 in 87 books, a hash-like value in 43
//	0x2C  name, 0x30 bytes (e.g. "MroMkr2")
//	0x5C  the group's total badge count, shared by every book of a group
//	      (Super Mario Maker: 488). The count the game showed always matched
//	      the real number of badges (player observation); our archive only
//	      holds the last weeks' badges, so it can't be recomputed from it.
//	0x60  also shared by the group (Super Mario Maker: 35) - likely pages;
//	      inferred
//	0x64  parent book ID (its own ID for a book without a group)
//	0x68  display names: 16 languages x 0x100 bytes UTF-16LE (as in badges)
//	0x1068 0x1018 bytes, zero in every book
//	0x2080 64x64 RGB565 icon, tiled (as in crane icons)

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
)

const cabSize = 0x4080

// CollectionBook is a parsed .cab file.
type CollectionBook struct {
	ID          uint32
	Unk28       uint32
	Name        string
	TotalBadges uint32 // 0x5C: badges in the book's group
	Pages       uint32 // 0x60 (inferred)
	ParentID    uint32
	Names       [prbNamesSize]byte // see DisplayName/SetDisplayName
	Icon        []byte             // 64x64 RGB565, tiled
}

// ParseCollectionBook parses a decompressed .cab file.
func ParseCollectionBook(d []byte) (*CollectionBook, error) {
	offs, err := partHeader(d, "CABS", 6)
	if err != nil {
		return nil, err
	}
	if len(d) != cabSize || offs[0] != 0x24 || offs[1] != 0x68 || offs[3] != 0x1068 || offs[4] != 0x2080 {
		return nil, fmt.Errorf("badgearcade: CABS: unexpected layout")
	}
	u32 := func(o int) uint32 { return binary.LittleEndian.Uint32(d[o:]) }
	b := &CollectionBook{
		ID: u32(0x24), Unk28: u32(0x28), Name: cString(d[0x2C:0x5C]),
		TotalBadges: u32(0x5C), Pages: u32(0x60), ParentID: u32(0x64),
		Icon: append([]byte(nil), d[0x2080:]...),
	}
	for _, c := range d[0x1068:0x2080] {
		if c != 0 {
			return nil, fmt.Errorf("badgearcade: CABS %s: unexpected data at 0x1068", b.Name)
		}
	}
	copy(b.Names[:], d[0x68:0x1068])
	return b, nil
}

// ParseCollectionBookFile reads a Yaz0-compressed .cab as stored in the archives.
func ParseCollectionBookFile(szs []byte) (*CollectionBook, error) {
	d, err := DecompressYaz0(szs)
	if err != nil {
		return nil, err
	}
	return ParseCollectionBook(d)
}

// Bytes serialises the book (uncompressed).
func (b *CollectionBook) Bytes() []byte {
	d := make([]byte, cabSize)
	putPartHeader(d, "CABS", 0x24, 0x68, 0x68, 0x1068, 0x2080, cabSize)
	put := func(o int, v uint32) { binary.LittleEndian.PutUint32(d[o:], v) }
	put(0x24, b.ID)
	put(0x28, b.Unk28)
	setCString(d[0x2C:0x5C], b.Name)
	put(0x5C, b.TotalBadges)
	put(0x60, b.Pages)
	put(0x64, b.ParentID)
	copy(d[0x68:], b.Names[:])
	copy(d[0x2080:], b.Icon)
	return d
}

func (b *CollectionBook) DisplayName(i int) string       { return displayName(b.Names[:], i) }
func (b *CollectionBook) SetDisplayName(i int, s string) { setDisplayName(b.Names[:], i, s) }

// IconImage decodes the 64x64 icon.
func (b *CollectionBook) IconImage() *image.NRGBA { return (&CraneIcon{Pixel: b.Icon}).Image() }

// encodeRGB565Tiled encodes a 64x64 image as tiled RGB565 (no alpha;
// transparency is flattened onto white).
func encodeRGB565Tiled(img image.Image, w, h int) []byte {
	out := make([]byte, w*h*2)
	bnd := img.Bounds()
	for i, p := range tiledPixels(w, h) {
		c := color.NRGBAModel.Convert(img.At(bnd.Min.X+p[0], bnd.Min.Y+p[1])).(color.NRGBA)
		a := int(c.A)
		blend := func(v uint8) int { return (int(v)*a + 255*(255-a)) / 255 }
		r, g, bl := blend(c.R), blend(c.G), blend(c.B)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(r>>3)<<11|uint16(g>>2)<<5|uint16(bl>>3))
	}
	return out
}

// CollectionBookSpec describes a custom set.
type CollectionBookSpec struct {
	ID          uint32 // unique among all books (Nintendo's run 25-9905)
	Name        string // internal name; badges set Prize.Category to it
	Names       [DisplayNameLanguages]string
	Icon        image.Image // scaled to cover 64x64
	TotalBadges uint32
	Pages       uint32
}

// BuildCollectionBook makes a standalone set (its own parent).
func BuildCollectionBook(s CollectionBookSpec) (*CollectionBook, error) {
	if len(s.Name) == 0 || len(s.Name) >= 0x30 {
		return nil, fmt.Errorf("badgearcade: set name %q must be 1-47 bytes", s.Name)
	}
	b := &CollectionBook{ID: s.ID, Name: s.Name, TotalBadges: s.TotalBadges, Pages: s.Pages, ParentID: s.ID}
	for i, n := range s.Names {
		if n == "" {
			n = s.Names[1]
		}
		b.SetDisplayName(i, n)
	}
	b.Icon = encodeRGB565Tiled(CoverImage(s.Icon, 64, 64), 64, 64)
	return b, nil
}
