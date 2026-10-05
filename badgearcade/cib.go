package badgearcade

// Badge Arcade crane instances (.cib, "CIBS" version 3): one crane machine -
// its background stage, crane type and colour, and which prizes, attachments
// and fixed objects sit where. Fixed size 0x4088 after Yaz0 decompression
// (sections end at 0x4080, followed by 8 zero bytes; the size field is 0x4088).
// Layout from Nintendo's files and Brewtendo's badgetool (GPL-3.0):
//
//	0x000  header (ID, names, availability, colour, crane type, counts)
//	0x100  display names: 16 x 0x100 bytes UTF-16LE
//	0x1100 prize names (20 x 0x30), 0x14C0 attachment names (20 x 0x30),
//	       0x1880 fixed object names (20 x 0x30)
//	0x1C40 placements (0x60 each): machine prizes (20), 0x23C0 collection
//	       prizes (30), 0x2F00 machine attachments (20), 0x3680 machine fixed
//	       objects (20); 0x3E00 attachment badges (20 x 0x20)

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	cibSize           = 0x4088
	cibSectionsEnd    = 0x4080
	cibNamesOff       = 0x100
	cibPrizeNamesOff  = 0x1100
	cibAttachNamesOff = 0x14C0
	cibFixedNamesOff  = 0x1880
	cibNameLen        = 0x30
	cibPlacementSize  = 0x60
	cibAttachBadgeLen = 0x20
)

// Crane types (badgetool / 3dbrew naming).
const (
	CraneStandard uint32 = 0
	CraneHammer   uint32 = 1
	CraneStick    uint32 = 3
	CraneBomb     uint32 = 4
)

// Placement positions a prize, attachment or fixed object in a machine. The
// machine area is the 400x240 top screen.
type Placement struct {
	Index    uint32 // into the matching name list
	ScaleW   float32
	ScaleH   float32
	Rotation float32
	X, Y     float32
	Unk18    float32
	Unk1C    float32
	Unk20    float32
	Unk24    float32
	Unk28    float32
	Gravity  uint32 // 0 = falls, 1 = fixed in place
	Unk30    [0x30]byte
}

func parsePlacement(d []byte) Placement {
	f := func(o int) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(d[o:])) }
	p := Placement{
		Index: binary.LittleEndian.Uint32(d), ScaleW: f(4), ScaleH: f(8), Rotation: f(0xC), X: f(0x10), Y: f(0x14),
		Unk18: f(0x18), Unk1C: f(0x1C), Unk20: f(0x20), Unk24: f(0x24), Unk28: f(0x28),
		Gravity: binary.LittleEndian.Uint32(d[0x2C:]),
	}
	copy(p.Unk30[:], d[0x30:0x60])
	return p
}

func (p Placement) put(d []byte) {
	binary.LittleEndian.PutUint32(d, p.Index)
	for i, v := range []float32{p.ScaleW, p.ScaleH, p.Rotation, p.X, p.Y, p.Unk18, p.Unk1C, p.Unk20, p.Unk24, p.Unk28} {
		binary.LittleEndian.PutUint32(d[4+i*4:], math.Float32bits(v))
	}
	binary.LittleEndian.PutUint32(d[0x2C:], p.Gravity)
	copy(d[0x30:0x60], p.Unk30[:])
}

// placementTable is one of the fixed-capacity placement lists.
type placementTable struct {
	countOff int // header field holding the count
	dataOff  int
	capacity int
}

var (
	cibMachinePrizes     = placementTable{0xEC, 0x1C40, 20}
	cibCollectionPrizes  = placementTable{0xF0, 0x23C0, 30}
	cibMachineAttachment = placementTable{0xF4, 0x2F00, 20}
	cibMachineFixed      = placementTable{0xF8, 0x3680, 20}
)

// CraneInstance is a parsed .cib file. The raw buffer keeps every byte not
// modelled here, so Bytes() reproduces Nintendo's files exactly.
type CraneInstance struct {
	raw []byte

	ID                 uint32
	Name               string // e.g. "PokeDot_006" (what Schedule.xml refers to)
	Crane              string // background stage (.crb), e.g. "CrSt_PokeDot_Wp07"
	Icon               string // crane icon (.icb)
	Availability       uint32 // 0 crane game, 2 tutorial
	UnkC4              uint32
	Color              [3]float32
	Type               uint32 // CraneStandard, CraneHammer, ...
	Prizes             []string
	Attachments        []string
	FixedObjects       []string
	MachinePrizes      []Placement
	CollectionPrizes   []Placement
	MachineAttachments []Placement
	MachineFixed       []Placement
	AttachmentBadges   [][cibAttachBadgeLen]byte
}

// ParseCraneInstance parses a decompressed .cib file.
func ParseCraneInstance(d []byte) (*CraneInstance, error) {
	if len(d) != cibSize || string(d[:4]) != "CIBS" {
		return nil, fmt.Errorf("badgearcade: not a CIBS file (%d bytes)", len(d))
	}
	u32 := func(o int) uint32 { return binary.LittleEndian.Uint32(d[o:]) }
	f32 := func(o int) float32 { return math.Float32frombits(u32(o)) }
	c := &CraneInstance{
		raw:          append([]byte(nil), d...),
		ID:           u32(0x2C),
		Name:         cString(d[0x30:0x60]),
		Crane:        cString(d[0x60:0x90]),
		Icon:         cString(d[0x90:0xC0]),
		Availability: u32(0xC0),
		UnkC4:        u32(0xC4),
		Color:        [3]float32{f32(0xC8), f32(0xCC), f32(0xD0)},
		Type:         u32(0xD4),
	}
	names := func(countOff, off, capacity int) ([]string, error) {
		n := int(u32(countOff))
		if n > capacity {
			return nil, fmt.Errorf("badgearcade: %d names at 0x%x (max %d)", n, off, capacity)
		}
		out := make([]string, n)
		for i := range out {
			out[i] = cString(d[off+i*cibNameLen : off+(i+1)*cibNameLen])
		}
		return out, nil
	}
	table := func(t placementTable) ([]Placement, error) {
		n := int(u32(t.countOff))
		if n > t.capacity {
			return nil, fmt.Errorf("badgearcade: %d placements at 0x%x (max %d)", n, t.dataOff, t.capacity)
		}
		out := make([]Placement, n)
		for i := range out {
			out[i] = parsePlacement(d[t.dataOff+i*cibPlacementSize:])
		}
		return out, nil
	}
	var err error
	if c.Prizes, err = names(0xE0, cibPrizeNamesOff, 20); err != nil {
		return nil, err
	}
	if c.Attachments, err = names(0xE4, cibAttachNamesOff, 20); err != nil {
		return nil, err
	}
	if c.FixedObjects, err = names(0xE8, cibFixedNamesOff, 20); err != nil {
		return nil, err
	}
	for _, t := range []struct {
		dst *[]Placement
		tab placementTable
	}{{&c.MachinePrizes, cibMachinePrizes}, {&c.CollectionPrizes, cibCollectionPrizes}, {&c.MachineAttachments, cibMachineAttachment}, {&c.MachineFixed, cibMachineFixed}} {
		if *t.dst, err = table(t.tab); err != nil {
			return nil, err
		}
	}
	nb := int(u32(0xFC))
	if nb > 20 {
		return nil, errors.New("badgearcade: too many attachment badges")
	}
	for i := 0; i < nb; i++ {
		var b [cibAttachBadgeLen]byte
		copy(b[:], d[0x3E00+i*cibAttachBadgeLen:])
		c.AttachmentBadges = append(c.AttachmentBadges, b)
	}
	return c, nil
}

// NewCraneInstance returns an empty machine in Nintendo's layout.
func NewCraneInstance() *CraneInstance {
	raw := make([]byte, cibSize)
	copy(raw, "CIBS")
	binary.LittleEndian.PutUint32(raw[4:], 3)
	return &CraneInstance{raw: raw}
}

// Bytes serialises the machine (uncompressed). Unused name and placement slots
// are zeroed; everything not modelled comes from the parsed file.
func (c *CraneInstance) Bytes() ([]byte, error) {
	d := append([]byte(nil), c.raw...)
	put := func(o int, v uint32) { binary.LittleEndian.PutUint32(d[o:], v) }
	for i, v := range []int{0x2C, cibNamesOff, cibNamesOff, cibPrizeNamesOff, cibPrizeNamesOff, 0x1C40, 0x1C40, cibSectionsEnd} {
		put(0x0C+i*4, uint32(v))
	}
	put(0x08, cibSize)
	put(0x2C, c.ID)
	for _, s := range []struct {
		off int
		v   string
	}{{0x30, c.Name}, {0x60, c.Crane}, {0x90, c.Icon}} {
		if len(s.v) >= cibNameLen {
			return nil, fmt.Errorf("badgearcade: name %q too long", s.v)
		}
		setCString(d[s.off:s.off+cibNameLen], s.v)
	}
	put(0xC0, c.Availability)
	put(0xC4, c.UnkC4)
	for i, v := range c.Color {
		put(0xC8+i*4, math.Float32bits(v))
	}
	put(0xD4, c.Type)
	lists := []struct {
		countOff, off int
		names         []string
	}{{0xE0, cibPrizeNamesOff, c.Prizes}, {0xE4, cibAttachNamesOff, c.Attachments}, {0xE8, cibFixedNamesOff, c.FixedObjects}}
	for _, l := range lists {
		if len(l.names) > 20 {
			return nil, fmt.Errorf("badgearcade: %d names (max 20)", len(l.names))
		}
		put(l.countOff, uint32(len(l.names)))
		for i := 0; i < 20; i++ {
			slot := d[l.off+i*cibNameLen : l.off+(i+1)*cibNameLen]
			if i < len(l.names) {
				if len(l.names[i]) >= cibNameLen {
					return nil, fmt.Errorf("badgearcade: name %q too long", l.names[i])
				}
				setCString(slot, l.names[i])
			} else {
				for j := range slot {
					slot[j] = 0
				}
			}
		}
	}
	tables := []struct {
		t placementTable
		p []Placement
	}{{cibMachinePrizes, c.MachinePrizes}, {cibCollectionPrizes, c.CollectionPrizes}, {cibMachineAttachment, c.MachineAttachments}, {cibMachineFixed, c.MachineFixed}}
	for _, tb := range tables {
		if len(tb.p) > tb.t.capacity {
			return nil, fmt.Errorf("badgearcade: %d placements (max %d)", len(tb.p), tb.t.capacity)
		}
		put(tb.t.countOff, uint32(len(tb.p)))
		for i := 0; i < tb.t.capacity; i++ {
			slot := d[tb.t.dataOff+i*cibPlacementSize : tb.t.dataOff+(i+1)*cibPlacementSize]
			if i < len(tb.p) {
				tb.p[i].put(slot)
			} else {
				for j := range slot {
					slot[j] = 0
				}
			}
		}
	}
	if len(c.AttachmentBadges) > 20 {
		return nil, errors.New("badgearcade: too many attachment badges")
	}
	put(0xFC, uint32(len(c.AttachmentBadges)))
	for i := 0; i < 20; i++ {
		slot := d[0x3E00+i*cibAttachBadgeLen : 0x3E00+(i+1)*cibAttachBadgeLen]
		if i < len(c.AttachmentBadges) {
			copy(slot, c.AttachmentBadges[i][:])
		} else {
			for j := range slot {
				slot[j] = 0
			}
		}
	}
	return d, nil
}
