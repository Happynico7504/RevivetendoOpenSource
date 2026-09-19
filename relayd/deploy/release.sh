#!/usr/bin/env bash
# Build and sign a release for every supported platform.
#   relayd/deploy/release.sh <version> [label]              # the relay daemon (relayd)
#   relayd/deploy/release.sh -c wscedge <version> [label]   # a separately shipped component
# <version> is a strictly increasing integer PER COMPONENT (relayd and wscedge each have their
# own version line). Signing needs the release key (relayhub release keygen). To keep the key off
# the main, run this script on another machine with RELEASES_DIR pointing at a scratch
# directory, then copy that directory's contents into the main's ~/.relayhub/releases/.
#
# Components are separate Go modules with their own binary and their own dependencies: "wscedge"
# (patched nex-go v1) and "wiiuchatedge" (stock nex-go v2).
set -euo pipefail
COMPONENT=relayd
if [ "${1:-}" = "-c" ]; then
	COMPONENT="${2:?usage: release.sh -c <component> <version> [label]}"
	shift 2
fi
VERSION="${1:?usage: release.sh [-c component] <version> [label]}"
LABEL="${2:-}"
cd "$(dirname "$0")/../.."
export PATH="$PATH:/usr/local/go/bin"
RELAYHUB="${RELAYHUB:-relayhub/relayhub-bin}"
OUT="${RELEASES_DIR:-$HOME/.relayhub/releases}"

# Where each component is built from, and which variable its version is stamped into.
case "$COMPONENT" in
relayd)
	MODULE=relayd; PKG=./cmd/relayd; VARNAME=github.com/Happynico7504/relayd.Version
	(cd relaylink && go test ./... >/dev/null) || { echo "relaylink tests failed" >&2; exit 1; }
	(cd relayd && go test ./... >/dev/null) || { echo "relayd tests failed" >&2; exit 1; }
	;;
wscedge)
	MODULE=relayd/wscedge; PKG=./cmd/wscedge; VARNAME=main.Version
	(cd relayd/wscedge && go vet ./... >/dev/null) || { echo "wscedge vet failed" >&2; exit 1; }
	;;
wiiuchatedge)
	MODULE=relayd/wiiuchatedge; PKG=./cmd/wiiuchatedge; VARNAME=main.Version
	(cd relayd/wiiuchatedge && go vet ./... >/dev/null && go test ./... >/dev/null) || { echo "wiiuchatedge vet/tests failed" >&2; exit 1; }
	;;
*)
	echo "unknown component: $COMPONENT (known: relayd, wscedge, wiiuchatedge)" >&2
	exit 1
	;;
esac
(cd relayhub && go build -o relayhub-bin ./cmd/relayhub)

for arch in amd64 arm64; do
	bin="$(mktemp)"
	(cd "$MODULE" && GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X $VARNAME=$VERSION" -o "$bin" "$PKG")
	"$RELAYHUB" release sign -component "$COMPONENT" -binary "$bin" -version "$VERSION" -os linux -arch "$arch" -label "$LABEL" -out "$OUT"
	rm -f "$bin"
done
echo "done. Relays pick this up within their next update check (default every 30 minutes)."
