package main

import (
	"strings"
	"testing"
)

func TestParseFriendCode(t *testing.T) {
	// Reference computed independently (Python: sha1 of the little-endian PID).
	if fc, ok := parseFriendCode("542601732896"); !ok || fc != 542601732896 {
		t.Fatalf("valid friend code rejected: %d %v", fc, ok)
	}
	if _, ok := parseFriendCode("5426-0173-2896"); !ok {
		t.Fatal("dashes should be accepted")
	}
	if fc, ok := parseFriendCode("0"); !ok || fc != 0 {
		t.Fatal("0 (no friend account) should be accepted")
	}
	for _, bad := range []string{"", "542601732897", "12345", "abcdefghijkl", "-42601732896"} {
		if _, ok := parseFriendCode(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseRvtaCSV(t *testing.T) {
	body := strings.Join([]string{
		"# comment",
		"1,1760140800,boot,87,4012,0,0,31,7,1,3,0,1,0,0,0,0",
		"2,1760140860,sample,86,4005,1,1,33,7,0,0,2,0,1125899908105728,0,3365978210,0",
		"3,1760140920,sleep,86,0,0,0,-128,0,0,0,0,0,0,0,0,0",    // MCU unreadable
		"4,1760140980,sample,86,4000,0,0,30,0,0,0,0,0,0,0,0",    // torn line (power loss mid-write)
		"5,1760141040,reboot,86,4000,0,0,30,0,0,0,0,0,0,0,0,0",  // unknown event
		"6,1760141100,sample,101,4000,0,0,30,0,0,0,0,0,0,0,0,0", // battery out of range
		"7,1760141160,sample,86,4000,0,0,30,0,0,4,0,0,0,0,0,0",  // strength out of range
		"",
	}, "\n")
	samples, skipped := parseRvtaCSV([]byte(body))
	if len(samples) != 3 || skipped != 4 {
		t.Fatalf("got %d samples, %d skipped; want 3, 4", len(samples), skipped)
	}
	s := samples[1]
	if s.Seq != 2 || s.Event != "sample" || s.BatteryPct != 86 || s.BatteryMV != 4005 || !s.Charging || !s.Adapter ||
		s.TempC != 33 || s.NetState != 7 || s.WifiConnected || s.WifiStrength != 0 || s.WirelessMode != 2 || s.ShellOpen ||
		s.TitleID != 0x0004000000134600 || s.FRDResult != 3365978210 {
		t.Fatalf("sample parsed wrong: %+v", s)
	}
	if samples[2].TempC != -128 || samples[2].BatteryMV != 0 {
		t.Fatalf("unknown MCU values rejected: %+v", samples[2])
	}
	if s.ConsoleTime.Unix() != 1760140860 {
		t.Fatalf("time parsed wrong: %v", s.ConsoleTime)
	}
}
