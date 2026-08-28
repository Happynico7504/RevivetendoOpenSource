#!/usr/bin/env bash
set -e

export PATH=$PATH:/usr/local/go/bin

ROOT="$(cd "$(dirname "$0")" && pwd)"
BUILD="$ROOT/build"
mkdir -p "$BUILD"

echo "==> building account gRPC stub..."
(cd "$ROOT/grpc-stubs" && go build -o "$BUILD/account-grpc" ./cmd/account)

echo "==> building friends-nex server..."
(cd "$ROOT/friends-nex" && go build -o "$BUILD/friends-nex" .)

echo "==> building wiiu-chat secure server..."
(cd "$ROOT/wiiu-chat-secure" && go build -o "$BUILD/wiiu-chat" .)

echo "==> building account proxy..."
(cd "$ROOT/account-proxy" && go build -o "$BUILD/account-proxy" .)

echo "==> building relay-admin..."
(cd "$ROOT/relay-admin" && go build -o "$BUILD/relay-admin" .)

echo "==> building mk8-authentication..."
(cd "$ROOT/mk8-authentication" && go build -o "$BUILD/mk8-auth" .)
echo "==> building mk8-secure..."
(cd "$ROOT/mk8-secure" && go build -o "$BUILD/mk8-secure" .)

echo "==> building angry-birds-star-wars..."
(cd "$ROOT/angry-birds-star-wars" && go build -o "$BUILD/absw" .)

echo "==> building wsc-authentication..."
(cd "$ROOT/wsc-authentication" && go build -o "$BUILD/wsc-auth" .)

echo "==> building wsc-secure..."
(cd "$ROOT/wsc-secure" && go build -o "$BUILD/wsc-secure" .)

echo "==> building minecraft-authentication..."
(cd "$ROOT/minecraft-authentication" && go build -o "$BUILD/mc-auth" .)
echo "==> building minecraft-secure..."
(cd "$ROOT/minecraft-secure" && go build -o "$BUILD/mc-secure" .)

echo "==> building swapdoodle server..."
(cd "$ROOT/swapdoodle" && go build -o "$BUILD/swapdoodle" .)

# Export all vars from the secure server .env into the environment
set -a
# shellcheck disable=SC1091
source "$ROOT/wiiu-chat-secure/.env"
# Also pick up discord-bot/.env (for MII_BOT_TOKEN etc.)
# shellcheck disable=SC1091
[ -f "$ROOT/discord-bot/.env" ] && source "$ROOT/discord-bot/.env"
set +a

LOG="$ROOT/log"
mkdir -p "$LOG"

# Helper: run a command in a restart loop, appending to a log file.
# Usage: autostart <logfile> <cmd...>
autostart() {
	local log="$1"
	shift
	while true; do
		"$@" >>"$log" 2>&1 || true
		echo "[$(date -Iseconds)] process exited (code $?), restarting in 45s..." >>"$log"
		sleep 45
	done
}

echo "==> starting services..."

(autostart "$LOG/account-grpc.log"      "$BUILD/account-grpc") &
ACCOUNT_PID=$!

(cd "$ROOT/friends-nex" && autostart "$LOG/friends-nex.log" "$BUILD/friends-nex") &
FRIENDS_PID=$!

(autostart "$LOG/relay-admin.log"       "$BUILD/relay-admin") &
ADMIN_PID=$!

(autostart "$LOG/account-proxy.log"     env GODEBUG=tls10server=1,tlsrsakex=1 "$BUILD/account-proxy") &
PROXY_PID=$!

MK8_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
export MK8_KERBEROS_PASSWORD
(cd "$ROOT/mk8-authentication" && autostart "$LOG/mk8-authentication.log" env KERBEROS_PASSWORD="$MK8_KERBEROS_PASSWORD" "$BUILD/mk8-auth") &
MK8_AUTH_PID=$!
(cd "$ROOT/mk8-secure"        && autostart "$LOG/mk8-secure.log"          env KERBEROS_PASSWORD="$MK8_KERBEROS_PASSWORD" "$BUILD/mk8-secure") &
MK8_SECURE_PID=$!

ABSW_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
(cd "$ROOT/angry-birds-star-wars" && autostart "$LOG/angry-birds-star-wars.log" \
	env $(cat .env | xargs) PN_KERBEROS_PASSWORD="$ABSW_KERBEROS_PASSWORD" "$BUILD/absw") &
ABSW_PID=$!

WSC_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
export WSC_KERBEROS_PASSWORD
(cd "$ROOT/wsc-authentication" && autostart "$LOG/wsc-authentication.log" \
	env $(cat .env | xargs) KERBEROS_PASSWORD="$WSC_KERBEROS_PASSWORD" "$BUILD/wsc-auth") &
WSC_AUTH_PID=$!

(cd "$ROOT/wsc-secure" && autostart "$LOG/wsc-secure.log" \
	env $(cat .env | xargs) KERBEROS_PASSWORD="$WSC_KERBEROS_PASSWORD" "$BUILD/wsc-secure") &
WSC_SECURE_PID=$!

MC_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
export MC_KERBEROS_PASSWORD
(cd "$ROOT/minecraft-authentication" && autostart "$LOG/mc-authentication.log" \
	env $(cat .env | xargs) KERBEROS_PASSWORD="$MC_KERBEROS_PASSWORD" "$BUILD/mc-auth") &
MC_AUTH_PID=$!

(cd "$ROOT/minecraft-secure" && autostart "$LOG/mc-secure.log" \
	env $(cat .env | xargs) KERBEROS_PASSWORD="$MC_KERBEROS_PASSWORD" "$BUILD/mc-secure") &
MC_SECURE_PID=$!

(cd "$ROOT/swapdoodle" && autostart "$LOG/swapdoodle.log" "$BUILD/swapdoodle") &
SWAPDOODLE_PID=$!

(autostart "$LOG/discord-bot.log" python3 "$ROOT/discord-bot/bot.py") &
BOT_PID=$!

MII_BOT_PID=""
if [ -n "${MII_BOT_TOKEN:-}" ]; then
	(autostart "$LOG/mii-bot.log" python3 "$ROOT/discord-bot/mii_bot.py") &
	MII_BOT_PID=$!
fi

cleanup() {
	echo "==> shutting down..."
	kill $ACCOUNT_PID $FRIENDS_PID $ADMIN_PID $PROXY_PID $MK8_AUTH_PID $MK8_SECURE_PID $ABSW_PID $WSC_AUTH_PID $WSC_SECURE_PID $MC_AUTH_PID $MC_SECURE_PID $SWAPDOODLE_PID $BOT_PID ${MII_BOT_PID:-} 2>/dev/null || true
}
trap cleanup EXIT INT TERM

sleep 1
echo "==> starting wiiu-chat..."
# Run from its source dir so any relative-path lookups still work
cd "$ROOT/wiiu-chat-secure"
exec "$BUILD/wiiu-chat" >>"$LOG/wiiu-chat.log" 2>&1
