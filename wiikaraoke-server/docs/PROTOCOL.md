# Wii Karaoke U by JOYSOUND (EU) — reverse-engineering notes

Source: `app.rpx` of the EU build (debug path `c:\Home\project\WiiU\wiiu_karaoke\trunk_eu_3_0\`).
All addresses below are the virtual addresses after loading (text `0x02000020`, rodata `0x10000000`, data `0x100FCF80`).
`tools/re/` has the decompressor plus the small PPC decoder/xref tool I used.

## Server and endpoint

| What | Where | Value |
|---|---|---|
| Base URL | `.rodata 0x1001D0E4` (48-byte slot) | `https://ssl.wahp.wah.wup.app.nintendo.net/` |
| Script | `.rodata 0x1001D0C8` | `control_Orchestra300_EU.php` |
| URL builder | `sub_02112D68` | `strcpy(base); strcat(script)` |
| HTTP | `sub_02111058` | libcurl: URL, FOLLOWLOCATION, WRITEFUNCTION, XFERINFO, RANGE `"%u-%u"`, NSSL context (opts 210/211) when flag 0x100, `POSTFIELDS` when method = 1 |
| Token | `sub_02123EF0` | `nn::act::AcquireIndependentServiceToken`, client id `471ac6f9b1368cba77a15da69d5b0241` |

There is one game-specific HTTP server. Miiverse (`nn::olv`) and NEX (rankings, unique ID) are separate Nintendo services that this server does not cover.

## Request encoding

`sub_02113468(buf, size, key, value, flags)` appends one parameter:

| flag | meaning | function |
|---|---|---|
| 0x01 | validate lengths | `sub_02112E5C` |
| 0x02 | XOR every byte with 0xAA (unused by the commands) | `sub_02112EEC` |
| 0x08 | base64 the value | `sub_02113018` |
| 0x10 | URL-encode (`%%%02X`) key and value | `sub_02113260` |
| 0x20 | first parameter: prefix `?` (otherwise `&`) | `sub_02113350` |

`type` is added with flags 57 (`?`+b64+url). The other parameters use 25 or 24 (b64+url).
Common parameters come from `sub_02111E58`:

| key | value |
|---|---|
| `cid` | console country (`cafe.cntry_reg`, stored at config+0xE6670 by `sub_0221C95C`). UK=110, DE=78 (German rating text), USA=49 |
| `lid` | language |
| `atk` | independent service token |
| `ver` | `300` |
| `gcd` | `0` (`0x1010FF2C`) |

The SendLog commands are POSTed (`sub_02116C14`, method 1). The body is `k=v&k=v`, built by `sub_021116D8`: keys raw, values b64+url.

## Response decoding

The reader is a big-endian binary stream (`sub_02018B74`, endian flag = 1, `u8` `sub_02018CC0`, `u16` `sub_02018D84`, `u32` `sub_02018EBC`).
A **line** (`sub_0213DDE0`) is read up to `\r\n`, URL-decoded (`+` becomes a space, `sub_02113B7C`), base64-decoded (`sub_02113924`), and length-checked against the target buffer.

Header (`sub_0213DF60`):

```
u8 category   0 = normal
u8 b          category 0 : 1 OK, 2 -> error 20 ("no record"), else -> error 22   (sub_02111FE4)
              category !=0: >0 means an error-message line follows
              category 2 : clears the ticket state; 7 : message is a Nintendo error code
```

## Commands

Type strings are at `0x1001D028..` and `0x1010FF98..`. The builder and handler pairs live in `0x02113EB8..0x02118920`.

| type | class | params | reply body | parser |
|---|---|---|---|---|
| SA0001 | SendToken | – | line expiry(16), line now(16) | `0213E0FC` |
| TE0001 | GetPurchaseHistory | – | u32 | `0213ED7C` |
| BA0001 | GetServicePeriod | – | u8 status, line A(16), line B(16), u8 permanent | `0213EEE4` |
| SD0001 | GetMusicURL | sid `%07u`, rlog | u32 size, line url(512) | `0213EADC` |
| WO0001 | GetLyrics | sid `%u` | u32 size, line url | `0213EADC` |
| MD0001 | GetMovieURL | mid, mno `%02d` | u32 size, line url | `0213EADC` |
| GP0001 | GetDBPatchURL | pdid | u32 size, line url | `0213EADC` |
| GF0001 | GetFirstLineDBURL | dbid | u32 size, line url | `0213EADC` |
| GD0001 | GetDBURL | – | line id(33), u32 size, line url | `0213E758` |
| GM0001 | GetMovieDBURL | – | line id(33), u32 size, line url | `0213E758` |
| FE0001 | GetFeatureURL | – | line id(33), u32 size, line url(513) | `0213F098` |
| MA0001 | GetMagazineURL | cid=`00`, lid (sent before the common params) | line id(33), u32 size, line url(513) | `0213F228` |
| GL0001 | GetDBPatchList | mdid | u16 n, n × line id(32) | `0213E558` |
| NI0002 | GetUrgentInfoURL | – | u8 has, [u32 size, line url] | `0213E3E0` |
| NI0002 | GetUrgentInfo (no caller found) | – | u8 has, [u16 len, line text(1024)] | `0213E26C` |
| LG0002–LG0005 | SendLog (POST) | type, styp, sstr, smod, sgen, srnk, … / ltyp mv, mb, er, ht, ft, mg, mo, es … | header only | `0213E894` |

All times are `YYYYMMDDhhmmss`. They are parsed by `sub_02113618` (strtoll, then repeated `/100`) into an `OSCalendarTime`.

### Tickets — BA0001 handling (`sub_02114488`)

```
status 1: A = expiry, B = server now
          permanent 1 -> ticket manager set to "permanent" (UI: top_ticket_permanent)
          permanent 0 -> if B > A: error 30 (MSG_DIALOG_ERROR_INVALID_PERIOD) else ticket valid until A
status 2: permanent 1 -> permanent ticket ; else error 20
status 0: permanent 1 -> permanent ticket ; else error 22
```

Ticket manager singleton: `*0x1010FF00`. `sub_0210CA40` stores the server-time offset (vs `OSGetTime`), the expiry, and the permanent flag. Remaining time is `expiry − (OSGetTime + offset)`.

### SendToken — SA0001 (`sub_02117C0C`)

`+172` is the token expiry and `+189` is the server's current time. It requires now ≤ expiry, otherwise error 30. The clock offset is stored (`sub_0210C870`). `sub_0210C920` re-acquires the token once less than one hour is left.

### Buying tickets

The "Ticket Purchase" button calls `SYSSwitchToEShop` with a zeroed 16-byte argument block (`0x02368EF8`, `0x02369170`, `0x023A3C84`, `0x023A3F2C`). That opens the app's own eShop page. A SendLog with `ltyp=es` and `route` is sent first. Back in the app, the period is re-read through BA0001 (and TE0001).

## Region

There is no client-side region or country lock. `cafe.cntry_reg` is read only to fill `cid`, show the German rating notice (`cid == 78`), and select `removal_list.bin`. The original server answered unsupported countries with `MSG_DIALOG_ERROR_NOT_SUPPORT_COUNTRY`. The system-level lock is `<region>` in `meta.xml`.


## NEX (rankings)

The full details are in `nex/README.md`. In short:

- game server `10100D01`, access key `dfc5a4ac`, NEX 3.2.1, PRUDP v1;
- protocols: TicketGranting (10), SecureConnection (11), Utility (110, only `AcquireNexUniqueId`), Ranking (112, methods 1–13);
- the DDL parse trees embedded in the RPX (magic `CD652312`) give the exact signatures and structures for this NEX version. Dump them with `tools/re/ddl.py`.

## Song database (`content/global/tsv/<lang>/wiiudb.dat`)

The format is unencrypted and big-endian:

- **Header:** `u32 version`, `u32 ?`, `u32 count`, then `count × {u32 size, u32 name hash, u32 1}`.
- **Tables:** each is 16-byte aligned. Column widths are at `0x04`, the row size at `0x2C`, column types at `0x2D` (3 = u32, 1 = string), the row count at `0x40`, and rows start at `0x50`. Strings are relative to `0x50`.
- **Songs:** table `B3783E9E`, 60-byte rows: `[song id, title, artist id, ?, year, …]`.
- **Artists:** table `A96E7E80`: `[artist id, name, …]`.
- **Languages:** one database file per language (`wiiudb.dat`, `_1`…`_5`), with the same 2,658 song IDs (750001–755898).
