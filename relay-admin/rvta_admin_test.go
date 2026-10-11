package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRvtaParseFriendCode(t *testing.T) {
	for in, want := range map[string]int64{"3919-5651-5704": 391956515704, "3919 5651 5704": 391956515704, "391956515704": 391956515704, "0": 0} {
		if got, ok := rvtaParseFriendCode(in); !ok || got != want {
			t.Errorf("%q: got %d %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "12345", "abcd-efgh-ijkl", "1234-5678-90123"} {
		if _, ok := rvtaParseFriendCode(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if rvtaFormatFriendCode(391956515704) != "3919-5651-5704" {
		t.Error("format")
	}
}

func TestRvtaMiiName(t *testing.T) {
	mii := make([]byte, 0x60)
	for i, c := range "Nico" {
		mii[0x1A+2*i] = byte(c)
	}
	if got := rvtaMiiName(mii); got != "Nico" {
		t.Fatalf("got %q", got)
	}
	if rvtaMiiName(nil) != "" {
		t.Fatal("empty mii")
	}
}

func TestRvtaAdminTemplateRenders(t *testing.T) {
	now := time.Now()
	type page = struct {
		Q, Event, Error, PNID, Mii, FCFormatted string
		FC                                      int64
		Searched                                bool
		Limit                                   int
		Events                                  []string
		FriendCodes                             []rvtaFCRow
		Consoles                                []rvtaConsoleRow
		Samples                                 []rvtaSampleRow
		Dumps                                   []rvtaDumpRow
		TotalConsoles, TotalSamples             int
	}
	list := page{Limit: 300, Events: rvtaAdminEvents, FriendCodes: []rvtaFCRow{
		{FriendCode: 391956515704, PNID: "NicoChristmann4", Consoles: 1, Rows: 150, Dumps: 1, FirstSeen: now, LastSeen: now},
		{FriendCode: 0, Consoles: 1, Rows: 3, FirstSeen: now, LastSeen: now},
		{FriendCode: 237397279760, Mii: "Rita", Consoles: 1, Rows: 1, FirstSeen: now, LastSeen: now},
	}}
	var buf bytes.Buffer
	if err := rvtaAdminTmpl.Execute(&buf, list); err != nil {
		t.Fatalf("list view: %v", err)
	}
	if !strings.Contains(buf.String(), "3919-5651-5704") || !strings.Contains(buf.String(), "no friend account") ||
		!strings.Contains(buf.String(), "Mii “Rita”") {
		t.Fatal("list view misses friend codes")
	}

	detail := page{Q: "391956515704", FC: 391956515704, Searched: true, FCFormatted: "3919-5651-5704", PNID: "NicoChristmann4",
		Limit: 300, Events: rvtaAdminEvents,
		Consoles: []rvtaConsoleRow{{ConsoleID: "19409b2b8ac1e601e95da7164ef00127", FriendCode: 391956515704, Version: "1.0.0",
			FirstSeen: now, LastSeen: now, Samples: 150, LastBattery: 49, LastTemp: -128}},
		Samples: []rvtaSampleRow{{ConsoleID: "19409b2b8ac1e601e95da7164ef00127", ConsoleTime: now, ReceivedAt: now, Event: "error",
			BatteryPct: 10, BatteryMV: 3500, TempC: 50, WifiConnected: true, WifiStrength: 3, WirelessMode: 1, TitleID: 0x000400000BEEF500,
			ACError: 0xC8C0BEB6}},
		Dumps: []rvtaDumpRow{{ConsoleID: "19409b2b8ac1e601e95da7164ef00127", Name: "crash-arm11-crash_dump_00000006.dmp", Kind: "crash",
			Total: 1516, Received: 1516, Started: now}}}
	buf.Reset()
	if err := rvtaAdminTmpl.Execute(&buf, detail); err != nil {
		t.Fatalf("detail view: %v", err)
	}
	for _, want := range []string{"NicoChristmann4", "000400000BEEF500", "C8C0BEB6", "download", "export.csv?fc=391956515704"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("detail view misses %q", want)
		}
	}
}

func TestRvtaAdminDumpRejectsTraversal(t *testing.T) {
	for _, q := range []string{"console=../../etc&name=memory-x", "console=19409b2b8ac1e601e95da7164ef00127&name=../../../etc/passwd",
		"console=19409b2b8ac1e601e95da7164ef00127&name=memory-x/../../y"} {
		w := httptest.NewRecorder()
		adminRvtaDump(w, httptest.NewRequest(http.MethodGet, "/admin/rvta/dump?"+q, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d", q, w.Code)
		}
	}
}
