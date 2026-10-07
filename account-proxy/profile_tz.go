package main

// Correcting the account's UTC offset in profile responses.
//
// The 3DS hands apps a UTC offset from the account profile (<utc_offset>,
// seconds, next to <tz_name>). Pretendo stores a fixed offset per account from
// its timezone list - standard time, no daylight saving (e.g. Europe/Berlin is
// always 3600) - so since their 2026-10-02 maintenance every player in a
// daylight-saving zone sees Badge Arcade's clock an hour behind, and the
// console's time-based checks (e.g. on Miiverse tokens) are off by the same
// hour. We replace the offset with the zone's real current one from the tz
// database, so it also follows the next DST change by itself.

import (
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // tz database built in, independent of the host's zoneinfo
)

var (
	profileTZNameRe    = regexp.MustCompile(`<tz_name>([^<]+)</tz_name>`)
	profileUTCOffsetRe = regexp.MustCompile(`<utc_offset>(-?\d+)</utc_offset>`)
)

// currentUTCOffset returns the zone's offset from UTC right now, in seconds.
func currentUTCOffset(tzName string, now time.Time) (int, bool) {
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return 0, false
	}
	_, offset := now.In(loc).Zone()
	return offset, true
}

// fixProfileUTCOffset rewrites <utc_offset> to the current offset of the
// profile's <tz_name>. Profiles with an unknown or missing timezone are left
// unchanged.
func fixProfileUTCOffset(body []byte) []byte {
	m := profileTZNameRe.FindSubmatch(body)
	if m == nil {
		return body
	}
	offset, ok := currentUTCOffset(string(m[1]), time.Now())
	if !ok {
		return body
	}
	return profileUTCOffsetRe.ReplaceAll(body, []byte("<utc_offset>"+strconv.Itoa(offset)+"</utc_offset>"))
}

var profileLocaleRe = regexp.MustCompile(`<(country|region|language|tz_name|utc_offset)>([^<]*)</`)

// profileLocaleSummary lists the profile's locale fields for the log, e.g.
// "country=DE language=de region=... tz_name=Europe/Berlin utc_offset=7200".
func profileLocaleSummary(body []byte) string {
	var parts []string
	seen := map[string]bool{}
	for _, m := range profileLocaleRe.FindAllSubmatch(body, -1) {
		if k := string(m[1]); !seen[k] {
			seen[k] = true
			parts = append(parts, k+"="+string(m[2]))
		}
	}
	return strings.Join(parts, " ")
}

// profileLocaleOverrides replaces an account's country, region and time zone
// in its profile, per PID - for testing whether a console/account region
// mismatch (e.g. a US account on a European console) breaks a game.
var profileLocaleOverrides = map[uint32]struct {
	Country string
	Region  uint32 // 3DS country code << 24 | subregion << 16
	TZName  string
}{}

// applyProfileLocaleOverride rewrites the profile for overridden PIDs; call it
// before fixProfileUTCOffset so the offset follows the new time zone.
func applyProfileLocaleOverride(pid uint32, body []byte) []byte {
	o, ok := profileLocaleOverrides[pid]
	if !ok {
		return body
	}
	set := func(tag, value string) {
		re := regexp.MustCompile(`<` + tag + `>[^<]*</` + tag + `>`)
		body = re.ReplaceAll(body, []byte("<"+tag+">"+value+"</"+tag+">"))
	}
	set("country", o.Country)
	set("region", strconv.FormatUint(uint64(o.Region), 10))
	set("tz_name", o.TZName)
	return body
}

// consoleLocaleHeaders lists the console's own platform/region/country
// headers on an account request, for the log (certificates, serials and
// tokens are left out).
func consoleLocaleHeaders(r *http.Request) string {
	var parts []string
	for _, h := range []string{"X-Nintendo-Platform-Id", "X-Nintendo-Device-Type", "X-Nintendo-Region", "X-Nintendo-Country", "X-Nintendo-System-Version", "Accept-Language"} {
		if v := r.Header.Get(h); v != "" {
			parts = append(parts, strings.TrimPrefix(h, "X-Nintendo-")+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// 3DS/Wii U country codes (System Settings order); the profile's <region> is
// code << 24 | subregion << 16.
var consoleCountryCodes = map[string]uint32{
	"JP": 1,
	"AI": 8, "AG": 9, "AR": 10, "AW": 11, "BS": 12, "BB": 13, "BZ": 14, "BO": 15, "BR": 16, "VG": 17, "CA": 18,
	"KY": 19, "CL": 20, "CO": 21, "CR": 22, "DM": 23, "DO": 24, "EC": 25, "SV": 26, "GF": 27, "GD": 28, "GP": 29,
	"GT": 30, "GY": 31, "HT": 32, "HN": 33, "JM": 34, "MQ": 35, "MX": 36, "MS": 37, "AN": 38, "NI": 39, "PA": 40,
	"PY": 41, "PE": 42, "KN": 43, "LC": 44, "VC": 45, "SR": 46, "TT": 47, "TC": 48, "US": 49, "UY": 50, "VI": 51, "VE": 52,
	"AL": 64, "AU": 65, "AT": 66, "BE": 67, "BA": 68, "BW": 69, "BG": 70, "HR": 71, "CY": 72, "CZ": 73, "DK": 74,
	"EE": 75, "FI": 76, "FR": 77, "DE": 78, "GR": 79, "HU": 80, "IS": 81, "IE": 82, "IT": 83, "LV": 84, "LS": 85,
	"LI": 86, "LT": 87, "LU": 88, "MK": 89, "MT": 90, "ME": 91, "MZ": 92, "NA": 93, "NL": 94, "NZ": 95, "NO": 96,
	"PL": 97, "PT": 98, "RO": 99, "RU": 100, "RS": 101, "SK": 102, "SI": 103, "ZA": 104, "ES": 105, "SZ": 106,
	"SE": 107, "CH": 108, "TR": 109, "GB": 110, "ZM": 111, "ZW": 112, "AZ": 113, "MR": 114, "ML": 115, "NE": 116,
	"TD": 117, "SD": 118, "ER": 119, "DJ": 120, "SO": 121, "AD": 122, "GI": 123, "GG": 124, "IM": 125, "JE": 126, "MC": 127,
}

// consoleRegionOfCountryCode is the console region (X-Nintendo-Region: 1
// Japan, 2 Americas, 4 Europe/Australia) a country code belongs to; 0 for
// the others (Taiwan, Korea, China, ...), which are left alone.
func consoleRegionOfCountryCode(code uint32) int {
	switch {
	case code == 1:
		return 1
	case code >= 8 && code <= 52:
		return 2
	case code >= 64 && code <= 127:
		return 4
	}
	return 0
}

var profileRegionRe = regexp.MustCompile(`<region>(\d+)</region>`)

// matchProfileToConsoleRegion makes a 3DS account profile's country fit the
// console: Nintendo locked accounts to the console's region, Pretendo doesn't,
// and Badge Arcade stalls ("still being set up") with an account from another
// region (confirmed 2026-10-07: US and CO accounts on a European console). When
// the account's country belongs to another region than the console, the
// profile reports the console's own country instead; the time zone stays.
func matchProfileToConsoleRegion(r *http.Request, body []byte) ([]byte, bool) {
	if r.Header.Get("X-Nintendo-Platform-Id") != "0" { // 3DS only
		return body, false
	}
	consoleRegion, err := strconv.Atoi(r.Header.Get("X-Nintendo-Region"))
	if err != nil {
		return body, false
	}
	consoleCode, ok := consoleCountryCodes[strings.ToUpper(r.Header.Get("X-Nintendo-Country"))]
	if !ok || consoleRegionOfCountryCode(consoleCode) != consoleRegion {
		return body, false
	}
	m := profileRegionRe.FindSubmatch(body)
	if m == nil {
		return body, false
	}
	accountRegion, _ := strconv.ParseUint(string(m[1]), 10, 32)
	accountCode := uint32(accountRegion) >> 24
	if r := consoleRegionOfCountryCode(accountCode); r == 0 || r == consoleRegion {
		return body, false
	}
	country := strings.ToUpper(r.Header.Get("X-Nintendo-Country"))
	body = regexp.MustCompile(`<country>[^<]*</country>`).ReplaceAll(body, []byte("<country>"+country+"</country>"))
	body = profileRegionRe.ReplaceAll(body, []byte(fmt.Sprintf("<region>%d</region>", consoleCode<<24|1<<16)))
	return body, true
}

var profileFieldRe = regexp.MustCompile(`<(birth_date|gender|country|language)>([^<]*)</`)

// accountProfileFields returns the profile's birth date, gender, country and
// language (the first of each tag; nested Mii data has none of them).
func accountProfileFields(body []byte) map[string]string {
	out := map[string]string{}
	for _, m := range profileFieldRe.FindAllSubmatch(body, -1) {
		if k := string(m[1]); out[k] == "" {
			out[k] = strings.TrimSpace(string(m[2]))
		}
	}
	return out
}

// storeAccountProfile keeps the account's profile fields for Juxt (birthday and
// country on user pages, shown only if the user enables them).
func storeAccountProfile(pid uint32, body []byte) {
	f := accountProfileFields(body)
	if f["birth_date"] == "" && f["country"] == "" {
		return
	}
	if _, err := db.Exec(`INSERT INTO account_profiles (pid, birth_date, gender, country, language)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (pid) DO UPDATE SET birth_date = EXCLUDED.birth_date, gender = EXCLUDED.gender,
			country = EXCLUDED.country, language = EXCLUDED.language, updated_at = NOW()`,
		pid, f["birth_date"], f["gender"], f["country"], f["language"]); err != nil {
		log.Printf("profile: storing account profile for PID=%d: %v", pid, err)
	}
}
