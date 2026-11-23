#!/usr/bin/env bash
# Undoes the service and nginx parts of test-up.sh. account-proxy keeps its new code, and the
# redirect row stays: both are harmless, because consoles only reach them through the game.
set -uo pipefail

sudo systemctl stop wiikaraoke-nex wiikaraoke-http
sudo rm -f /etc/nginx/sites-enabled/wahp.nicoch.net
sudo nginx -t && sudo systemctl reload nginx
echo "Stopped."
