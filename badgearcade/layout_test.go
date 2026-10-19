package badgearcade

import (
	"math"
	"strings"
	"testing"
)

// Applying a machine's own layout to it reproduces every placement exactly
// (hidden values included), for every Nintendo machine.
func TestLayoutRoundTrip(t *testing.T) {
	n := 0
	for _, e := range testCraneFiles(t) {
		if !strings.HasSuffix(e.Name, ".cib.szs") {
			continue
		}
		orig, err := ParseCraneInstanceFile(e.Data)
		if err != nil || len(orig.MachinePrizes) == 0 || len(orig.MachinePrizes) > LayoutMaxBadges {
			continue
		}
		l := LayoutFromMachine(orig)
		c, _ := ParseCraneInstanceFile(e.Data)
		if err := ApplyLayout(c, l); err != nil {
			t.Fatalf("%s: its own layout is rejected: %v", orig.Name, err)
		}
		if len(c.MachinePrizes) != len(orig.MachinePrizes) || len(c.MachineFixed) != len(orig.MachineFixed) {
			t.Fatalf("%s: counts changed", orig.Name)
		}
		for i, p := range c.MachinePrizes {
			want := orig.MachinePrizes[i]
			want.Index = 0
			if p != want {
				t.Fatalf("%s: badge spot %d\n got %+v\nwant %+v", orig.Name, i, p, want)
			}
		}
		for i, p := range c.MachineFixed {
			want := orig.MachineFixed[i]
			if c.FixedObjects[p.Index] != orig.FixedObjects[want.Index] {
				t.Fatalf("%s: obstacle %d is %s, want %s", orig.Name, i, c.FixedObjects[p.Index], orig.FixedObjects[want.Index])
			}
			p.Index, want.Index = 0, 0
			if p != want {
				t.Fatalf("%s: obstacle %d\n got %+v\nwant %+v", orig.Name, i, p, want)
			}
		}
		for i, p := range c.MachineAttachments {
			want := orig.MachineAttachments[i]
			if c.Attachments[p.Index] != orig.Attachments[want.Index] {
				t.Fatalf("%s: attachment %d is %s, want %s", orig.Name, i, c.Attachments[p.Index], orig.Attachments[want.Index])
			}
			p.Index, want.Index = 0, 0
			if p != want {
				t.Fatalf("%s: attachment %d\n got %+v\nwant %+v", orig.Name, i, p, want)
			}
		}
		if len(c.Joints) != len(orig.Joints) {
			t.Fatalf("%s: %d joints, want %d", orig.Name, len(c.Joints), len(orig.Joints))
		}
		for i, j := range c.Joints {
			w := orig.Joints[i]
			if j.KindA != w.KindA || j.KindB != w.KindB || j.A != w.A || j.B != w.B ||
				math.Abs(float64(j.X-w.X)) > 1e-3 || math.Abs(float64(j.Y-w.Y)) > 1e-3 {
				t.Fatalf("%s: joint %d\n got %+v\nwant %+v", orig.Name, i, j, w)
			}
		}
		if _, err := c.Bytes(); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n < 900 {
		t.Fatalf("only %d machines checked (every Nintendo machine must be a valid layout)", n)
	}
	t.Logf("%d machines reproduce exactly", n)
}

func TestLayoutValidate(t *testing.T) {
	f := func(v float32) *float32 { return &v }
	ok := Layout{Template: "X", Objects: []LayoutObject{{Kind: LayoutBadge, X: 200, Y: 100, ScaleW: 0.6, ScaleH: 0.6, From: -1, Bounce: f(1)}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Layout{
		{Objects: []LayoutObject{{Kind: LayoutObstacle, Part: "FxOb_X", X: 1, Y: 1, ScaleW: 1, ScaleH: 1}}},                      // no badge spot
		{Objects: []LayoutObject{{Kind: LayoutBadge, X: 9999, Y: 1, ScaleW: 1, ScaleH: 1}}},                                      // off-screen
		{Objects: []LayoutObject{{Kind: LayoutBadge, X: 1, Y: 1, ScaleW: 1, ScaleH: 1, Strength: f(5)}}},                         // beyond Nintendo's range
		{Objects: []LayoutObject{{Kind: LayoutBadge, X: 1, Y: 1, ScaleW: 0, ScaleH: 1}}},                                         // zero scale
		{Objects: []LayoutObject{{Kind: "spring", X: 1, Y: 1, ScaleW: 1, ScaleH: 1}, {Kind: LayoutBadge, ScaleW: 1, ScaleH: 1}}}, // unknown kind
	}
	for i, l := range bad {
		if l.Validate() == nil {
			t.Errorf("bad layout %d accepted", i)
		}
	}
}

// Removing a badge spot drops its joints and re-points the rest; moving an
// attachment carries the anchors of its joints along.
func TestLayoutJoints(t *testing.T) {
	var c *CraneInstance
	for _, e := range testCraneFiles(t) {
		if !strings.HasSuffix(e.Name, ".cib.szs") {
			continue
		}
		m, err := ParseCraneInstanceFile(e.Data)
		if err != nil || len(m.MachinePrizes) < 3 {
			continue
		}
		// A machine with an attachment-to-badge joint whose badge isn't spot 0.
		for _, j := range m.Joints {
			if c == nil && j.KindA == JointAttachment && j.KindB == JointBadge && j.B > 0 {
				c = m
			}
		}
	}
	if c == nil {
		t.Skip("no machine with a badge joint")
	}
	l := LayoutFromMachine(c)
	var target Joint
	for _, j := range c.Joints {
		if j.KindA == JointAttachment && j.KindB == JointBadge && j.B > 0 {
			target = j
			break
		}
	}
	// Remove badge spot 0 (always before target.B), move attachment target.A by (+10, +5).
	var objs []LayoutObject
	for _, o := range l.Objects {
		if o.Kind == LayoutBadge && o.From == 0 {
			continue
		}
		if o.Kind == LayoutAttachment && o.From == int(target.A) {
			o.X += 10
			o.Y += 5
		}
		objs = append(objs, o)
	}
	l.Objects = objs
	m := *c
	if err := ApplyLayout(&m, l); err != nil {
		t.Fatal(err)
	}
	for _, j := range m.Joints {
		if j.KindB == JointBadge && j.B >= uint32(len(m.MachinePrizes)) || j.KindA == JointBadge && j.A >= uint32(len(m.MachinePrizes)) {
			t.Fatalf("%s: joint %+v points past %d badge spots", c.Name, j, len(m.MachinePrizes))
		}
	}
	found := false
	for _, j := range m.Joints {
		if j.KindA == JointAttachment && j.KindB == JointBadge && j.B == target.B-1 &&
			math.Abs(float64(j.X-target.X-10)) < 1e-3 && math.Abs(float64(j.Y-target.Y-5)) < 1e-3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("%s: joint %+v not re-pointed to badge %d with its anchor moved (got %+v)", c.Name, target, target.B-1, m.Joints)
	}
	for _, j := range c.Joints {
		if (j.KindA == JointBadge && j.A == 0) || (j.KindB == JointBadge && j.B == 0) {
			for _, k := range m.Joints {
				if k.X == j.X && k.Y == j.Y && k.KindA == j.KindA && k.KindB == j.KindB {
					t.Fatalf("%s: joint of the removed badge spot survived: %+v", c.Name, k)
				}
			}
		}
	}
}
