package badgearcade

import (
	"image"
	"image/color"
	"testing"
)

func TestDeployMachine(t *testing.T) {
	_, _, _, data := testArchive(t, "GB_en_data_data_v131.dat.boss")
	i, err := WeeklyArchiveIndex(data)
	if err != nil {
		t.Fatal(err)
	}
	weekly := data[i].Data
	tmpls, err := CraneTemplates(weekly)
	if err != nil || len(tmpls) < 50 {
		t.Fatalf("%d templates, err %v", len(tmpls), err)
	}
	easy := 0
	for _, tp := range tmpls {
		if !tp.Difficult {
			easy++
		}
	}
	t.Logf("%d templates (%d without Difficult obstacles)", len(tmpls), easy)
	if TemplatePrizeCategory(weekly, "Pokemon_091") == "" {
		t.Fatal("no category for Pokemon_091")
	}
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for k := range img.Pix {
		img.Pix[k] = 255
	}
	img.SetNRGBA(0, 0, color.NRGBA{})
	var badges []*Prize
	for n := 0; n < 3; n++ {
		badges = append(badges, BuildPrize(PrizeSpec{BadgeID: uint32(900000000 + n), FileName: "Pr_T_" + string(rune('a'+n)), Category: "Test", Image: img}))
	}
	c, err := DeployMachine(weekly, "Pokemon_091", "Rvt_D0001", 9001, badges)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Prizes) != 3 || c.Name != "Rvt_D0001" || c.ID != 9001 {
		t.Fatalf("machine %+v", c.Prizes)
	}
	for k, p := range c.MachinePrizes {
		if int(p.Index) != k%3 {
			t.Fatalf("spot %d -> badge %d", k, p.Index)
		}
	}
	if _, err := c.Bytes(); err != nil {
		t.Fatal(err)
	}
	if _, err := DeployMachine(weekly, "NoSuchMachine", "X", 1, badges); err == nil {
		t.Fatal("missing template accepted")
	}
}
