package badgearcade

import (
	"image"
	"image/color"
	"strings"
	"testing"
)

// TestCrossRegionMachine copies a machine that only exists in the EUR package
// (with the parts JPN lacks) into the JPN weekly archive.
func TestCrossRegionMachine(t *testing.T) {
	load := func(file string) []SARCEntry { _, _, _, e := testArchive(t, file); return e }
	gbData, jpData := load("GB_en_data_data_v131.dat.boss"), load("JP_ja_data_data_v131.dat.boss")
	gbAll, jpAll := load("GB_en_data_allbadge_v131.dat.boss"), load("JP_ja_data_allbadge_v131.dat.boss")
	gi, _ := WeeklyArchiveIndex(gbData)
	ji, _ := WeeklyArchiveIndex(jpData)
	gbWeekly, _ := ParseSARC(gbData[gi].Data)
	jpWeekly, _ := ParseSARC(jpData[ji].Data)

	has := func(arch []SARCEntry, path string) bool {
		for _, e := range arch {
			if e.Name == path {
				return true
			}
		}
		return false
	}
	// Pick an EUR machine that JPN lacks and that needs parts copied.
	tmpls, _ := CraneTemplates(gbData[gi].Data)
	tmpl := ""
	for _, tp := range tmpls {
		if has(jpWeekly, CraneInstancePath(tp.Name)) {
			continue
		}
		m, err := DeployMachine(gbData[gi].Data, tp.Name, "probe", 1, []*Prize{{Version: 3}})
		if err != nil {
			continue
		}
		for _, p := range MachinePartPaths(m) {
			if !has(jpWeekly, p.Path) && !has(jpAll, p.Path) {
				tmpl = tp.Name
			}
		}
		if tmpl != "" {
			break
		}
	}
	if tmpl == "" {
		t.Skip("no EUR-only machine needing parts")
	}
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	img.SetNRGBA(0, 0, color.NRGBA{})
	badge := BuildPrize(PrizeSpec{BadgeID: 900000500, FileName: "Pr_X_500", Category: "Pokemon04", Image: img})
	machine, err := DeployMachine(gbData[gi].Data, tmpl, "Rvt_X0500", 9500, []*Prize{badge})
	if err != nil {
		t.Fatal(err)
	}
	parts, missing := MachineParts(machine, gbWeekly, gbAll)
	if len(missing) > 0 {
		t.Fatalf("parts missing in the source region: %v", missing)
	}
	var needed []ArchiveFile
	for _, p := range parts {
		if !has(jpWeekly, p.Path) && !has(jpAll, p.Path) {
			needed = append(needed, p)
		}
	}
	t.Logf("%s (EUR only) needs %d of %d parts copied into JPN", tmpl, len(needed), len(parts))
	if len(needed) == 0 {
		t.Fatal("expected parts to copy")
	}
	out, err := AddCustomContent(jpData[ji].Data, []*Prize{badge}, []*CraneInstance{machine}, needed...)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := ParseSARC(out)
	var xml string
	for _, e := range after {
		if e.Name == prizeCollectionPath {
			xml = string(e.Data)
		}
	}
	for _, p := range append(needed, ArchiveFile{Path: CraneInstancePath("Rvt_X0500"), Kind: "CraneInstance", Name: "Rvt_X0500"}) {
		if !has(after, p.Path) {
			t.Fatalf("%s not in the archive", p.Path)
		}
		if !strings.Contains(xml, `<`+p.Kind+` name="`+p.Name+`" />`) {
			t.Fatalf("%s %s not registered", p.Kind, p.Name)
		}
	}
	if len(after) != len(jpWeekly)+2+len(needed) {
		t.Fatalf("%d entries, want %d", len(after), len(jpWeekly)+2+len(needed))
	}
}
