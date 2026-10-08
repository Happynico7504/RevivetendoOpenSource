package main

// Wii U title names for Discord Rich Presence (discord_presence.go). No external
// title database is used at runtime:
//
//   - discordTitleNames (below) is our own curated list. It wins over the
//     backfill (which has e.g. "MARIO KART 8" in caps) and adds titles
//     the backfill lacks. Add a title when players report it
//     (user_settings.presence_title_id / pretendo_friends.title_id). Entries
//     marked "seen" were checked against our own data (the NEX game server the
//     console reported with that title, or Juxt's community title IDs).
//   - wiiubrewTitleNames (wiiu_titles_wiiubrew.go) is generated once from a
//     saved copy of WiiUBrew's title database by scripts/gen_wiiu_titles.py.
//
// WiiUBrew lists 000500001019E500/E600 as Twilight Princess HD, although Juxt's
// Wii Sports Club community carries them.

// discordTitleNames maps a base title ID (updates and DLC map to it, see
// baseTitleID) to its name.
var discordTitleNames = map[uint64]string{
	// System
	0x0005001010040000: "Wii U Menu", // JPN
	0x0005001010040100: "Wii U Menu", // USA
	0x0005001010040200: "Wii U Menu", // EUR
	0x000500101004A000: "Mii Maker",  // JPN
	0x000500101004A100: "Mii Maker",  // USA
	0x000500101004A200: "Mii Maker",  // EUR
	0x000500101005A000: "Wii U Chat", // JPN, seen
	0x000500101005A100: "Wii U Chat", // USA, seen
	0x000500101005A200: "Wii U Chat", // EUR, seen

	// Homebrew
	0x0005000010F7C01A: "Spotify", // Nico's Spotify app (Juxt "Spotify" community)

	// Games
	0x000500001010EB00: "Mario Kart 8", // JPN
	0x000500001010EC00: "Mario Kart 8", // EUR, seen
	0x000500001010ED00: "Mario Kart 8", // USA, seen

	0x0005000010162B00: "Splatoon", // JPN, seen
	0x0005000010176900: "Splatoon", // EUR, seen
	0x0005000010176A00: "Splatoon", // USA, seen

	0x0005000010110E00: "Super Smash Bros. for Wii U", // JPN
	0x0005000010144F00: "Super Smash Bros. for Wii U", // EUR, seen
	0x0005000010145000: "Super Smash Bros. for Wii U", // USA

	0x000500001012F100: "Wii Sports Club", // seen (Juxt)
	0x0005000010144C00: "Wii Sports Club", // seen (Juxt)
	0x0005000010144D00: "Wii Sports Club", // seen (Juxt)
	0x0005000010144E00: "Wii Sports Club", // seen (Juxt)

	0x00050000101D9D00: "Minecraft: Wii U Edition", // USA, seen
	0x00050000101D7500: "Minecraft: Wii U Edition", // EUR, seen
	0x00050000101DBE00: "Minecraft: Wii U Edition", // JPN, seen

	0x000500001018DB00: "Super Mario Maker",
	0x000500001018DC00: "Super Mario Maker",
	0x000500001018DD00: "Super Mario Maker",

	0x00050000101C9300: "The Legend of Zelda: Breath of the Wild", // JPN
	0x00050000101C9400: "The Legend of Zelda: Breath of the Wild", // USA
	0x00050000101C9500: "The Legend of Zelda: Breath of the Wild", // EUR

	0x0005000010143400: "The Legend of Zelda: The Wind Waker HD", // JPN
	0x0005000010143500: "The Legend of Zelda: The Wind Waker HD", // USA
	0x0005000010143600: "The Legend of Zelda: The Wind Waker HD", // EUR

	0x0005000010106100: "Super Mario 3D World", // JPN
	0x0005000010145C00: "Super Mario 3D World", // EUR
	0x0005000010145D00: "Super Mario 3D World", // USA

	0x0005000010101C00: "New Super Mario Bros. U", // JPN
	0x0005000010101D00: "New Super Mario Bros. U", // USA
	0x0005000010101E00: "New Super Mario Bros. U", // EUR

	// Top titles: everything our players have played (user_settings /
	// pretendo_friends, 2026-10-08) plus the best-known Wii U titles, 50 games
	// and apps together with the entries above. IDs from WiiUBrew; the names
	// here fix its capitalisation and use the English title.
	// YouTube (played here)
	0x0005000010105700: "YouTube",
	0x000500001014CE00: "YouTube",

	// Miiverse (played here)
	0x000500301001600A: "Miiverse",
	0x000500301001610A: "Miiverse",
	0x000500301001620A: "Miiverse",

	// Yoshi's Woolly World (played here)
	0x0005000010131F00: "Yoshi's Woolly World",
	0x0005000010184D00: "Yoshi's Woolly World",
	0x0005000010184E00: "Yoshi's Woolly World",

	// Wii Party U (played here)
	0x000500001011A800: "Wii Party U",
	0x0005000010137D00: "Wii Party U",
	0x0005000010137E00: "Wii Party U",

	// The Legend of Zelda: Ocarina of Time (played here)
	0x0005000010199A00: "The Legend of Zelda: Ocarina of Time",
	0x0005000010199B00: "The Legend of Zelda: Ocarina of Time",
	0x0005000010199C00: "The Legend of Zelda: Ocarina of Time",

	// Punch-Out!! (played here)
	0x000500001019D100: "Punch-Out!!",
	0x000500001019D200: "Punch-Out!!",
	0x000500001019D300: "Punch-Out!!",

	// Xenoblade Chronicles X (played here)
	0x0005000010116100: "Xenoblade Chronicles X",
	0x00050000101C4C00: "Xenoblade Chronicles X",
	0x00050000101C4D00: "Xenoblade Chronicles X",

	// Minecraft: Story Mode - The Complete Adventure (played here)
	0x000500001020A200: "Minecraft: Story Mode - The Complete Adventure",
	0x000500001020A300: "Minecraft: Story Mode - The Complete Adventure",

	// Nintendo eShop (played here)
	0x000500301001400A: "Nintendo eShop",
	0x000500301001410A: "Nintendo eShop",
	0x000500301001420A: "Nintendo eShop",

	// Call of Duty: Black Ops II (played here)
	0x000500001010CF00: "Call of Duty: Black Ops II",
	0x0005000010113400: "Call of Duty: Black Ops II",
	0x0005000010113500: "Call of Duty: Black Ops II",
	0x0005000010113700: "Call of Duty: Black Ops II",
	0x000500001011B400: "Call of Duty: Black Ops II",

	// Skylanders Giants (played here)
	0x000500001010D700: "Skylanders Giants",
	0x0005000010116000: "Skylanders Giants",

	// niconico (played here)
	0x0005000010116400: "niconico",

	// Need for Speed: Most Wanted U (played here)
	0x0005000010128400: "Need for Speed: Most Wanted U",
	0x0005000010128800: "Need for Speed: Most Wanted U",
	0x000500001012B700: "Need for Speed: Most Wanted U",

	// Mario & Luigi: Superstar Saga (played here)
	0x0005000010157300: "Mario & Luigi: Superstar Saga",
	0x0005000010157400: "Mario & Luigi: Superstar Saga",
	0x0005000010157500: "Mario & Luigi: Superstar Saga",

	// Ziggurat (played here)
	0x00050000101F7800: "Ziggurat",
	0x00050000101F9C00: "Ziggurat",

	// 99Seconds (played here)
	0x000500001016E700: "99Seconds",
	0x0005000010182700: "99Seconds",
	0x00050000101C4600: "99Seconds",

	// Affordable Space Adventures (played here)
	0x000500001018AB00: "Affordable Space Adventures",
	0x00050000101A1200: "Affordable Space Adventures",
	0x00050000101E0000: "Affordable Space Adventures",

	// Meme Run (played here)
	0x0005000010194000: "Meme Run",

	// Nintendo Land
	0x0005000010101F00: "Nintendo Land",
	0x0005000010102000: "Nintendo Land",
	0x0005000010102100: "Nintendo Land",

	// Mario Party 10
	0x0005000010161F00: "Mario Party 10",
	0x0005000010162D00: "Mario Party 10",
	0x0005000010162E00: "Mario Party 10",

	// Donkey Kong Country: Tropical Freeze
	0x0005000010137F00: "Donkey Kong Country: Tropical Freeze",
	0x0005000010138300: "Donkey Kong Country: Tropical Freeze",
	0x0005000010144800: "Donkey Kong Country: Tropical Freeze",

	// The Legend of Zelda: Twilight Princess HD
	0x000500001019C800: "The Legend of Zelda: Twilight Princess HD",
	0x000500001019E500: "The Legend of Zelda: Twilight Princess HD",
	0x000500001019E600: "The Legend of Zelda: Twilight Princess HD",

	// Pikmin 3
	0x000500001012BC00: "Pikmin 3",
	0x000500001012BD00: "Pikmin 3",
	0x000500001012BE00: "Pikmin 3",

	// Captain Toad: Treasure Tracker
	0x0005000010180500: "Captain Toad: Treasure Tracker",
	0x0005000010180600: "Captain Toad: Treasure Tracker",
	0x0005000010180700: "Captain Toad: Treasure Tracker",

	// Kirby and the Rainbow Curse
	0x0005000010188B00: "Kirby and the Rainbow Curse",
	0x00050000101ABC00: "Kirby and the Rainbow Curse",
	0x00050000101B5100: "Kirby and the Rainbow Curse",

	// Bayonetta 2
	0x000500001011B900: "Bayonetta 2",
	0x0005000010172600: "Bayonetta 2",
	0x0005000010172700: "Bayonetta 2",

	// Hyrule Warriors
	0x000500001017CD00: "Hyrule Warriors",
	0x000500001017D800: "Hyrule Warriors",
	0x000500001017D900: "Hyrule Warriors",

	// Mario Tennis: Ultra Smash
	0x0005000010199000: "Mario Tennis: Ultra Smash",
	0x00050000101A3500: "Mario Tennis: Ultra Smash",
	0x00050000101A3600: "Mario Tennis: Ultra Smash",

	// Paper Mario: Color Splash
	0x000500001F600900: "Paper Mario: Color Splash",
	0x000500001F600A00: "Paper Mario: Color Splash",
	0x000500001F600B00: "Paper Mario: Color Splash",

	// Star Fox Zero
	0x00050000101AFF00: "Star Fox Zero",
	0x00050000101B0400: "Star Fox Zero",
	0x00050000101B0500: "Star Fox Zero",

	// Pokkén Tournament
	0x00050000101C5800: "Pokkén Tournament",
	0x00050000101DF400: "Pokkén Tournament",
	0x00050000101DF500: "Pokkén Tournament",

	// Wii Fit U
	0x0005000010102200: "Wii Fit U",
	0x0005000010102300: "Wii Fit U",
	0x0005000010102400: "Wii Fit U",

	// LEGO City Undercover
	0x0005000010101A00: "LEGO City Undercover",
	0x0005000010101B00: "LEGO City Undercover",
	0x0005000010142F00: "LEGO City Undercover",

	// Netflix
	0x0005000010105A00: "Netflix",

	// Internet Browser
	0x0005001010010307: "Internet Browser",
	0x000500101FBD0205: "Internet Browser",
	0x000500101FBD0206: "Internet Browser",
	0x000500301001200A: "Internet Browser",
	0x0005003010012109: "Internet Browser",
	0x000500301001210A: "Internet Browser",
	0x000500301001220A: "Internet Browser",
	0x000500301002200A: "Internet Browser",
	0x000500301002210A: "Internet Browser",
	0x000500301002220A: "Internet Browser",

	// Animal Crossing: amiibo Festival
	0x0005000010190100: "Animal Crossing: amiibo Festival",
	0x00050000101C6400: "Animal Crossing: amiibo Festival",
	0x00050000101C6500: "Animal Crossing: amiibo Festival",

	// New Super Luigi U
	0x0005000010142200: "New Super Luigi U",
	0x0005000010142300: "New Super Luigi U",
	0x0005000010142400: "New Super Luigi U",
}

// baseTitleID maps an update (0005000E) or DLC (0005000C) title, and the
// sub-variants Wii Sports Club uses in its low byte, to the game's base title.
func baseTitleID(titleID uint64) uint64 {
	switch titleID >> 32 {
	case 0x0005000E, 0x0005000C:
		titleID = 0x00050000<<32 | titleID&0xFFFFFFFF
	}
	if titleID>>32 == 0x00050000 {
		titleID &^= 0xFF
	}
	return titleID
}

// curatedTitleName is the title's name from the curated list, else the
// WiiUBrew backfill ("" when neither knows it).
func curatedTitleName(titleID uint64) string {
	base := baseTitleID(titleID)
	for _, names := range []map[uint64]string{discordTitleNames, wiiubrewTitleNames} {
		if n := names[titleID]; n != "" {
			return n
		}
		if n := names[base]; n != "" {
			return n
		}
	}
	return ""
}
