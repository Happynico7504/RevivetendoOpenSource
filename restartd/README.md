# restartd - restart one bridge service from a web page

`start.sh` supervises every bridge service in its own loop. `restartd` (a small
Python listener that `start.sh` starts) lets an authenticated caller restart or
rebuild **one** of them without touching the rest. `php/restart.php` is a
self-contained control page for your Apache2 server. It also shows live per-process
CPU, memory, disk and network usage.

Independent of relay-admin and nginx: it keeps working while they are down.

```
browser --HTTPS--> Apache2 box: restart.php --signed HTTP--> restartd (:9333) --request file--> start.sh supervisor
```

## What it does

- **Restart** - stops the service and starts it again immediately (skips the usual 45 s crash delay).
- **Rebuild** - runs the service's `go build` first, and only restarts it if the build succeeds. A broken
  build leaves the running copy alone (status shows "last rebuild failed", details in the service log).
- While a service is down waiting out its crash delay, a request starts it right away.
- Only the services `start.sh` lists in `run/services.json` can be touched. `wiiu-chat` is not in the list
  (it is the foreground process of `start.sh` itself), and neither is `restartd`.
- Juxt (`juxt.service`, `juxt-ui.service`) is separate systemd units, not part of `start.sh`, so it is not covered.

## Setup

1. **Restart the bridge once** (`sudo systemctl restart nico-pretendo-bridge`) so it picks up the new `start.sh`.
   On first start `restartd` creates `restartd/secret.key` (mode 0600, git-ignored).
2. **Make port 9333 reachable** from the Apache2 server (the bridge currently has no firewall rules, so it is
   already open). Change it with `RESTARTD_PORT` / `RESTARTD_BIND` in the environment `start.sh` runs under.
3. **On the Apache2 server**, put `restart.php` and a filled-in `restart-config.php` (copy
   `restart-config.sample.php`) in one directory:
   - `backend`: `http://<bridge address>:9333`
   - `secret`: the contents of `restartd/secret.key` on the bridge
   - `password_hash`: `php -r 'echo password_hash("a long password", PASSWORD_DEFAULT), "\n";'`
   - keep `restart-config.php` out of Git and out of any public download path
4. Serve it over HTTPS only (the page refuses plain HTTP unless you set `allow_http`).

## Security model

| Layer | What it protects against |
|---|---|
| Signed requests (HMAC-SHA256, shared secret) | Anyone without the secret. The secret never travels; each request is bound to its method, path, body, a timestamp (+-60 s) and a single-use nonce, so it cannot be replayed or altered. |
| Whitelist from `start.sh` | Restarting or building anything that `start.sh` did not list. Names are matched against `^[a-z0-9-]+$` and the list, never used in a shell. |
| Rate limits | A service can be restarted once per 20 s; 10 bad signatures block an address for 5 min. |
| Page login (`password_hash`), 5-attempt lockout, CSRF token, `SameSite=Strict` cookie, CSP | Someone who reaches the page but is not you. |
| Optional: Apache client certificate, IP allowlist | Extra gates in `restart-config.php` (`require_client_cert`, `allowed_ips`). |

Known limits:
- **restartd itself is plain HTTP.** Nothing secret crosses it (an eavesdropper can learn which service was
  restarted, nothing more), but the port is public and can be probed or flooded. If that bothers you, put TLS in
  front of it or restrict the port to your Apache2 server's address with your firewall.
- Anyone who obtains `secret.key` **and** the page password (or the secret alone, calling `restartd` directly)
  can restart services. Rotate by deleting `restartd/secret.key` and restarting the bridge.
- A restart drops that service's connected users (for WSC, its players).

## Usage numbers

Per service, including its child processes, sampled every 3 s from `/proc` and `ss`:
CPU (per core, so 200 % = two cores), memory (RSS), disk read/write (actual storage I/O), TCP connection
count with bytes in/out per second, and UDP socket count. Linux keeps **no per-process UDP byte counters**, so
WSC's game traffic (UDP) can only be shown as socket counts per service and as host-wide network totals; exact
per-service UDP throughput would need root/eBPF. TCP rates come from live connections, so a closed connection's
bytes disappear from the total and a rate can briefly dip.

## API (for scripts)

`GET /v1/services` and `POST /v1/restart` with `{"service": "wsc-secure", "rebuild": false}`. Sign as
`HMAC-SHA256(secret, "v1\n" + METHOD + "\n" + PATH + "\n" + TIMESTAMP + "\n" + NONCE + "\n" + sha256hex(body))` and
send `X-RD-Timestamp`, `X-RD-Nonce`, `X-RD-Signature` (see `rd_call()` in `php/restart.php` for a working example).
