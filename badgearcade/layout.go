package badgearcade

// Custom machine layouts: badge spots, obstacles and attachments placed on
// top of a Nintendo template machine. The template provides everything a
// layout doesn't model (crane type and colour, icon, the collection-screen
// placements) and the starting values of the physics and colour fields that
// players never see. Joints follow their bodies: a joint survives when both
// bodies are still in the layout, and its anchor moves rigidly with body A.
// Coordinates as in machine.go.

import (
	"fmt"
	"math"
)

// Layout object kinds.
const (
	LayoutBadge      = "badge"
	LayoutObstacle   = "obstacle"
	LayoutAttachment = "attachment"
)

// LayoutObject is one badge spot, obstacle or attachment.
type LayoutObject struct {
	Kind     string  `json:"kind"`           // LayoutBadge, LayoutObstacle or LayoutAttachment
	Part     string  `json:"part,omitempty"` // obstacle (.fob) or attachment (.atb) name
	X        float32 `json:"x"`
	Y        float32 `json:"y"`
	ScaleW   float32 `json:"sw"`
	ScaleH   float32 `json:"sh"`
	Rotation float32 `json:"rot"`
	// Template placement this object started as (index into the template's
	// table for its kind), or -1 for an added object; its hidden values and
	// joints are kept.
	From int `json:"from"`
	// Badge physics (confirmed on hardware); nil keeps the starting value.
	Friction *float32 `json:"friction,omitempty"`
	Bounce   *float32 `json:"bounce,omitempty"`
	Strength *float32 `json:"strength,omitempty"` // Unk24 (and Unk28 when changed)
	Falls    *bool    `json:"falls,omitempty"`    // Gravity flag: start falling / at rest
}

// Layout is a machine layout on a template.
type Layout struct {
	Template string         `json:"template"`
	Objects  []LayoutObject `json:"objects"`
}

// Layout limits: each .cib table holds 20 placements, and the obstacle and
// attachment name lists 20 names each.
const (
	LayoutMaxBadges      = 20
	LayoutMaxObstacles   = 20
	LayoutMaxAttachments = 20
)

// Layout value ranges: everything Nintendo's own machines use is accepted
// (obstacles scaled 0.01-4.25, attachments parked down to y = 927, badge
// physics strength up to 3.5, rotations up to 554 degrees), so any Nintendo
// machine is a valid starting layout. Editors steer players to the comfortable
// part (LayoutStrengthComfort: 2 already made the game lag in a hardware test).
var (
	LayoutXRange          = [2]float32{-256, 656}
	LayoutYRange          = [2]float32{-256, 960}
	LayoutScaleRange      = [2]float32{0.01, 5}
	LayoutRotationRange   = [2]float32{-720, 720}
	LayoutFrictionRange   = [2]float32{0, 2}
	LayoutBounceRange     = [2]float32{0, 1}
	LayoutStrengthRange   = [2]float32{0.005, 3.5}
	LayoutStrengthComfort = float32(1.5)
)

// LayoutFromMachine describes a Nintendo machine as a layout (every badge
// spot, obstacle and attachment, From set).
func LayoutFromMachine(c *CraneInstance) Layout {
	l := Layout{Template: c.Name, Objects: []LayoutObject{}}
	for i, p := range c.MachinePrizes {
		f, b, s := p.Unk18, p.Unk20, p.Unk24
		falls := p.Gravity == 1
		l.Objects = append(l.Objects, LayoutObject{Kind: LayoutBadge, X: p.X, Y: p.Y, ScaleW: p.ScaleW, ScaleH: p.ScaleH,
			Rotation: p.Rotation, From: i, Friction: &f, Bounce: &b, Strength: &s, Falls: &falls})
	}
	for _, t := range []struct {
		kind   string
		places []Placement
		names  []string
	}{{LayoutObstacle, c.MachineFixed, c.FixedObjects}, {LayoutAttachment, c.MachineAttachments, c.Attachments}} {
		for i, p := range t.places {
			if int(p.Index) >= len(t.names) {
				continue
			}
			l.Objects = append(l.Objects, LayoutObject{Kind: t.kind, Part: t.names[p.Index], X: p.X, Y: p.Y,
				ScaleW: p.ScaleW, ScaleH: p.ScaleH, Rotation: p.Rotation, From: i})
		}
	}
	return l
}

// Validate checks a layout's ranges and limits (not that its parts exist).
func (l *Layout) Validate() error {
	count := map[string]int{}
	names := map[string]map[string]bool{LayoutObstacle: {}, LayoutAttachment: {}}
	in := func(v float32, r [2]float32) bool { return v >= r[0] && v <= r[1] && !math.IsNaN(float64(v)) }
	for i, o := range l.Objects {
		if !in(o.X, LayoutXRange) || !in(o.Y, LayoutYRange) {
			return fmt.Errorf("object %d is too far off-screen", i+1)
		}
		if !in(o.ScaleW, LayoutScaleRange) || !in(o.ScaleH, LayoutScaleRange) {
			return fmt.Errorf("object %d: scale must be %g-%g", i+1, LayoutScaleRange[0], LayoutScaleRange[1])
		}
		if !in(o.Rotation, LayoutRotationRange) {
			return fmt.Errorf("object %d: rotation must be %g to %g", i+1, LayoutRotationRange[0], LayoutRotationRange[1])
		}
		switch o.Kind {
		case LayoutBadge:
			for _, c := range []struct {
				v *float32
				r [2]float32
				n string
			}{{o.Friction, LayoutFrictionRange, "friction"}, {o.Bounce, LayoutBounceRange, "bounciness"}, {o.Strength, LayoutStrengthRange, "physics strength"}} {
				if c.v != nil && !in(*c.v, c.r) {
					return fmt.Errorf("badge spot %d: %s must be %g-%g", i+1, c.n, c.r[0], c.r[1])
				}
			}
		case LayoutObstacle, LayoutAttachment:
			if o.Part == "" || len(o.Part) >= 0x30 {
				return fmt.Errorf("object %d has no valid part", i+1)
			}
			names[o.Kind][o.Part] = true
		default:
			return fmt.Errorf("object %d: unknown kind %q", i+1, o.Kind)
		}
		count[o.Kind]++
	}
	switch {
	case count[LayoutBadge] == 0:
		return fmt.Errorf("a layout needs at least one badge spot")
	case count[LayoutBadge] > LayoutMaxBadges:
		return fmt.Errorf("at most %d badge spots", LayoutMaxBadges)
	case count[LayoutObstacle] > LayoutMaxObstacles || len(names[LayoutObstacle]) > LayoutMaxObstacles:
		return fmt.Errorf("at most %d obstacles", LayoutMaxObstacles)
	case count[LayoutAttachment] > LayoutMaxAttachments || len(names[LayoutAttachment]) > LayoutMaxAttachments:
		return fmt.Errorf("at most %d attachments", LayoutMaxAttachments)
	}
	return nil
}

// Hidden values for added objects: the most common ones in Nintendo's
// machines (see Placement).
var (
	defaultBadgePlacement = Placement{Unk18: 0.2, Unk1C: 1, Unk20: 0.5, Unk24: 1, Unk28: 1, Gravity: 1,
		Unk30: rgbTriples([12]float32{1, 1, 1, 0, 0, 0, 1, 1, 1, 1, 1, 1})}
	defaultObstaclePlacement = Placement{Unk18: 0.2, Unk1C: 0, Unk20: 0.5, Unk24: 1, Unk28: 1, Gravity: 0,
		Unk30: rgbTriples([12]float32{1, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 1})}
	defaultAttachmentPlacement = Placement{Unk18: 0.2, Unk1C: 1, Unk20: 0.5, Unk24: 1, Unk28: 1, Gravity: 0,
		Unk30: rgbTriples([12]float32{1, 1, 1, 0, 0, 0, 1, 1, 1, 1, 1, 1})}
)

func rgbTriples(v [12]float32) (out [0x30]byte) {
	for i, f := range v {
		bits := math.Float32bits(f)
		out[i*4], out[i*4+1], out[i*4+2], out[i*4+3] = byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24)
	}
	return
}

// movePoint maps a point that moved with a body from the body's old pose to
// its new one (scale, rotation about the centre, translation).
func movePoint(x, y float32, from, to Placement) (float32, float32) {
	if from.ScaleW == 0 || from.ScaleH == 0 {
		return x - from.X + to.X, y - from.Y + to.Y
	}
	rad := func(d float32) float64 { return float64(d) * math.Pi / 180 }
	// into the old body's unscaled local frame
	dx, dy := float64(x-from.X), float64(y-from.Y)
	a := rad(from.Rotation)
	lx := (dx*math.Cos(a) + dy*math.Sin(a)) / float64(from.ScaleW)
	ly := (-dx*math.Sin(a) + dy*math.Cos(a)) / float64(from.ScaleH)
	// back out with the new pose
	lx, ly = lx*float64(to.ScaleW), ly*float64(to.ScaleH)
	b := rad(to.Rotation)
	return to.X + float32(lx*math.Cos(b)-ly*math.Sin(b)), to.Y + float32(lx*math.Sin(b)+ly*math.Cos(b))
}

// ApplyLayout replaces the template machine's badge spots, obstacles and
// attachments with the layout's. Badge spots get Index 0 (DeployMachine
// assigns badges); the name lists are rebuilt from the parts used; joints are
// kept for bodies that remain and follow body A.
func ApplyLayout(c *CraneInstance, l Layout) error {
	if err := l.Validate(); err != nil {
		return err
	}
	orig := *c
	var prizes, fixed, atts []Placement
	var fixedNames, attNames []string
	fixedIdx, attIdx := map[string]int{}, map[string]int{}
	newBadge, newAtt := map[int]int{}, map[int]int{} // template index -> new index
	nameIndex := func(names *[]string, idx map[string]int, part string) uint32 {
		i, ok := idx[part]
		if !ok {
			i = len(*names)
			idx[part] = i
			*names = append(*names, part)
		}
		return uint32(i)
	}
	fromPart := func(places []Placement, names []string, o LayoutObject) (Placement, bool) {
		if o.From >= 0 && o.From < len(places) && int(places[o.From].Index) < len(names) && names[places[o.From].Index] == o.Part {
			return places[o.From], true
		}
		return Placement{}, false
	}
	for _, o := range l.Objects {
		var p Placement
		switch o.Kind {
		case LayoutBadge:
			p = defaultBadgePlacement
			if o.From >= 0 && o.From < len(orig.MachinePrizes) {
				p = orig.MachinePrizes[o.From]
				if _, dup := newBadge[o.From]; !dup {
					newBadge[o.From] = len(prizes)
				}
			} else if len(orig.MachinePrizes) > 0 {
				p = orig.MachinePrizes[0] // the template's own feel for new spots
			}
			if o.Friction != nil {
				p.Unk18 = *o.Friction
			}
			if o.Bounce != nil {
				p.Unk20 = *o.Bounce
			}
			// Unk24/Unk28 differ on 2% of Nintendo's badge spots; both are only
			// overwritten when the strength actually changed.
			if o.Strength != nil && *o.Strength != p.Unk24 {
				p.Unk24, p.Unk28 = *o.Strength, *o.Strength
			}
			if o.Falls != nil {
				p.Gravity = 0
				if *o.Falls {
					p.Gravity = 1
				}
			}
			p.Index = 0
		case LayoutObstacle:
			var ok bool
			if p, ok = fromPart(orig.MachineFixed, orig.FixedObjects, o); !ok {
				p = defaultObstaclePlacement
			}
			p.Index = nameIndex(&fixedNames, fixedIdx, o.Part)
		case LayoutAttachment:
			var ok bool
			if p, ok = fromPart(orig.MachineAttachments, orig.Attachments, o); ok {
				if _, dup := newAtt[o.From]; !dup {
					newAtt[o.From] = len(atts)
				}
			} else {
				p = defaultAttachmentPlacement
			}
			p.Index = nameIndex(&attNames, attIdx, o.Part)
		}
		p.X, p.Y, p.ScaleW, p.ScaleH, p.Rotation = o.X, o.Y, o.ScaleW, o.ScaleH, o.Rotation
		switch o.Kind {
		case LayoutBadge:
			prizes = append(prizes, p)
		case LayoutObstacle:
			fixed = append(fixed, p)
		default:
			atts = append(atts, p)
		}
	}
	// Joints: keep those whose bodies both remain, re-pointed; the anchor
	// moves with body A.
	body := func(kind, i uint32) (newIndex int, old, cur Placement, ok bool) {
		m, oldTable, newTable := newBadge, orig.MachinePrizes, prizes
		if kind == JointAttachment {
			m, oldTable, newTable = newAtt, orig.MachineAttachments, atts
		}
		n, found := m[int(i)]
		if !found || int(i) >= len(oldTable) {
			return 0, Placement{}, Placement{}, false
		}
		return n, oldTable[i], newTable[n], true
	}
	var joints []Joint
	for _, j := range orig.Joints {
		na, oldA, curA, okA := body(j.KindA, j.A)
		nb, _, _, okB := body(j.KindB, j.B)
		if !okA || !okB {
			continue
		}
		j.A, j.B = uint32(na), uint32(nb)
		j.X, j.Y = movePoint(j.X, j.Y, oldA, curA)
		joints = append(joints, j)
	}
	c.MachinePrizes, c.MachineFixed, c.FixedObjects = prizes, fixed, fixedNames
	c.MachineAttachments, c.Attachments, c.Joints = atts, attNames, joints
	return nil
}
