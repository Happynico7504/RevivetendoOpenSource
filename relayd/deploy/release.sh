#!/usr/bin/env bash
# Build and sign a relayd release for every supported platform.
#   relayd/deploy/release.sh <version> [label]
# <version> is a strictly increasing integer. Signing needs the release key
# (relayhub release keygen). To keep the key off the main, run this script on
# another machine with RELEASES_DIR pointing at a scratch directory, then copy
# that directory's linux-*/ folders into the main's ~/.relayhub/releases/.
set -euo pipefail
VERSION="${1:?usage: release.sh <version> [label]}"
LABEL="${2:-}"
cd "$(dirname "$0")/../.."
export PATH="$PATH:/usr/local/go/bin"
RELAYHUB="${RELAYHUB:-relayhub/relayhub-bin}"
OUT="${RELEASES_DIR:-$HOME/.relayhub/releases}"

(cd relaylink && go test ./... >/dev/null) || { echo "relaylink tests failed" >&2; exit 1; }
(cd relayd && go test ./... >/dev/null) || { echo "relayd tests failed" >&2; exit 1; }
(cd relayhub && go build -o relayhub-bin ./cmd/relayhub)

for arch in amd64 arm64; do
	bin="$(mktemp)"
	(cd relayd && GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X github.com/Happynico7504/relayd.Version=$VERSION" -o "$bin" ./cmd/relayd)
	"$RELAYHUB" release sign -binary "$bin" -version "$VERSION" -os linux -arch "$arch" -label "$LABEL" -out "$OUT"
	rm -f "$bin"
done
echo "done. Relays pick this up within their next update check (default every 30 minutes)."
