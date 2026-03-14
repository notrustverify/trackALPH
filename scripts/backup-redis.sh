#!/usr/bin/env bash
set -euo pipefail

# Backup Redis RDB from docker-compose service "redis".
# Usage:
#   ./scripts/backup-redis.sh [backup_dir]
#
# Example:
#   ./scripts/backup-redis.sh backups

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKUP_DIR="${1:-$PROJECT_ROOT/backups}"
TIMESTAMP="$(date +%Y%m%d-%H%M%S)"
OUT_FILE="$BACKUP_DIR/dump-$TIMESTAMP.rdb"

mkdir -p "$BACKUP_DIR"

cd "$PROJECT_ROOT"

if ! docker compose ps redis >/dev/null 2>&1; then
  echo "Error: could not access docker compose redis service."
  echo "Run this script from the project with docker compose available."
  exit 1
fi

echo "Triggering Redis background save..."
LASTSAVE_BEFORE="$(docker compose exec -T redis redis-cli LASTSAVE | tr -d '\r')"
docker compose exec -T redis redis-cli BGSAVE >/dev/null || true

echo "Waiting for background save to finish..."
for _ in $(seq 1 30); do
  sleep 1
  LASTSAVE_AFTER="$(docker compose exec -T redis redis-cli LASTSAVE | tr -d '\r')"
  if [[ "$LASTSAVE_AFTER" != "$LASTSAVE_BEFORE" ]]; then
    break
  fi
done

echo "Copying dump.rdb to $OUT_FILE"
docker compose cp redis:/data/dump.rdb "$OUT_FILE"

# Keep only the latest backup file in target directory.
echo "Removing older backups..."
find "$BACKUP_DIR" -maxdepth 1 -type f -name 'dump-*.rdb' ! -name "$(basename "$OUT_FILE")" -delete

echo "Backup complete: $OUT_FILE"
