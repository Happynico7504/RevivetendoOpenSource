package badgearcade

import (
	"bytes"
	"image"
	"strings"
	"testing"
)

// Every Nintendo obstacle, attachment and crane icon parses and serialises
// byte-identically, and its collision shapes decode. Shapes may reach well
// past the texture (FxOb_Slip_StepsDoubleRound00L has a point at x = -54 on a
// 512-wide one), so only absurd coordinates fail.
func TestPartsCorpus(t *testing.T) {
	counts := map[string]int{}
	for _, e := range testCraneFiles(t) {
		var kind string
		var back []byte
		if !strings.HasSuffix(e.Name, ".fob.szs") && !strings.HasSuffix(e.Name, ".atb.szs") && !strings.HasSuffix(e.Name, ".icb.szs") {
			continue
		}
		raw, err := DecompressYaz0(e.Data)
		if err != nil {
			t.Fatalf("%s: %v", e.Name, err)
		}
		switch {
		case strings.HasSuffix(e.Name, ".fob.szs"):
			kind = "fob"
			f, err := ParseFixedObject(raw)
			if err != nil {
				t.Fatalf("%s: %v", e.Name, err)
			}
			if FixedObjectPath(f.Name) != e.Name {
				t.Errorf("%s: name %q", e.Name, f.Name)
			}
			checkPolygons(t, e.Name, f.Collision, f.Width, f.Height)
			back = f.Bytes()
		case strings.HasSuffix(e.Name, ".atb.szs"):
			kind = "atb"
			a, err := ParseAttachment(raw)
			if err != nil {
				t.Fatalf("%s: %v", e.Name, err)
			}
			if AttachmentPath(a.Name) != e.Name {
				t.Errorf("%s: name %q", e.Name, a.Name)
			}
			checkPolygons(t, e.Name, a.Collision, a.Width, a.Height)
			back = a.Bytes()
		case strings.HasSuffix(e.Name, ".icb.szs"):
			kind = "icb"
			c, err := ParseCraneIcon(raw)
			if err != nil {
				t.Fatalf("%s: %v", e.Name, err)
			}
			if CraneIconPath(c.Name) != e.Name {
				t.Errorf("%s: name %q", e.Name, c.Name)
			}
			back = c.Bytes()
		default:
			continue
		}
		if !bytes.Equal(back, raw) {
			t.Errorf("%s: rebuild differs", e.Name)
		}
		counts[kind]++
	}
	if counts["fob"] < 100 || counts["atb"] < 20 || counts["icb"] < 300 {
		t.Fatalf("too few parts: %v", counts)
	}
	t.Log(counts)
}

func checkPolygons(t *testing.T, name string, col []byte, w, h int) {
	t.Helper()
	polys, err := decodeCollision(col)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	for _, p := range polys {
		for _, pt := range p {
			if !(pt[0] > -1024 && pt[1] > -1024 && pt[0] < float32(w)+1024 && pt[1] < float32(h)+1024) {
				t.Errorf("%s: collision point %v not plausible for %dx%d", name, pt, w, h)
				return
			}
		}
	}
}

// Rendering a Nintendo machine puts its stage and objects on the screen: the
// result must differ from the bare stage, and badges must land on-screen.
func TestRenderMachine(t *testing.T) {
	files := map[string][]byte{}
	for _, e := range testCraneFiles(t) {
		files[e.Name] = e.Data
	}
	src := func(p string) ([]byte, bool) { d, ok := files[p]; return d, ok }
	c, err := ParseCraneInstanceFile(files[CraneInstancePath("DotHard_041")])
	if err != nil {
		t.Skip("DotHard_041 not in this corpus")
	}
	objs := MachineObjects(c, src)
	var prizes int
	for _, o := range objs {
		if o.Layer == LayerPrize {
			prizes++
			if p := o.Placement; p.X < 0 || p.X > 400 || p.Y < 0 || p.Y > 240 {
				t.Errorf("badge %s off-screen at %v,%v", o.Name, p.X, p.Y)
			}
		}
	}
	if prizes != len(c.MachinePrizes) {
		t.Fatalf("decoded %d of %d badges", prizes, len(c.MachinePrizes))
	}
	st, _ := ParseCraneStageFile(files[CraneStagePath(c.Crane)])
	img, bare := RenderMachine(c, src), st.Screen()
	diff := 0
	for i := range img.Pix {
		if img.Pix[i] != bare.Pix[i] {
			diff++
		}
	}
	if diff < len(img.Pix)/20 {
		t.Fatalf("render barely differs from the stage (%d bytes)", diff)
	}
}

// Every Nintendo collection book round-trips, and every category a badge uses
// has a book.
func TestCollectionBookCorpus(t *testing.T) {
	books := map[string]*CollectionBook{}
	ids := map[uint32]string{}
	var categories []string
	for _, e := range testCraneFiles(t) {
		switch {
		case strings.HasSuffix(e.Name, ".cab.szs"):
			raw, err := DecompressYaz0(e.Data)
			if err != nil {
				t.Fatal(err)
			}
			b, err := ParseCollectionBook(raw)
			if err != nil {
				t.Fatalf("%s: %v", e.Name, err)
			}
			if CategoryPath(b.Name) != e.Name {
				t.Errorf("%s: name %q", e.Name, b.Name)
			}
			if !bytes.Equal(b.Bytes(), raw) {
				t.Errorf("%s: rebuild differs", e.Name)
			}
			if other, dup := ids[b.ID]; dup && other != b.Name {
				t.Errorf("ID %d used by %s and %s", b.ID, other, b.Name)
			}
			books[b.Name], ids[b.ID] = b, b.Name
		case strings.HasSuffix(e.Name, ".prb.szs"):
			if p, err := ParsePrizeFile(e.Data); err == nil {
				categories = append(categories, p.CategoryName())
			}
		}
	}
	if len(books) < 50 {
		t.Fatalf("only %d books", len(books))
	}
	for _, c := range categories {
		if books[c] == nil {
			t.Fatalf("badge category %q has no book", c)
		}
	}
	for _, b := range books {
		if p := ids[b.ParentID]; p == "" {
			t.Errorf("%s: parent %d is not a book", b.Name, b.ParentID)
		}
	}
}

func TestBuildCollectionBook(t *testing.T) {
	var spec CollectionBookSpec
	spec.ID, spec.Name, spec.TotalBadges, spec.Pages = 9950, "Rvt_Set001", 12, 2
	spec.Names[1], spec.Names[3] = "Revivetendo", "Revivetendo DE"
	icon := image.NewNRGBA(image.Rect(0, 0, 80, 80))
	for i := 0; i < len(icon.Pix); i += 4 {
		icon.Pix[i], icon.Pix[i+3] = 255, 255
	}
	spec.Icon = icon
	b, err := BuildCollectionBook(spec)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseCollectionBook(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if back.ParentID != 9950 || back.DisplayName(1) != "Revivetendo" || back.DisplayName(3) != "Revivetendo DE" || back.DisplayName(0) != "Revivetendo" {
		t.Fatalf("got %+v / %q %q", back.ParentID, back.DisplayName(1), back.DisplayName(0))
	}
	if c := back.IconImage().NRGBAAt(10, 10); c.R < 240 || c.G > 10 {
		t.Fatalf("icon %v", c)
	}
}
