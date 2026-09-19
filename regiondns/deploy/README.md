# revivetendo-dns

`regiondns` deployed as the `revivetendo-dns` systemd service: an authoritative
DNS server for one delegated zone (`regionselect.nicochristmann.net`) that
answers with region-dependent A/AAAA records.

- Deploy / update binary: `regiondns/deploy/deploy.sh` (`--geoip` refreshes the GeoIP DB)
- Config: `/etc/revivetendo-dns/regiondns.json` (reloads automatically, or `systemctl reload revivetendo-dns`)
- Logs: `journalctl -u revivetendo-dns`
- Delegation (Cloudflare, DNS-only): `regionselect NS netcup-server.nicochristmann.net`
  (the NS target must be an A record, never a CNAME); service hostnames `CNAME regionselect.nicochristmann.net`.
- Port 53 on this host: dnsmasq is restricted to loopback via `/etc/dnsmasq.d/local-only.conf`.
- GeoIP data: DB-IP "IP to Country Lite" (attribution required by its license, see db-ip.com).
