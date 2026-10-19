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
//	       objects (20); 0x3E00 joints (20 x 0x20, see Joint)

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
	cibJointLen       = 0x20
)

// Crane types (badgetool / 3dbrew naming).
const (
	CraneStandard uint32 = 0
	CraneHammer   uint32 = 1
	CraneStick    uint32 = 3
	CraneBomb     uint32 = 4
)

// Placement positions a prize, attachment or fixed object in a machine; see
// machine.go for the coordinate system (centre, top-screen pixels, y down,
// degrees clockwise). The game simulates machines with Box2D. Friction and
// restitution were confirmed on hardware with test machines (2026-10-18):
//
//	Unk18  friction (CONFIRMED): 0.01 slides off slopes, 0.5 sticks; 0.2 on
//	       nearly every obstacle (Box2D's default)
//	Unk1C  density? 1.0 on most badges, 0 on most obstacles (static bodies)
//	Unk20  restitution / bounce (CONFIRMED): 0 = no bounce, 1 = bounces a lot;
//	       Nintendo's bounciest are the Mario Tennis courts (~0.85)
//	Unk24, Unk28  "physics strength" on hardware (both changed together):
//	       0.01 = badges barely move, 2 = violent enough to make the game lag.
//	       Equal on obstacles, attachments and 98% of badge spots; 157 badge
//	       spots in 71 machines differ (e.g. 0.1/1.0), so they are two related
//	       values. 1.0 on obstacles and attachments, 0.01-1.85 on badges
//	Gravity  1 = the object starts falling (PokeDot_061's badges hang above
//	       their seesaw), 0 = it starts at rest where placed (DotHard_041's
//	       badges stand exactly on crates); collection prizes and obstacles: 0
//	Unk30  12 floats, read as 4 RGB triples; badges mostly (1,1,1) (0,0,0)
//	       (1,1,1) (1,1,1), obstacles mostly (1,0,0) (0,0,0) (0,1,0) (0,0,1),
//	       a few machines use tints - a colour transform of some kind
//
// An editor keeps these from the template (or the table's usual values for new
// objects) rather than letting players set them.
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
	Gravity  uint32
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
	Joints             []Joint
}

// Joint kinds: what a joint's body index refers to.
const (
	JointBadge      uint32 = 0 // MachinePrizes
	JointAttachment uint32 = 1 // MachineAttachments
)

// Joint pins two bodies together at an anchor point (top-screen pixels): a
// badge on a hook, a seesaw plank on its stand. Reversed from all 1,033 joints
// in Nintendo's machines (every index is in range for its kind): 525 pin a
// badge to an attachment, 444 link attachments, 47 an attachment to a badge,
// 17 two badges. Unk0 and Unk1C are always 0.
type Joint struct {
	Unk0         uint32
	KindA, KindB uint32 // JointBadge or JointAttachment
	A, B         uint32 // index into the kind's placement table
	X, Y         float32
	Unk1C        uint32
}

func parseJoint(d []byte) Joint {
	u := func(o int) uint32 { return binary.LittleEndian.Uint32(d[o:]) }
	return Joint{Unk0: u(0), KindA: u(4), KindB: u(8), A: u(0xC), B: u(0x10),
		X: math.Float32frombits(u(0x14)), Y: math.Float32frombits(u(0x18)), Unk1C: u(0x1C)}
}

func (j Joint) put(d []byte) {
	for i, v := range []uint32{j.Unk0, j.KindA, j.KindB, j.A, j.B, math.Float32bits(j.X), math.Float32bits(j.Y), j.Unk1C} {
		binary.LittleEndian.PutUint32(d[i*4:], v)
	}
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
	nj := int(u32(0xFC))
	if nj > 20 {
		return nil, errors.New("badgearcade: too many joints")
	}
	for i := 0; i < nj; i++ {
		c.Joints = append(c.Joints, parseJoint(d[0x3E00+i*cibJointLen:]))
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
	if len(c.Joints) > 20 {
		return nil, errors.New("badgearcade: too many joints")
	}
	put(0xFC, uint32(len(c.Joints)))
	for i := 0; i < 20; i++ {
		slot := d[0x3E00+i*cibJointLen : 0x3E00+(i+1)*cibJointLen]
		if i < len(c.Joints) {
			c.Joints[i].put(slot)
		} else {
			for j := range slot {
				slot[j] = 0
			}
		}
	}
	return d, nil
}
