#!/bin/sh
# Tests for relayd-run. Run: sh relayd/deploy/launcher_test.sh
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: got '$2', want '$3'"; fail=1; fi; }

mk() { # mk <path> <name> <exitcode>: a fake relayd that reports which binary ran
	mkdir -p "$(dirname "$1")"
	printf '#!/bin/sh\necho "%s" >> "$RUNLOG"\nexit %s\n' "$2" "$3" > "$1"
	chmod +x "$1"
}
new() { T=$(mktemp -d); export STATE_DIRECTORY="$T/state" RELAYD_SYSTEM_BIN="$T/system-relayd" RUNLOG="$T/runs"; : > "$RUNLOG"; mkdir -p "$STATE_DIRECTORY/bin"; mk "$RELAYD_SYSTEM_BIN" system 0; }
run() { sh "$HERE/relayd-run" >/dev/null 2>&1; }
runs() { tr '\n' ' ' < "$RUNLOG" | sed 's/ $//'; }

# 1. no OTA binary: the installed one runs
new; run; check "falls back to the installed binary" "$(runs)" "system"

# 2. OTA binary, nothing pending: it is preferred, and nothing is counted
new; mk "$STATE_DIRECTORY/bin/relayd" ota5 0; run; run; run; run; run
check "prefers the OTA binary" "$(runs)" "ota5 ota5 ota5 ota5 ota5"
check "no boot counting without a pending update" "$([ -e "$STATE_DIRECTORY/bin/boots" ] && echo yes || echo no)" "no"

# 3. pending update that keeps failing: 3 launches, then rolled back to the previous OTA binary
new; mk "$STATE_DIRECTORY/bin/relayd" ota6 1; mk "$STATE_DIRECTORY/bin/relayd.prev" ota5 0; echo 6 > "$STATE_DIRECTORY/bin/pending"
run; run; run; run; run
check "rolls back after 3 failed launches" "$(runs)" "ota6 ota6 ota6 ota5 ota5"
check "records the rejected version" "$(cat "$STATE_DIRECTORY/bin/rejected")" "6"
check "clears the pending marker" "$([ -e "$STATE_DIRECTORY/bin/pending" ] && echo yes || echo no)" "no"
check "the failed binary is gone" "$([ -e "$STATE_DIRECTORY/bin/relayd.prev" ] && echo yes || echo no)" "no"

# 4. no previous OTA binary: fall back to the binary install.sh put on the system
new; mk "$STATE_DIRECTORY/bin/relayd" ota2 1; echo 2 > "$STATE_DIRECTORY/bin/pending"
run; run; run; run; run
check "falls back to the installed binary when there is no previous OTA one" "$(runs)" "ota2 ota2 ota2 system system"
check "the failed OTA binary was removed" "$([ -e "$STATE_DIRECTORY/bin/relayd" ] && echo yes || echo no)" "no"

# 5. the new version proves itself (pending removed) after 2 launches: never rolled back
new; mk "$STATE_DIRECTORY/bin/relayd" ota3 0; mk "$STATE_DIRECTORY/bin/relayd.prev" ota2 0; echo 3 > "$STATE_DIRECTORY/bin/pending"
run; run; rm -f "$STATE_DIRECTORY/bin/pending" "$STATE_DIRECTORY/bin/boots"; run; run; run; run
check "a committed update is never rolled back" "$(runs)" "ota3 ota3 ota3 ota3 ota3 ota3"
check "and nothing is recorded as rejected" "$([ -e "$STATE_DIRECTORY/bin/rejected" ] && echo yes || echo no)" "no"

# 6. arguments are passed through unchanged
new; printf '#!/bin/sh\necho "$@" >> "$RUNLOG"\n' > "$RELAYD_SYSTEM_BIN"; sh "$HERE/relayd-run" -config /x/y.json >/dev/null 2>&1
check "arguments pass through" "$(runs)" "-config /x/y.json"

exit $fail
