#!/usr/bin/env bash
# Build regiondns and (re)install it as the revivetendo-dns systemd service.
# Usage: ./deploy.sh            build + install binary/unit, restart if running
#        ./deploy.sh --geoip    also refresh the GeoIP database (DB-IP lite)
# The config lives in /etc/revivetendo-dns/regiondns.json and is NOT
# overwritten here; start from regiondns.example.json on first install.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$PATH:/usr/local/go/bin"

go vet ./...
go build -trimpath -ldflags "-s -w" -o /tmp/revivetendo-dns .
sudo install -m 0755 /tmp/revivetendo-dns /usr/local/bin/revivetendo-dns
rm -f /tmp/revivetendo-dns
sudo install -m 0644 deploy/revivetendo-dns.service /etc/systemd/system/revivetendo-dns.service
sudo install -d -m 0755 /etc/revivetendo-dns /usr/local/share/revivetendo-dns
[ -f /etc/revivetendo-dns/regiondns.json ] || sudo install -m 0644 deploy/regiondns.example.json /etc/revivetendo-dns/regiondns.json

if [ "${1:-}" = "--geoip" ] || [ ! -f /usr/local/share/revivetendo-dns/dbip-country-lite.mmdb ]; then
	tmp="$(mktemp)"
	curl -fsSL --max-time 120 "https://download.db-ip.com/free/dbip-country-lite-$(date +%Y-%m).mmdb.gz" | gunzip > "$tmp"
	sudo install -m 0644 "$tmp" /usr/local/share/revivetendo-dns/dbip-country-lite.mmdb
	rm -f "$tmp"
fi

sudo systemctl daemon-reload
if systemctl is-active --quiet revivetendo-dns; then
	sudo systemctl restart revivetendo-dns
fi
echo "installed; enable/start with: sudo systemctl enable --now revivetendo-dns"
