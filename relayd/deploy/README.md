# Deploying a relay

A relay is a small Debian server in another region. It needs ports 443, 6666, 9013
and 80 (the consoles' plain-HTTP connection test) open (the ones in `relayd.json`) and outbound access to the main's relay API
(port 7777).

1. **Register it** in the dashboard: `/inkay/admin/relays/` -> *Add relay*
   (id like `us-1`, region, the host name DNS should hand out, health port 443).
   Copy the secret bundle shown once into `bundle.json`.
2. **Build the binary on the main** (static, no dependencies on the relay; stamp a version so OTA can compare):
   `cd relayd && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/Happynico7504/relayd.Version=1" -o relayd ./cmd/relayd`
3. **Copy to the relay**: `relayd`, `deploy/relayd-run`, `deploy/relayd.service`, `deploy/install.sh`,
   `bundle.json`, and a `relayd.json` based on `relayd.example.json` with
   `"bundle": "/run/credentials/relayd.service/bundle"` and `"data_dir": "/var/lib/relayd"`.
4. **On the relay, as root**: `./install.sh && systemctl start relayd`, then delete
   the local `bundle.json`.
5. **Check**: `journalctl -u relayd` should say `certificates synced` and `listening on ...`.
   From another machine: `openssl s_client -connect <relay>:443 -servername olv.nicochristmann.net`
   must show the same certificate fingerprint as the main.
6. **Send players to it**: on the main, `relayhub dns-config -o /etc/revivetendo-dns/regiondns.json`
   (the DNS server reloads it by itself and only hands the relay out while its
   TCP health check on the health port passes).

Updating: rebuild, copy the binary, `systemctl restart relayd`. Certificates are
re-synced from the main every 5 minutes on their own.
Revoking: *Disable* or *Delete* the relay in the dashboard; its bundle stops
working within ~10 seconds.

## Over-the-air updates

Relays update themselves from the main. One-time setup on the main:

    relayhub release keygen          # creates ~/.relayhub/release-key (+ .pub)

Relay bundles created **after** this pin the public half, and only those relays
auto-update. Best practice: move `release-key` off the main and sign releases
elsewhere, so that even a compromised main cannot push code to the relays (it
can only withhold updates).

Publishing a release (version = a strictly increasing integer):

    relayd/deploy/release.sh 12 "what changed"

That builds linux/amd64 and linux/arm64 with the version stamped in, signs them
and puts them in `~/.relayhub/releases/`. Relays check every 30 minutes (config
`update.check_minutes`), optionally only inside a window (`update.window`, e.g.
`"03:00-05:00"` local time). To sign on another machine, set `RELEASES_DIR` to a
scratch directory and copy its `linux-*/` folders into the main's releases directory.

What a relay checks before it runs anything: the manifest signature (pinned
release key), OS/arch, a version strictly higher than the running one and any
rolled-back one (no downgrades), the size and SHA-256 of the download, and a
pre-flight `relayd -version` run of the new binary.

**Rollback.** The launcher (`relayd-run`) counts launches of a freshly installed
version. If it has not proven itself healthy (30 seconds of serving) within 3
launches, the launcher restores the previous binary (or the one from
`install.sh`), records the version as rejected, and it is never offered again;
publish a higher version to try again. This works even when the new binary
crashes before running any of its own code.

### Separately shipped components

Other binaries ship the same way, each with **its own version line** (relayd 8 and
wscedge 1 are unrelated numbers), its own directory and its own rollback, so updating one
never touches another. Today the only one is `wscedge` (the WSC edge prototype, see
`WSC-EDGE.md`; it is its own Go module because it needs a patched nex-go).

    relayd/deploy/release.sh -c wscedge 2 "what changed"
    relayhub release list                       # every component and platform

A relay runs a component only if its config lists it:

    "components": [
      {"name": "wscedge", "args": ["-port", "60115"], "env": ["WSC_KERBEROS_PASSWORD=..."]}
    ]

`relayd` downloads it (same signature, platform, no-downgrade, size/hash and pre-flight
`wscedge -version` checks as its own updates), runs it as a child process under
`<data_dir>/components/wscedge/` and, when a new version lands, restarts **only that
child**. The relay and its NEX auth servers keep running; players connected to the
component reconnect. A `fallback` path can name a binary to run until the first release
arrives; without one nothing runs until a release is published.

The manifest of a non-relayd component signs the component name too (a `wscedge` build
can never be accepted as `relayd`, whatever the hub serves), while relayd's own signing
format is unchanged so every earlier release still verifies.

Rollback for a component is done by relayd's supervisor (the launcher script only
protects relayd itself): a freshly installed version that does not stay up for 30 seconds
within 3 launches, or cannot print its version, is replaced by the previous binary and
never offered again. Components need `-version` printing exactly `<name> <number>`.

Limits: OTA replaces `relayd` only, not the OS, the unit file or `relayd-run`
(those come from `install.sh`); a manual `install.sh` always wins over an earlier
OTA binary. A restart takes about two seconds during which connections reset.
Test the launcher: `sh relayd/deploy/launcher_test.sh`.

## Content cache (Miiverse)

`"content_cache": {"enabled": true}` in `relayd.json` makes the relay keep GET
answers of the OLV hosts for 5 minutes, so repeated reads are served locally.

- **Per user, never shared.** The key contains the host, the exact path and query
  and every request header except a short ignore list (so the user's service token
  and parameter pack are part of it). Only `/assets/` and `/favicon.ico` are shared.
- **Only plain 200 answers** (no `Set-Cookie`, `no-store`, `private`, or `Connection:
  close`), at most 1 MB each, within a memory budget (default 32 MB).
- **Never cached:** notification badges (`/users/notifications`, `/v1/notifications`),
  other hosts, range requests. Tunable: `never_cache`, `hosts`, `shared_prefixes`,
  `ttl_seconds`, `max_mb`, `max_entry_kb`.
- **A write flushes everything ("full resync").** Any non-GET request through the
  relay flushes its cache before the console hears the answer; the main tells every
  other relay to flush within about one network hop. A read that overlapped a write
  is never stored.
- **Bypassed when the main is unreachable** (no contact for 45 seconds).
- Known gap: writes that reach the main without passing a relay or `account-proxy`
  (the Juxt web UI through Cloudflare) do not trigger a flush; they show up when the
  entry expires.
