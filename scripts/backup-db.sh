#!/usr/bin/env bash
set -e

BACKUP_DIR="/var/backups/revivetendo-db"
KEEP=30  # keep 30 daily backups

mkdir -p "$BACKUP_DIR"

STAMP=$(date +%Y-%m-%d_%H-%M-%S)
FILE="$BACKUP_DIR/backup_${STAMP}.sql"

pg_dump -U postgres -d wiiuchat \
  --no-owner --no-privileges \
  -t redirects \
  -t user_access \
  -t banned_users \
  -t web_users \
  -t web_logins \
  -t pnid_cache \
  -t mii_names \
  > "$FILE"

gzip "$FILE"
echo "Backup written to ${FILE}.gz"

# Prune old backups
ls -1t "$BACKUP_DIR"/backup_*.sql.gz 2>/dev/null | tail -n +$((KEEP + 1)) | xargs -r rm --
