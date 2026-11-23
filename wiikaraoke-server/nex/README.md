# Wii Karaoke U — NEX game server (rankings)

This is a standalone Python server for the game's NEX services. It needs no third-party packages, except `grpcio` if you use the Pretendo password provider.

| | |
|---|---|
| Game server ID | `10100D01` (`lis r6,0x1010; addi r6,r6,0xD01` at `0x0211F508`, passed to `NgsFacade::Login`) |
| Access key | `dfc5a4ac` (UTF-16 string at `0x1001FDA8`, pointer at `0x1010FFF4`) |
| NEX version | 3.2.1 build 2 (header of the DDL parse trees at file offset `0x8198AC`) |
| Transport | PRUDP v1, stream type 10, server port 1, RC4 (`CD&ML` on the auth server, the Kerberos session key on the secure server) |

## What the game uses

All of this comes from the DDL trees embedded in the RPX and from the game's NEX wrapper at `0x0211F3A4..0x02120124`.

| Job state | Call | Ranking client function |
|---|---|---|
| LOGIN | `TicketGranting.LoginEx` (`AuthenticationInfo`) → secure connect → `SecureConnection.RegisterEx` | `0x026613B8` |
| ACQUIRE_UNIQUE_ID | `Utility.AcquireNexUniqueId` | `0x0266FC3C` |
| UPLOAD_COMMON | `Ranking.UploadCommonData` (≤ 255 bytes; the game sends up to 200) | `0x026690E0` |
| UPLOAD_SCORE | `Ranking.UploadScore` | `0x026686A4` |
| GET_RANKING_LIST | `Ranking.GetRanking` | `0x0266A238` |
| DELETE_SCORE | `Ranking.DeleteScore` / `DeleteAllScores` | `0x02668A14` / `0x02668D90` |
| GET_APPROX_ORDER | `Ranking.GetApproxOrder` | `0x0266A668` |
| GET_STATS | `Ranking.GetStats` / `GetRankingByPIDList` | `0x0266AA98` / `0x0266AF04` |

The game has job states for DataStore (`DATA_STORE_*`), but the DataStore client isn't linked into the RPX, so they're dead code.

### What the game puts in a score (`0x022BBBB0`, `0x021303E4`)

| field | value |
|---|---|
| category | song number from the current-song record, almost certainly the `wiiudb.dat` song ID (750001…) |
| score | singing score × 1000 (`fctiwz(points * 1000.0)`) |
| orderBy | 1 (higher is better) |
| updateMode | 0 (keep best) |
| groups | `[country, 0]`; group 0 is the console country code (`cafe.cntry_reg`: UK = 110, Germany = 78, US = 49) |
| param | 0 |

The ranking screens call `GetRanking` like this:

- with mode 4 ("own rank") right after a song;
- with modes 0/1 for the lists;
- with `groupIndex` 255 for no filter, or 0 to filter by country;
- with `timeScope` 0, 1 or 2.

Scopes 0 and 1 are game-defined periods whose meaning isn't recoverable from the client, so `time_scopes` in the config controls them. The default is all-time.

## Running it

```
cp nex_config.example.json nex_config.json   # set public_ip, ports, secure_server_password, password provider
python3 nex_server.py
python3 tools/nex_test_client.py 127.0.0.1 60000          # console-like end-to-end test
```

## The account server part

A Wii U doesn't get the NEX address from the game. It asks the account server for `/v1/api/provider/nex_token/@me?game_server_id=10100D01`. The reply holds `host`, `port`, `pid`, `nex_password` and `token`.

This means you need an account server you control. A self-hosted Pretendo account server works: add a `servers` document like this:

```json
{ "service_name": "Wii Karaoke U (NEX)", "service_type": "nex", "game_server_id": "10100D01",
  "title_ids": ["<your title id from meta.xml>"], "ip": "<public_ip>", "port": 60000,
  "access_mode": "prod", "maintenance_mode": false, "device": 1, "aes_key": "<any non-empty hex>",
  "health_check_port": 60002 }
```

Then pick how this server learns each user's NEX password, since it needs it to build the Kerberos ticket:

| provider | how |
|---|---|
| `pretendo` | gRPC `account.Account/GetNEXPassword` with your account server's master API key (`pip install grpcio`) |
| `json` | `nex_passwords.json` `{ "<pid>": "<password>" }`, kept in sync by your own account server |
| `static` | one password for everyone, for a private account server that hands out the same one |

You can't add servers to Pretendo's public network yourself.
