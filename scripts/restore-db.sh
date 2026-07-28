#!/usr/bin/env bash
set -e

BACKUP_DIR="/var/backups/revivetendo-db"

if [ -n "$1" ]; then
    FILE="$1"
else
    FILE=$(ls -1t "$BACKUP_DIR"/backup_*.sql.gz 2>/dev/null | head -1)
    if [ -z "$FILE" ]; then
        echo "No backup found in $BACKUP_DIR" >&2
        exit 1
    fi
fi

echo "Restoring from: $FILE"

# Truncate the tables we're about to restore so we don't get duplicate key errors.
psql -U postgres -d wiiuchat -c "
TRUNCATE redirects, user_access, banned_users, web_users, web_logins, pnid_cache, mii_names RESTART IDENTITY CASCADE;
"

zcat "$FILE" | psql -U postgres -d wiiuchat

echo "Restore complete."
