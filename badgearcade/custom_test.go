package badgearcade

import (
	"image"
	"image/color"
	"strings"
	"testing"
)

func TestAddCustomContent(t *testing.T) {
	_, _, _, data := testArchive(t, "GB_en_data_data_v131.dat.boss")
	var weekly []byte
	for _, e := range data {
		if strings.HasPrefix(e.Name, "sharc/") {
			weekly = e.Data
		}
	}
	entries, _ := ParseSARC(weekly)
	var tmpl *CraneInstance
	var xmlBefore string
	for _, e := range entries {
		if e.Name == CraneInstancePath("PokeDot_124") {
			tmpl, _ = ParseCraneInstanceFile(e.Data)
		}
		if e.Name == prizeCollectionPath {
			xmlBefore = string(e.Data)
		}
	}
	if tmpl == nil {
		t.Fatal("no template machine")
	}
	img := image.NewNRGBA(image.Rect(0, 0, 64, 96))
	for y := 10; y < 86; y++ {
		for x := 8; x < 56; x++ {
			img.SetNRGBA(x, y, color.NRGBA{200, 40, 40, 255})
		}
	}
	badge := BuildPrize(PrizeSpec{BadgeID: 900000001, FileName: "Pr_Test_000", Category: "Test", Image: img})
	tmpl.ID, tmpl.Name = 9001, "Test_000"
	tmpl.Prizes = []string{badge.Name()}
	for i := range tmpl.MachinePrizes {
		tmpl.MachinePrizes[i].Index = 0
	}
	out, err := AddCustomContent(weekly, []*Prize{badge}, []*CraneInstance{tmpl})
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseSARC(out)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	for _, e := range after {
		got[e.Name] = e.Data
	}
	if len(after) != len(entries)+2 {
		t.Fatalf("%d entries, want %d", len(after), len(entries)+2)
	}
	p, err := ParsePrizeFile(got[PrizePath("Pr_Test_000")])
	if err != nil || p.BadgeID != 900000001 {
		t.Fatalf("badge not readable back: %v", err)
	}
	c, err := ParseCraneInstanceFile(got[CraneInstancePath("Test_000")])
	if err != nil || c.Name != "Test_000" || c.Prizes[0] != "Pr_Test_000" {
		t.Fatalf("machine not readable back: %v", err)
	}
	xml := string(got[prizeCollectionPath])
	if !strings.Contains(xml, "    <Prize name=\"Pr_Test_000\" />\r\n  </Prizes>") ||
		!strings.Contains(xml, "    <CraneInstance name=\"Test_000\" />\r\n  </CraneInstances>") {
		t.Fatal("not registered in PrizeCollection.xml")
	}
	if !strings.Contains(xmlBefore, `<Prizes count="7751">`) || !strings.Contains(xml, `<Prizes count="7752">`) {
		t.Fatal("prize count not updated")
	}
	// Registering again changes nothing.
	again, _ := RegisterInPrizeCollection(xml, []string{"Pr_Test_000"}, []string{"Test_000"})
	if again != xml {
		t.Fatal("registration is not idempotent")
	}
}
