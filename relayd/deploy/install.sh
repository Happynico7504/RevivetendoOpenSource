#!/usr/bin/env bash
# Install relayd on a relay server. Run as root, from a directory that contains:
#   relayd            the binary (built on the main:  cd relayd && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/Happynico7504/relayd.Version=N" -o relayd ./cmd/relayd)
#   relayd-run        the launcher script (does the OTA binary selection and rollback)
#   relayd.service    this directory's unit file
#   bundle.json       the relay's secret bundle from the relay-admin page (shown once)
#   relayd.json       your listener config (start from relayd.example.json)
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
for f in relayd relayd-run relayd.service bundle.json relayd.json; do
	[ -f "$f" ] || { echo "missing ./$f" >&2; exit 1; }
done
install -m 0755 relayd /usr/local/bin/relayd
install -m 0755 relayd-run /usr/local/bin/relayd-run
# A manual install always wins over an earlier over-the-air one.
rm -rf /var/lib/private/relayd/bin /var/lib/private/relayd/update.json /var/lib/relayd/bin /var/lib/relayd/update.json 2>/dev/null || true
install -m 0644 relayd.service /etc/systemd/system/relayd.service
install -d -m 0755 /etc/relayd
install -m 0600 bundle.json /etc/relayd/bundle.json
install -m 0644 relayd.json /etc/relayd/relayd.json
grep -q '/run/credentials/relayd.service/bundle' /etc/relayd/relayd.json ||
	echo "WARNING: set \"bundle\": \"/run/credentials/relayd.service/bundle\" and \"data_dir\": \"/var/lib/relayd\" in /etc/relayd/relayd.json"
systemctl daemon-reload
systemctl enable relayd
echo "installed. Start it with: systemctl start relayd   (logs: journalctl -u relayd -f)"
echo "Delete the local copy of bundle.json now: it contains the relay's private key."
