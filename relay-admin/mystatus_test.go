package main

import (
	"bytes"
	"strings"
	"testing"
)

func renderStatus(t *testing.T, d myStatusData) string {
	t.Helper()
	var buf bytes.Buffer
	if err := myStatusTmpl.Execute(&buf, d); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func mustNotContain(t *testing.T, out string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(out, b) {
			t.Errorf("should not contain %q", b)
		}
	}
}

func mustContain(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q", w)
		}
	}
}

// A 3DS-only account: no Wii U content at all, wallet shown when present.
func TestMyStatus3DSOnlyWithWallet(t *testing.T) {
	out := renderStatus(t, myStatusData{PID: 1, PNID: "ds_only", MiiName: "Keaton", ShowWiiU: false, ShowWallet: true, WalletBalance: 42})
	mustContain(t, out, "Keaton", "42 RevivetendoCoin", "Badge Arcade wallet", "/inkay/my/patreon")
	mustNotContain(t, out, "Friends", "No friends yet", "Offline", "Online", "refresh-label", "setInterval", "Status unavailable")
}

func TestMyStatus3DSOnlyWithoutWallet(t *testing.T) {
	out := renderStatus(t, myStatusData{PID: 1, PNID: "ds_only", MiiName: "Keaton"})
	mustContain(t, out, "Keaton")
	mustNotContain(t, out, "RevivetendoCoin", "Friends", "Offline", "setInterval")
}

// A Wii U account keeps everything it had.
func TestMyStatusWiiU(t *testing.T) {
	out := renderStatus(t, myStatusData{PID: 1, PNID: "wiiu", ShowWiiU: true})
	mustContain(t, out, "Offline", `<span data-i18n="my.friends">Friends</span> (0)`, "No friends yet", "refresh-label", "setInterval")
	mustNotContain(t, out, "RevivetendoCoin")
	on := renderStatus(t, myStatusData{PID: 1, PNID: "wiiu", IsOnline: true, ShowWiiU: true})
	mustContain(t, on, "Online")
}

// A player with both consoles sees everything, including the wallet.
func TestMyStatusBothConsoles(t *testing.T) {
	out := renderStatus(t, myStatusData{PID: 1, PNID: "both", ShowWiiU: true, ShowWallet: true, WalletBalance: 5})
	mustContain(t, out, `<span data-i18n="my.friends">Friends</span> (0)`, "5 RevivetendoCoin")
}

// Account management and the Discord link are for everyone (the bot's password reset
// and /mii work for 3DS-only accounts too); the check mark shows once linked.
func TestMyStatusNavLinks(t *testing.T) {
	for _, d := range []myStatusData{{PID: 1, PNID: "ds_only"}, {PID: 1, PNID: "wiiu", ShowWiiU: true}} {
		out := renderStatus(t, d)
		mustContain(t, out, `href="/inkay/my/account"`, `href="/inkay/my/patreon"`, `href="/inkay/my/discord"`)
		mustNotContain(t, out, "Discord ✓")
	}
	linked := renderStatus(t, myStatusData{PID: 1, PNID: "ds_only", DiscordLinked: true})
	mustContain(t, linked, "Discord ✓")
}

func TestLandingPageCards(t *testing.T) {
	var buf bytes.Buffer
	if err := landingTmpl.Execute(&buf, nil); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	mustContain(t, out, `href="/inkay/my/"`, `href="/inkay/stats/"`, `href="/inkay/admin/"`)
	mustNotContain(t, out, `href="/inkay/my/discord"`, `href="/inkay/my/account"`, "Discord Link", "My Account")
}
