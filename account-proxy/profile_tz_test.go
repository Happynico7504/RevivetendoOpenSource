package main

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCurrentUTCOffset(t *testing.T) {
	summer := time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)
	winter := time.Date(2026, 12, 6, 14, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		tz             string
		summer, winter int
	}{
		{"Europe/Berlin", 7200, 3600}, // Pretendo always sends 3600
		{"America/New_York", -14400, -18000},
		{"America/Dawson", -25200, -25200}, // permanently UTC-7 since 2020; Pretendo sends -28800
		{"Asia/Tokyo", 32400, 32400},
	} {
		s, ok1 := currentUTCOffset(c.tz, summer)
		w, ok2 := currentUTCOffset(c.tz, winter)
		if !ok1 || !ok2 || s != c.summer || w != c.winter {
			t.Errorf("%s: got %d/%d, want %d/%d", c.tz, s, w, c.summer, c.winter)
		}
	}
	if _, ok := currentUTCOffset("Not/AZone", summer); ok {
		t.Error("unknown zone accepted")
	}
}

func TestFixProfileUTCOffset(t *testing.T) {
	body := `<?xml version="1.0"?><person><country>DE</country><tz_name>Europe/Berlin</tz_name><utc_offset>3600</utc_offset></person>`
	out := string(fixProfileUTCOffset([]byte(body)))
	want, _ := currentUTCOffset("Europe/Berlin", time.Now())
	if !strings.Contains(out, "<utc_offset>"+strconv.Itoa(want)+"</utc_offset>") || !strings.Contains(out, "<tz_name>Europe/Berlin</tz_name>") {
		t.Fatalf("not rewritten: %s", out)
	}
	unknown := `<person><tz_name>Nowhere/Land</tz_name><utc_offset>3600</utc_offset></person>`
	if string(fixProfileUTCOffset([]byte(unknown))) != unknown {
		t.Fatal("unknown zone changed")
	}
	if string(fixProfileUTCOffset([]byte("<person/>"))) != "<person/>" {
		t.Fatal("profile without timezone changed")
	}
}
