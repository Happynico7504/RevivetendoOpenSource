package main

import "testing"

func TestCuratedTitleName(t *testing.T) {
	for id, want := range map[uint64]string{
		0x000500001010EC00: "Mario Kart 8",
		0x0005000E1010EC00: "Mario Kart 8", // update
		0x000500001012F101: "Wii Sports Club",
		0x000500101005A100: "Wii U Chat",
		0x0005000010105700: "YouTube",                     // WiiUBrew backfill
		0x0005000010110E00: "Super Smash Bros. for Wii U", // JPN
		0x0005000010F7C01A: "Spotify",
		0x0005000010FFFF00: "",
	} {
		if got := curatedTitleName(id); got != want {
			t.Errorf("%016x: got %q, want %q", id, got, want)
		}
	}
}

func TestDiscordActivityName(t *testing.T) {
	for _, c := range []struct {
		title uint64
		want  string
	}{
		{0, "Wii U Menu"},
		{0x0005000010176A00, "Splatoon"},
		{0x0005000010FFFF00, "Unknown title"},
	} {
		a, _ := discordActivityFor(&discordPresenceLink{online: true, titleID: c.title, pid: 1})
		if a["name"] != c.want {
			t.Errorf("%016x: got %v, want %q", c.title, a["name"], c.want)
		}
	}
}

func TestDiscordActivityOffline(t *testing.T) {
	if _, key := discordActivityFor(&discordPresenceLink{online: false}); key != "" {
		t.Errorf("offline player got key %q", key)
	}
}
