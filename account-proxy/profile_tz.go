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
	"regexp"
	"strconv"
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
