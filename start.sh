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

echo "==> starting services..."
"$BUILD/account-grpc" >"$LOG/account-grpc.log" 2>&1 &
ACCOUNT_PID=$!

(cd "$ROOT/friends-nex" && "$BUILD/friends-nex") >"$LOG/friends-nex.log" 2>&1 &
FRIENDS_PID=$!

"$BUILD/relay-admin" >"$LOG/relay-admin.log" 2>&1 &
ADMIN_PID=$!

GODEBUG=tls10server=1,tlsrsakex=1 "$BUILD/account-proxy" >"$LOG/account-proxy.log" 2>&1 &
PROXY_PID=$!

MK8_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
export MK8_KERBEROS_PASSWORD
(cd "$ROOT/mk8-authentication" && KERBEROS_PASSWORD="$MK8_KERBEROS_PASSWORD" "$BUILD/mk8-auth") >"$LOG/mk8-authentication.log" 2>&1 &
MK8_AUTH_PID=$!
(cd "$ROOT/mk8-secure" && KERBEROS_PASSWORD="$MK8_KERBEROS_PASSWORD" "$BUILD/mk8-secure") >"$LOG/mk8-secure.log" 2>&1 &
MK8_SECURE_PID=$!

ABSW_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
(cd "$ROOT/angry-birds-star-wars" && env $(cat .env | xargs) PN_KERBEROS_PASSWORD="$ABSW_KERBEROS_PASSWORD" "$BUILD/absw") >"$LOG/angry-birds-star-wars.log" 2>&1 &
ABSW_PID=$!

WSC_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
export WSC_KERBEROS_PASSWORD
(cd "$ROOT/wsc-authentication" && env $(cat .env | xargs) KERBEROS_PASSWORD="$WSC_KERBEROS_PASSWORD" "$BUILD/wsc-auth") >"$LOG/wsc-authentication.log" 2>&1 &
WSC_AUTH_PID=$!

(cd "$ROOT/wsc-secure" && KERBEROS_PASSWORD="$WSC_KERBEROS_PASSWORD" "$BUILD/wsc-secure") >"$LOG/wsc-secure.log" 2>&1 &
WSC_SECURE_PID=$!

python3 "$ROOT/discord-bot/bot.py" >"$LOG/discord-bot.log" 2>&1 &
BOT_PID=$!

MII_BOT_PID=""
if [ -n "${MII_BOT_TOKEN:-}" ]; then
	python3 "$ROOT/discord-bot/mii_bot.py" >"$LOG/mii-bot.log" 2>&1 &
	MII_BOT_PID=$!
fi

cleanup() {
	echo "==> shutting down..."
	kill $ACCOUNT_PID $FRIENDS_PID $ADMIN_PID $PROXY_PID $MK8_AUTH_PID $MK8_SECURE_PID $ABSW_PID $WSC_AUTH_PID $WSC_SECURE_PID $BOT_PID ${MII_BOT_PID:-} 2>/dev/null || true
}
trap cleanup EXIT INT TERM

sleep 1
echo "==> starting wiiu-chat..."
# Run from its source dir so any relative-path lookups still work
cd "$ROOT/wiiu-chat-secure"
exec "$BUILD/wiiu-chat" >>"$LOG/wiiu-chat.log" 2>&1
