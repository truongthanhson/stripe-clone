#!/usr/bin/env bash
# Dumps the production database to BACKUP_DIR and deletes dumps older than BACKUP_RETENTION_DAYS.
# Run nightly from cron, e.g.: 0 3 * * * cd /srv/stripe-clone && scripts/backup.sh >> backups/backup.log 2>&1
set -euo pipefail

cd "$(dirname "$0")/.."
set -a
. ./.env
set +a

dir="${BACKUP_DIR:-./backups}"
keep_days="${BACKUP_RETENTION_DAYS:-14}"
mkdir -p "$dir"

file="$dir/${POSTGRES_DB}-$(date -u +%Y%m%dT%H%M%SZ).dump"
docker compose -f compose.prod.yaml exec -T postgres \
	pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --format=custom >"$file.partial"
mv "$file.partial" "$file"
echo "wrote $file ($(du -h "$file" | cut -f1))"

find "$dir" -name "${POSTGRES_DB}-*.dump" -mtime +"$keep_days" -print -delete
