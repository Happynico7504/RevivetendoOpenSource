#!/usr/bin/env bash
# Brings Wii Karaoke U up for a console test WITHOUT a full bridge restart:
#   - the HTTP and NEX servers as transient systemd units (not under start.sh)
#   - the nginx site for wahp.nicoch.net
#   - the 10100D01 redirect row
#   - a rebuild + restart of account-proxy only (other services keep running)
# account-proxy will not have WAHP_SERVICE_TOKEN_KEY until a full restart, so the HTTP
# server identifies players by IP for now. That is fine in ticket_mode "unlimited".
#
# Undo with deploy/test-down.sh. Run test-down.sh BEFORE the next full bridge restart:
# start.sh starts these services itself and the ports would clash.
set -euo pipefail

ROOT=/nico-pretendo-bridge
K=$ROOT/wiikaraoke-server
PY=$K/.venv/bin/python
SECRETS=$ROOT/wiiu-chat-secure/.env

sudo systemd-run --quiet --unit=wiikaraoke-nex --uid=nico --working-directory="$K/nex" \
	--property=EnvironmentFile="$SECRETS" --property=EnvironmentFile="$K/.env" \
	--property=Restart=on-failure --property=RestartSec=5 \
	--property=StandardOutput=append:"$ROOT/log/wiikaraoke-nex.log" \
	--property=StandardError=append:"$ROOT/log/wiikaraoke-nex.log" \
	"$PY" nex_server.py nex_config.revivetendo.json

sudo systemd-run --quiet --unit=wiikaraoke-http --uid=nico --working-directory="$K" \
	--property=EnvironmentFile="$K/.env" --setenv=WAHP_CONFIG=config.revivetendo.json \
	--property=Restart=on-failure --property=RestartSec=5 \
	--property=StandardOutput=append:"$ROOT/log/wiikaraoke-http.log" \
	--property=StandardError=append:"$ROOT/log/wiikaraoke-http.log" \
	"$PY" server.py

sudo cp "$K/deploy/nginx-wahp.conf" /etc/nginx/sites-available/wahp.nicoch.net
sudo ln -sf /etc/nginx/sites-available/wahp.nicoch.net /etc/nginx/sites-enabled/wahp.nicoch.net
sudo nginx -t
sudo systemctl reload nginx

PG_URI="$(grep '^PN_WUC_POSTGRES_URI=' "$SECRETS" | cut -d= -f2-)"
psql "$PG_URI" -q -c "INSERT INTO redirects (type, from_host, to_host, game_server_id, port, access_mode)
	SELECT 'dns', 'account.pretendo.cc', '45.157.178.35', '10100D01', 60022, 'open'
	WHERE NOT EXISTS (SELECT 1 FROM redirects WHERE game_server_id = '10100D01');"

# The supervisor loop picks this up, builds first and keeps the old instance on failure.
printf '{"rebuild": true, "time": %s, "from": "wiikaraoke test-up"}\n' "$(date +%s)" >"$ROOT/run/requests/account-proxy.tmp"
mv "$ROOT/run/requests/account-proxy.tmp" "$ROOT/run/requests/account-proxy"

echo "Started. account-proxy rebuilds, then returns about 15s after it stops."
systemctl is-active wiikaraoke-nex wiikaraoke-http
