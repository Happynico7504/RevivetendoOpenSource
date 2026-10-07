package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestMatchProfileToConsoleRegion(t *testing.T) {
	profile := func(country string, region uint32) []byte {
		return []byte(fmt.Sprintf(`<person><country>%s</country><language>en</language><region>%d</region><tz_name>America/Bogota</tz_name></person>`, country, region))
	}
	req := func(platform, region, country string) *http.Request {
		r := httptest.NewRequest("GET", "/v1/api/people/@me/profile", nil)
		r.Header.Set("X-Nintendo-Platform-Id", platform)
		r.Header.Set("X-Nintendo-Region", region)
		r.Header.Set("X-Nintendo-Country", country)
		return r
	}
	// Real samples from 2026-10-07: a CO account (0x15020000) and a US account
	// (0x310B0000) on a European 3DS set to Spain.
	for _, in := range [][]byte{profile("CO", 352518144), profile("US", 822804480)} {
		out, changed := matchProfileToConsoleRegion(req("0", "4", "ES"), in)
		if !changed || profileLocaleSummary(out) != "country=ES language=en region=1761673216 tz_name=America/Bogota" {
			t.Fatalf("%s -> changed=%v %s", in, changed, profileLocaleSummary(out))
		}
	}
	for name, c := range map[string]struct {
		r    *http.Request
		body []byte
	}{
		"same region (DE account, Spanish console)": {req("0", "4", "ES"), profile("DE", 1309343744)},
		"Wii U":                        {req("1", "4", "ES"), profile("US", 822804480)},
		"unknown console country":      {req("0", "4", "XX"), profile("US", 822804480)},
		"console country/region clash": {req("0", "2", "ES"), profile("DE", 1309343744)},
	} {
		if out, changed := matchProfileToConsoleRegion(c.r, c.body); changed || string(out) != string(c.body) {
			t.Fatalf("%s: changed", name)
		}
	}
}

func TestAccountProfileFields(t *testing.T) {
	body := []byte(`<person><birth_date>1999-04-01</birth_date><country>CO</country><gender>M</gender><language>en</language><mii><name>X</name></mii></person>`)
	f := accountProfileFields(body)
	if f["birth_date"] != "1999-04-01" || f["country"] != "CO" || f["gender"] != "M" || f["language"] != "en" {
		t.Fatalf("%v", f)
	}
}
