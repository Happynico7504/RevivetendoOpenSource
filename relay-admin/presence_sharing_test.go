package main

import (
	"bytes"
	"testing"
	"time"
)

func TestPublicOnlinePage(t *testing.T) {
	var buf bytes.Buffer
	err := publicOnlineTmpl.Execute(&buf, struct {
		Users  []sharedPresence
		Online int
	}{[]sharedPresence{
		{PID: 1, PNID: "Alice", MiiName: "<b>Ali</b>", Online: true, Game: "Wii Sports Club"},
		{PID: 2, PNID: "Bob", Online: false, LastOnline: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)},
		{PID: 3, PNID: "Carol", Online: true},
	}, 2})
	if err != nil {
		t.Fatal(err)
	}
	page := buf.String()
	mustContain(t, page, "&lt;b&gt;Ali&lt;/b&gt;", "Wii Sports Club", "@Bob", "last seen", `data-utc="2026-10-18T12:00:00Z"`,
		`data-i18n="online.online"`, `{"n":2,"total":3}`, "/mii/3/normal_face.png")
	mustNotContain(t, page, "<b>Ali</b>", "Nobody has opted in yet.")

	buf.Reset()
	publicOnlineTmpl.Execute(&buf, struct {
		Users  []sharedPresence
		Online int
	}{nil, 0})
	mustContain(t, buf.String(), "Nobody has opted in yet.")
}

func TestMyAccountSharingSection(t *testing.T) {
	off := renderAccount(t, myAccountData{PNID: "Alice"})
	mustContain(t, off, "/wsc-public/overlay?pnid=Alice", `name="overlay_online"`, `name="public_list"`, "/inkay/my/account/sharing")
	mustNotContain(t, off, "checked")

	on := renderAccount(t, myAccountData{PNID: "Alice", OverlayOnline: true, PublicList: true})
	if n := bytes.Count([]byte(on), []byte(" checked")); n != 2 {
		t.Errorf("want both boxes checked, got %d", n)
	}
}
