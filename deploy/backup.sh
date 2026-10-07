#!/usr/bin/env bash
# Writes a consistent, compressed snapshot of the mail database while the
# service keeps running, and keeps the newest $KEEP snapshots (default 14).
#
#   deploy/backup.sh [DEST_DIR]        default DEST_DIR: ~/backups/school-email-sync
set -euo pipefail

DEST="${1:-$HOME/backups/school-email-sync}"
KEEP="${KEEP:-14}"
DB="${DB_PATH:-$HOME/.local/state/school-email-sync/mail.db}"

umask 077
install -d -m 0700 "$DEST"
snapshot="$DEST/mail-$(date +%Y%m%d-%H%M%S).db"
"$HOME/.local/bin/school-email-sync" backup -db "$DB" "$snapshot"
gzip -6 "$snapshot"
echo "wrote $snapshot.gz ($(du -h "$snapshot.gz" | cut -f1))"

# Prune old snapshots.
ls -1t "$DEST"/mail-*.db.gz 2>/dev/null | tail -n +"$((KEEP + 1))" | xargs -r rm -f --
