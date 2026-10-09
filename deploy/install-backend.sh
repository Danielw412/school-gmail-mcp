#!/usr/bin/env bash
# Installs or updates the backend as a systemd user service. No root needed.
#
#   make install            (builds first, then runs this script)
#
# Safe to re-run: the binary is replaced atomically, secrets are only generated
# when ~/.config/school-email-sync/backend.env does not exist yet, and the
# service is restarted at the end.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAME=school-email-sync
BIN_SRC="$REPO/backend/bin/$NAME"
BIN_DST="$HOME/.local/bin/$NAME"
CONF_DIR="$HOME/.config/$NAME"
ENV_FILE="$CONF_DIR/backend.env"
UNIT_DIR="$HOME/.config/systemd/user"
UNIT="$NAME.service"

if [[ $EUID -eq 0 ]]; then
  echo "Run this as your normal user, not root." >&2
  exit 1
fi
if [[ ! -x $BIN_SRC ]]; then
  echo "Missing $BIN_SRC; run 'make build' first." >&2
  exit 1
fi

# 1. Binary.
install -d -m 0755 "$HOME/.local/bin"
install -m 0755 "$BIN_SRC" "$BIN_DST.new"
mv -f "$BIN_DST.new" "$BIN_DST"
echo "installed $BIN_DST ($("$BIN_DST" version))"

# 2. Configuration and secrets: created once, never overwritten or printed.
install -d -m 0700 "$CONF_DIR"
if [[ ! -f $ENV_FILE ]]; then
  (
    umask 077
    {
      echo "# School Email Sync backend settings (see .env.example in the repository)."
      echo "# SYNC_SECRET goes into the Apps Script Script Property of the same name."
      echo "SYNC_SECRET=$("$BIN_DST" gen-secret)"
      echo "# For local tools only (search, queueing send/reply). Never give this to Apps Script."
      echo "API_SECRET=$("$BIN_DST" gen-secret)"
      echo "LISTEN_ADDR=127.0.0.1:8766"
    } >"$ENV_FILE.tmp"
    mv -f "$ENV_FILE.tmp" "$ENV_FILE"
  )
  echo "created $ENV_FILE with new random secrets"
fi
chmod 600 "$ENV_FILE"

# 3. systemd user unit. The state directory (database) must exist before the first
# start: otherwise systemd 254+ sees ~/.config/$NAME, assumes an upgrade from an
# old layout, and symlinks the state directory to the config directory.
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/$NAME"
if [[ -L $STATE_DIR ]]; then
  echo "$STATE_DIR is a symlink to $(readlink -f "$STATE_DIR"); stop the service and move the database first." >&2
  exit 1
fi
install -d -m 0700 "$STATE_DIR"
install -d -m 0755 "$UNIT_DIR"
sed "s|@REPO@|$REPO|g" "$REPO/deploy/$UNIT" >"$UNIT_DIR/$UNIT.tmp"
mv -f "$UNIT_DIR/$UNIT.tmp" "$UNIT_DIR/$UNIT"
systemctl --user daemon-reload
systemctl --user enable "$UNIT" >/dev/null
systemctl --user restart "$UNIT"

if [[ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != "yes" ]]; then
  echo "WARNING: lingering is off, so the service will not start at boot until you log in." >&2
  echo "         Fix with: sudo loginctl enable-linger $USER" >&2
fi

# Attachment conversion (optional tools).
command -v pdftotext >/dev/null && command -v pdftoppm >/dev/null ||
  echo "NOTE: install poppler-utils for PDF text in get_attachment (sudo apt install poppler-utils)." >&2
command -v bwrap >/dev/null ||
  echo "NOTE: install bubblewrap so PDF converters run sandboxed (sudo apt install bubblewrap)." >&2

# 4. Wait for the health check.
addr="$(sed -n 's/^LISTEN_ADDR=//p' "$ENV_FILE" | tail -n1)"
addr="${addr:-127.0.0.1:8766}"
if curl -fs --retry 20 --retry-delay 1 --retry-connrefused --max-time 2 "http://$addr/healthz" >/dev/null; then
  echo "$UNIT is running and healthy on http://$addr"
else
  echo "$UNIT did not become healthy; recent log:" >&2
  journalctl --user -u "$UNIT" -n 30 --no-pager >&2 || true
  exit 1
fi
