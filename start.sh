#!/usr/bin/env bash
set -e

export PATH=$PATH:/usr/local/go/bin

ROOT="$(cd "$(dirname "$0")" && pwd)"
BUILD="$ROOT/build"
mkdir -p "$BUILD"

# ---- build ---------------------------------------------------------------------
# One row per compiled service: name | source dir (under $ROOT) | binary in $BUILD |
# go package (default ".") | extra source paths that are not .go files (//go:embed).
# This table is the single source of truth: the startup build below and the
# "rebuild + restart" requests handled by restartd both use it.
BUILD_ORDER=(account-grpc friends-nex wiiu-chat relay-admin account-proxy mk8-authentication mk8-secure
	angry-birds-star-wars wsc-authentication wsc-secure mc-authentication mc-secure swapdoodle
	badge-arcade-authentication badge-arcade-secure)
declare -A SRC_DIR=(
	[account-grpc]=grpc-stubs [friends-nex]=friends-nex [wiiu-chat]=wiiu-chat-secure
	[relay-admin]=relay-admin [account-proxy]=account-proxy
	[mk8-authentication]=mk8-authentication [mk8-secure]=mk8-secure
	[angry-birds-star-wars]=angry-birds-star-wars [wsc-authentication]=wsc-authentication
	[wsc-secure]=wsc-secure [mc-authentication]=minecraft-authentication [mc-secure]=minecraft-secure
	[swapdoodle]=swapdoodle [badge-arcade-authentication]=badge-arcade-authentication
	[badge-arcade-secure]=badge-arcade-secure
)
declare -A BIN_NAME=(
	[account-grpc]=account-grpc [friends-nex]=friends-nex [wiiu-chat]=wiiu-chat [relay-admin]=relay-admin
	[account-proxy]=account-proxy [mk8-authentication]=mk8-auth [mk8-secure]=mk8-secure
	[angry-birds-star-wars]=absw [wsc-authentication]=wsc-auth [wsc-secure]=wsc-secure
	[mc-authentication]=mc-auth [mc-secure]=mc-secure [swapdoodle]=swapdoodle
	[badge-arcade-authentication]=badge-arcade-auth [badge-arcade-secure]=badge-arcade-secure
)
declare -A BUILD_PKG=([account-grpc]=./cmd/account)
declare -A SRC_EXTRA=([account-proxy]="assets")

# The command that rebuilds one service (run in a subshell via eval).
declare -A BUILD_CMD=()
for name in "${BUILD_ORDER[@]}"; do
	BUILD_CMD[$name]="cd \"\$ROOT/${SRC_DIR[$name]}\" && go build -o \"\$BUILD/${BIN_NAME[$name]}\" ${BUILD_PKG[$name]:-.}"
done

# True (exit 0) when the service must be rebuilt: no binary yet, or any Go source,
# go.mod/go.sum or embedded asset is newer than the binary. (Go's own build cache
# already makes an unchanged build cheap, but it still relinks every binary, which
# is where the startup time went.) Does not notice a Go toolchain upgrade - use
# START_FORCE_BUILD=1 after one.
needs_build() {
	local name="$1" bin="$BUILD/${BIN_NAME[$1]}" dir="$ROOT/${SRC_DIR[$1]}" extra newer
	if [ ! -x "$bin" ]; then
		return 0
	fi
	newer="$(find "$dir" -type f \( -name '*.go' -o -name go.mod -o -name go.sum \) -newer "$bin" -print -quit 2>/dev/null)"
	for extra in ${SRC_EXTRA[$name]:-}; do
		if [ -z "$newer" ] && [ -e "$dir/$extra" ]; then
			newer="$(find "$dir/$extra" -type f -newer "$bin" -print -quit 2>/dev/null)"
		fi
	done
	[ -n "$newer" ]
}

# START_SKIP_BUILD=1  never build at startup (a missing binary is still built)
# START_FORCE_BUILD=1 rebuild everything, as start.sh used to
build_all() {
	local name
	for name in "${BUILD_ORDER[@]}"; do
		if [ "${START_FORCE_BUILD:-0}" = 1 ] || needs_build "$name"; then
			if [ "${START_SKIP_BUILD:-0}" = 1 ] && [ -x "$BUILD/${BIN_NAME[$name]}" ]; then
				echo "==> skipping build of $name (START_SKIP_BUILD=1)"
				continue
			fi
			echo "==> building $name..."
			(eval "${BUILD_CMD[$name]}")
		else
			echo "==> $name is up to date, not rebuilding"
		fi
	done
}
build_all

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

# ---- supervisor -------------------------------------------------------------
# Every service below runs inside autostart(), which restarts it when it exits
# and (new) also restarts it on request, so one service can be restarted or
# rebuilt without touching the rest of the bridge. Requests arrive as files in
# run/requests/<service>, written by restartd (restartd/restartd.py), which
# authenticates the caller. Nothing else in this script reacts to them.
RUN="$ROOT/run"
mkdir -p "$RUN/requests" "$RUN/status" "$RUN/pids"
# Fresh state on every start: a stale request must not restart anything and a
# stale pid/status file must not make a dead service look alive.
rm -f "$RUN/requests/"* "$RUN/status/"* "$RUN/pids/"* "$RUN/services.json"
export RESTARTD_RUN="$RUN"

# BUILD_CMD (defined with the build table near the top) says how to rebuild each
# compiled service on a "rebuild + restart" request. Services without an entry (the
# Python bots) can only be restarted. The command runs in a subshell just before the
# running process is stopped, and the running process is only stopped if the build
# succeeds - a broken build never takes a working service down.

# --- supervisor functions begin (extracted verbatim by the test harness) ---
# Usage: set_status <service> <state> [message]
set_status() {
	printf 'state=%s\nsince=%s\nmessage=%s\n' "$2" "$(date +%s)" "${3:-}" >"$RUN/status/$1.tmp" &&
		mv "$RUN/status/$1.tmp" "$RUN/status/$1"
}

# Consume a restart request. Returns 0 when the service should be (re)started
# now - i.e. its running process (if any) has been stopped - and 1 when nothing
# should change (failed rebuild: the old process, if any, keeps running).
# Usage: handle_restart_request <service> <logfile> <running child pid or "">
handle_restart_request() {
	local name="$1" log="$2" child="$3" req="$RUN/requests/$1" rebuild=0 i
	if grep -q '"rebuild": *true' "$req" 2>/dev/null; then
		rebuild=1
	fi
	rm -f "$req"
	if [ "$rebuild" -eq 1 ]; then
		if [ -z "${BUILD_CMD[$name]:-}" ]; then
			set_status "$name" build_failed "no build step for $name"
			return 1
		fi
		set_status "$name" building "rebuilding; the running instance stays up until the build succeeds"
		echo "[$(date -Iseconds)] rebuild requested, building..." >>"$log"
		if ! (eval "${BUILD_CMD[$name]}") >>"$log" 2>&1; then
			echo "[$(date -Iseconds)] rebuild FAILED, leaving things as they were" >>"$log"
			if [ -n "$child" ]; then
				set_status "$name" running "last rebuild failed - the previous build is still running (see the service log)"
			else
				set_status "$name" down "last rebuild failed (see the service log)"
			fi
			return 1
		fi
	fi
	set_status "$name" restarting
	if [ -n "$child" ]; then
		kill -TERM "$child" 2>/dev/null || true
		for i in $(seq 1 15); do
			kill -0 "$child" 2>/dev/null || break
			sleep 1
		done
		kill -KILL "$child" 2>/dev/null || true
	fi
	return 0
}

# Helper: run a command in a restart loop, appending to a log file, and restart
# it early when a restart request for <service> shows up.
# Usage: autostart <service> <logfile> <cmd...>
autostart() {
	local name="$1" log="$2"
	shift 2
	local code child requested waited req="$RUN/requests/$name" delay="${RESTART_DELAY:-45}"
	while true; do
		# Run the command in the background so this loop can watch for restart
		# requests while it runs.
		"$@" >>"$log" 2>&1 &
		child=$!
		printf '%s\n' "$child" >"$RUN/pids/$name"
		set_status "$name" running
		requested=0
		while kill -0 "$child" 2>/dev/null; do
			if [ -f "$req" ] && handle_restart_request "$name" "$log" "$child"; then
				requested=1
				break
			fi
			sleep 1
		done
		# The `if` here is load-bearing, not style: with `set -e` active script-wide,
		# a bare `wait` on a crashed child would abort this whole subshell the
		# instant a supervised binary crashes, silently killing its own restart loop
		# forever (each autostart runs in its own backgrounded subshell, so this
		# wouldn't take down the rest of the bridge - but it WOULD mean that one
		# service just stays dead until the next full "systemctl restart"). Testing
		# the exit status via `if` is one of the few constructs `set -e` exempts from
		# triggering errexit, and it keeps the real code (a bare `|| true` would
		# collapse $? to 0, so every restart logged "exited (code 0)").
		if wait "$child"; then
			code=0
		else
			code=$?
		fi
		rm -f "$RUN/pids/$name"
		if [ "$requested" -eq 1 ]; then
			echo "[$(date -Iseconds)] restarted on request" >>"$log"
			continue
		fi
		if [ "$code" -eq 0 ]; then
			echo "[$(date -Iseconds)] process exited cleanly (code 0), restarting in ${delay}s..." >>"$log"
		else
			# Bash reports a command killed by signal N as exit code 128+N (e.g.
			# 139 = SIGSEGV, 134 = SIGABRT, 137 = SIGKILL/OOM-killed) - called out
			# explicitly since these are the "genuine crash" cases worth noticing,
			# as opposed to an ordinary nonzero exit from the program itself.
			local note=""
			if [ "$code" -gt 128 ]; then
				note=" [killed by signal $((code - 128))]"
			fi
			echo "[$(date -Iseconds)] process CRASHED (exit code $code$note), restarting in ${delay}s..." >>"$log"
		fi
		set_status "$name" down "exited with code $code, restarting in ${delay}s"
		# Wait out the restart delay, but start right away if a restart is requested.
		waited=0
		while [ "$waited" -lt "$delay" ]; do
			if [ -f "$req" ] && handle_restart_request "$name" "$log" ""; then
				break
			fi
			sleep 1
			waited=$((waited + 1))
		done
	done
}
# --- supervisor functions end ---

echo "==> starting services..."

(autostart account-grpc "$LOG/account-grpc.log"      "$BUILD/account-grpc") &
ACCOUNT_PID=$!

(cd "$ROOT/friends-nex" && autostart friends-nex "$LOG/friends-nex.log" "$BUILD/friends-nex") &
FRIENDS_PID=$!

(autostart relay-admin "$LOG/relay-admin.log"       "$BUILD/relay-admin") &
ADMIN_PID=$!

(autostart account-proxy "$LOG/account-proxy.log"     env GODEBUG=tls10server=1,tlsrsakex=1 "$BUILD/account-proxy") &
PROXY_PID=$!

MK8_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
export MK8_KERBEROS_PASSWORD
(cd "$ROOT/mk8-authentication" && autostart mk8-authentication "$LOG/mk8-authentication.log" env KERBEROS_PASSWORD="$MK8_KERBEROS_PASSWORD" "$BUILD/mk8-auth") &
MK8_AUTH_PID=$!
(cd "$ROOT/mk8-secure"        && autostart mk8-secure "$LOG/mk8-secure.log"          env KERBEROS_PASSWORD="$MK8_KERBEROS_PASSWORD" "$BUILD/mk8-secure") &
MK8_SECURE_PID=$!

ABSW_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
(cd "$ROOT/angry-birds-star-wars" && autostart angry-birds-star-wars "$LOG/angry-birds-star-wars.log" \
	env $(cat .env | xargs) PN_KERBEROS_PASSWORD="$ABSW_KERBEROS_PASSWORD" "$BUILD/absw") &
ABSW_PID=$!

WSC_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
export WSC_KERBEROS_PASSWORD
(cd "$ROOT/wsc-authentication" && autostart wsc-authentication "$LOG/wsc-authentication.log" \
	env $(cat .env | xargs) KERBEROS_PASSWORD="$WSC_KERBEROS_PASSWORD" "$BUILD/wsc-auth") &
WSC_AUTH_PID=$!

(cd "$ROOT/wsc-secure" && autostart wsc-secure "$LOG/wsc-secure.log" \
	env $(cat .env | xargs) KERBEROS_PASSWORD="$WSC_KERBEROS_PASSWORD" "$BUILD/wsc-secure") &
WSC_SECURE_PID=$!

MC_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
export MC_KERBEROS_PASSWORD
(cd "$ROOT/minecraft-authentication" && autostart mc-authentication "$LOG/mc-authentication.log" \
	env $(cat .env | xargs) KERBEROS_PASSWORD="$MC_KERBEROS_PASSWORD" "$BUILD/mc-auth") &
MC_AUTH_PID=$!

(cd "$ROOT/minecraft-secure" && autostart mc-secure "$LOG/mc-secure.log" \
	env $(cat .env | xargs) KERBEROS_PASSWORD="$MC_KERBEROS_PASSWORD" "$BUILD/mc-secure") &
MC_SECURE_PID=$!

(cd "$ROOT/swapdoodle" && autostart swapdoodle "$LOG/swapdoodle.log" "$BUILD/swapdoodle") &
SWAPDOODLE_PID=$!

BA_KERBEROS_PASSWORD="$(openssl rand -hex 16)"
(cd "$ROOT/badge-arcade-authentication" && autostart badge-arcade-authentication "$LOG/badge-arcade-authentication.log" \
	env $(cat .env | xargs) KERBEROS_PASSWORD="$BA_KERBEROS_PASSWORD" "$BUILD/badge-arcade-auth") &
BA_AUTH_PID=$!

(cd "$ROOT/badge-arcade-secure" && autostart badge-arcade-secure "$LOG/badge-arcade-secure.log" \
	env $(cat .env | xargs) KERBEROS_PASSWORD="$BA_KERBEROS_PASSWORD" "$BUILD/badge-arcade-secure") &
BA_SECURE_PID=$!

(autostart discord-bot "$LOG/discord-bot.log" python3 "$ROOT/discord-bot/bot.py") &
BOT_PID=$!

MII_BOT_PID=""
if [ -n "${MII_BOT_TOKEN:-}" ]; then
	(autostart mii-bot "$LOG/mii-bot.log" python3 "$ROOT/discord-bot/mii_bot.py") &
	MII_BOT_PID=$!
fi

# Registry of the services restartd may restart (it accepts nothing outside this
# list). wiiu-chat is not in it: it runs as the foreground process at the bottom
# of this script, so it cannot be restarted on its own. restartd is not in it
# either, so a request can never stop the thing that is listening for requests.
SERVICES=(account-grpc friends-nex relay-admin account-proxy mk8-authentication mk8-secure
	angry-birds-star-wars wsc-authentication wsc-secure mc-authentication mc-secure
	swapdoodle badge-arcade-authentication badge-arcade-secure discord-bot)
if [ -n "$MII_BOT_PID" ]; then
	SERVICES+=(mii-bot)
fi
{
	printf '{"services":['
	first=1
	for svc in "${SERVICES[@]}"; do
		if [ "$first" -eq 0 ]; then printf ','; fi
		first=0
		buildable=false
		if [ -n "${BUILD_CMD[$svc]:-}" ]; then buildable=true; fi
		printf '{"name":"%s","buildable":%s}' "$svc" "$buildable"
	done
	printf ']}\n'
} >"$RUN/services.json"

# Authenticated listener for restart requests (see restartd/README.md). Secret is
# generated on first start in restartd/secret.key; port/bind via RESTARTD_PORT and
# RESTARTD_BIND (defaults 9333 / 0.0.0.0).
(autostart restartd "$LOG/restartd.log" python3 "$ROOT/restartd/restartd.py") &
RESTARTD_PID=$!

cleanup() {
	echo "==> shutting down..."
	kill ${RESTARTD_PID:-} $ACCOUNT_PID $FRIENDS_PID $ADMIN_PID $PROXY_PID $MK8_AUTH_PID $MK8_SECURE_PID $ABSW_PID $WSC_AUTH_PID $WSC_SECURE_PID $MC_AUTH_PID $MC_SECURE_PID $SWAPDOODLE_PID $BA_AUTH_PID $BA_SECURE_PID $BOT_PID ${MII_BOT_PID:-} 2>/dev/null || true
}
trap cleanup EXIT INT TERM

sleep 1
echo "==> starting wiiu-chat..."
# Run from its source dir so any relative-path lookups still work
cd "$ROOT/wiiu-chat-secure"
exec "$BUILD/wiiu-chat" >>"$LOG/wiiu-chat.log" 2>&1
