# Wii Karaoke U by JOYSOUND — revival servers (EU / WAHP)

The game talks to two separate backends, so this project has two servers:

| | What | Where |
|---|---|---|
| **JOYSOUND server** | HTTP API `control_Orchestra300_EU.php`: tickets, song/lyrics/movie/DB URLs, urgent info, play logs | `server.py` (Flask) |
| **NEX server** | Nintendo game server `10100D01`: login, unique IDs, **rankings** | `nex/` (pure Python, UDP) |

Both are built from what's inside the game's `app.rpx`. The reverse-engineering notes are in `docs/PROTOCOL.md` and `nex/README.md`.

```
Wii U ──(account server: tokens)──► your account server (e.g. self-hosted Pretendo)
  │
  ├─ HTTP ─► server.py      /control_Orchestra300_EU.php  (+ /shop, /rankings, /admin)
  └─ UDP  ─► nex_server.py  auth :60000 → secure :60001
```

## What it does

* Implements all 18 command types the app sends: token, service period, purchase history, the music/lyrics/movie/DB/patch/feature/magazine URL lookups, urgent info, and the POSTed play logs.
* **Restores ticket purchases.** The app's "Ticket Purchase" button originally jumped to its Nintendo eShop page. That page is replaced by a web **Ticket Shop** (`/shop`) with the real EU line-up:
  * Free 1-hour trial: one per console, and only before any other ticket
  * 1-Hour Ticket: £1.79 / €1.99
  * 24-Hour Ticket: £4.49 / €4.99
  * 30-Day Ticket: £13.49 / €14.99

  Tickets stack, and you pay with simulated funds (no real money). The game then sees the period through BA0001 exactly as before: remaining time, expiry date, and the "no ticket" dialog.
* Serves the downloadable content the game asks for, with HTTP Range support.
* Logs every request, decoded, plus the play logs. The admin page lets you grant or clear tickets.

## What it can't do by itself

The songs. The JOYSOUND song database, music, lyrics and movies were streamed from Nintendo/JOYSOUND servers and are **not** in the RPX. The server answers each request with a URL and a size from `content/manifest.json`. Until you have those files (for example from an archive or a CDN dump), the app reports "no record" for songs. Feature/magazine pages work the same way.

## Setup

```
pip install -r requirements.txt
cp config.example.json config.json      # set public_url to the address the Wii U can reach
python3 server.py
```

### Point the game at it (recommended: patch the RPX)

```
python3 tools/rpx_patch_url.py app.rpx app.rpx.patched http://192.168.1.10:8383/
```

This rewrites the base URL (up to 47 characters), recompresses `.rodata` and fixes the RPX CRC table. Using `http://` avoids having to patch out SSL. Replace `code/app.rpx` with the patched file, or load it through SDCafiine.

Alternative: redirect `ssl.wahp.wah.wup.app.nintendo.net` in DNS to this server, run it with `ssl_cert`/`ssl_key`, and use an SSL-verification patch. The game pins Nintendo's CA through NSSL.

### Buying tickets

On a PC or phone on the same network as the Wii U, or in the Wii U Internet Browser, open `http://<server>:8383/shop`. Add funds and buy a ticket, then go back into the app.

Tickets are tied to the user's account PID, which the server reads from the `atk` service token (`"identity": "token"`). A Pretendo-compatible account server issues `base64(u32 pid | u64 title | u64 issued | u64 expires | HMAC-SHA256)`. Set `service_token_key` to that server entry's key to verify the HMAC.

The shop lists the accounts that have recently played from the visitor's network, so you just pick yours. The other options are `"identity": "ip"` (one wallet per IP) and `"single"` (one shared wallet).

**Account server note:** the game won't contact the JOYSOUND server without a service token for client ID `471ac6f9b1368cba77a15da69d5b0241`. On a Pretendo account server, that means a `servers` entry with that `client_id` and an `aes_key`.

To skip tickets entirely, `"ticket_mode": "unlimited"` hands every console the game's built-in *permanent* ticket.

Admin page: `/admin?key=<admin_key>`

## Installing the song catalog on real hardware

`tools/build_karaoke_nand.py` builds the full, checksummed `data_base01-04.bin` files plus a `version.txt` marker from `content/manifest.json` and serves them over plain HTTP. **Wii Karaoke U - Catalog Installer** (a standalone Aroma homebrew app, not a plugin, in `/home/nico/custom-tools/wiikaraoke-installer`) downloads those straight into the console's save data and exits — no need to wait through the game's own online update.

Only the **European (EUR)** release is supported. The catalog format, save layout and this server all target that release specifically:

* The **Japanese (WAHJ)** release talks to its own, differently-shaped backend and isn't supported by this server or the installer app.
* The game was **never released in North America** at all. NA consoles need the region patch described below to install/run the EUR release first — there's nothing to install the catalog onto otherwise.

The installer checks for the EUR release's save data before doing anything, and tells the user the above if it isn't there.

## USA region

There never was a North American release. The game only shipped in Japan (WAHJ) and Europe (WAHP). The RPX has no region check of its own, so nothing in it needs patching:

* The console country is only sent to the server as `cid`. This server accepts every country, where the original rejected non-EU ones.
* The system-level lock is `<region>` in `meta/meta.xml`. `python3 tools/meta_region.py meta/meta.xml` changes it from `00000004` (EUR) to `00000006` (USA+EUR). Reinstalling with modified metadata needs CFW signature patches. A region-free plugin on Aroma works without editing any files.
* The in-game languages stay the EU set (English and others). A US console set to English uses English.

## Rankings (NEX)

See `nex/README.md`. Scores show up in-game and on `http://<server>:8383/rankings`, with song titles taken from `song_db` (copy an English `wiiudb.dat` to `content/wiiudb.dat`).

## Song database

`tools/wiiudb.py` reads the game's `wiiudb.dat` (the containers and tables are unencrypted): `python3 tools/wiiudb.py wiiudb.dat songs`.

## Test without a console

```
python3 tools/game_client.py http://127.0.0.1:8383 all
```

This sends requests the way the RPX builds them and parses the replies with ports of the game's parsers.

```
cd nex && python3 tools/nex_test_client.py 127.0.0.1 60000
```

This one runs a full NEX session like the console does: PRUDP handshake with signature checks, Kerberos, RegisterEx and the ranking calls.
