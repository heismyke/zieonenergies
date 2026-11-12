#!/usr/bin/env bash
# Nightly logical backup of the Zieon database. Install via cron on the host:
#   15 2 * * * /home/myke/apps/zieon/deploy/backup.sh >> /home/myke/apps/zieon/backups/backup.log 2>&1
# Restore: see docs/PRODUCTION_LAUNCH_CHECKLIST.md ("Backup and restore").
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p backups
stamp=$(date -u +%Y%m%dT%H%M%SZ)
docker compose -f docker-compose.prod.yml exec -T db pg_dump -U zieon -d zieon -Fc > "backups/zieon-$stamp.dump"
chmod 600 "backups/zieon-$stamp.dump"
# keep 14 days locally; copy off-host as well (not configured yet — see checklist)
find backups -name 'zieon-*.dump' -mtime +14 -delete
echo "$(date -u) backup ok: zieon-$stamp.dump"
