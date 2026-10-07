#!/usr/bin/env bash
# Connects this server to the existing remotely managed Cloudflare Tunnel
# "School Email Sync" as a persistent systemd service.
#
#   sudo deploy/install-cloudflared.sh              first install (asks for the token)
#   sudo deploy/install-cloudflared.sh --new-token  replace the stored token
#
# 1. Installs cloudflared from Cloudflare's apt repository if it is missing.
# 2. Reads the tunnel token from a hidden prompt (paste the token, or the whole
#    "sudo cloudflared service install eyJ..." command from the dashboard) and
#    stores it in /etc/cloudflared/school-email-sync.env, root-only. The token is
#    never placed on a command line.
# 3. Installs /etc/systemd/system/cloudflared.service, enables and starts it, and
#    waits until the tunnel reports a live connection to Cloudflare.
#
# It does not create tunnels, and it refuses to replace a cloudflared.service that
# it did not install.
set -euo pipefail

EXPECTED_TUNNEL_ID="${EXPECTED_TUNNEL_ID:-0e9e0e6f-a472-4463-8f71-aa093343bedf}" # "School Email Sync"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UNIT=/etc/systemd/system/cloudflared.service
ENV_FILE=/etc/cloudflared/school-email-sync.env
MARKER="deploy/install-cloudflared.sh (school-gmail-mcp)"

if [[ $EUID -ne 0 ]]; then
  echo "Run with sudo: sudo $0" >&2
  exit 1
fi
if [[ -f $UNIT ]] && ! grep -qF "$MARKER" "$UNIT"; then
  echo "$UNIT already exists and was not installed by this script; leaving it alone." >&2
  exit 1
fi

# 1. Package.
if ! command -v cloudflared >/dev/null 2>&1; then
  echo "Installing cloudflared from pkg.cloudflare.com ..."
  mkdir -p --mode=0755 /usr/share/keyrings
  curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
  echo 'deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main' \
    >/etc/apt/sources.list.d/cloudflared.list
  apt-get update
  apt-get install -y cloudflared
fi
echo "Using $(cloudflared --version)"

# 2. Token.
if [[ -s $ENV_FILE && ${1:-} != "--new-token" ]]; then
  echo "Using the token already stored in $ENV_FILE (pass --new-token to replace it)."
else
  if [[ -t 0 ]]; then
    read -rsp "Paste the tunnel token (input is hidden), then press Enter: " input
    echo
  else
    read -r input
  fi
  token="$(grep -oE 'eyJ[A-Za-z0-9+/_=-]{40,}' <<<"$input" | tail -n1 || true)"
  unset input
  if [[ -z $token ]]; then
    echo "That does not look like a tunnel token (a long string starting with eyJ)." >&2
    exit 1
  fi
  # The token is base64 JSON {"a": account, "t": tunnel id, "s": secret}; check "t" only.
  tunnel_id="$(python3 -c '
import base64, json, sys
t = sys.stdin.read().strip()
t += "=" * (-len(t) % 4)
try:
    print(json.loads(base64.b64decode(t.replace("-", "+").replace("_", "/")))["t"])
except Exception:
    pass
' <<<"$token")"
  if [[ -z $tunnel_id ]]; then
    echo "Could not read the tunnel ID from that token; is it complete?" >&2
    exit 1
  fi
  if [[ $tunnel_id != "$EXPECTED_TUNNEL_ID" ]]; then
    echo "This token belongs to tunnel $tunnel_id, not School Email Sync ($EXPECTED_TUNNEL_ID)." >&2
    echo "If that is intended, re-run: sudo EXPECTED_TUNNEL_ID=$tunnel_id $0 --new-token" >&2
    exit 1
  fi
  install -d -o root -g root -m 0755 /etc/cloudflared
  (
    umask 077
    printf 'TUNNEL_TOKEN=%s\n' "$token" >"$ENV_FILE.tmp"
    mv -f "$ENV_FILE.tmp" "$ENV_FILE"
  )
  unset token
  chown root:root "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  echo "Stored the token for tunnel $tunnel_id in $ENV_FILE (root-only)."
fi

# 3. Service.
{
  echo "# $MARKER"
  cat "$REPO/deploy/cloudflared.service"
} >"$UNIT.tmp"
chmod 644 "$UNIT.tmp"
mv -f "$UNIT.tmp" "$UNIT"
systemctl daemon-reload
systemctl enable cloudflared.service
systemctl restart cloudflared.service

echo "Waiting for the tunnel to connect ..."
if curl -fsS --retry 30 --retry-delay 1 --retry-all-errors --max-time 2 http://127.0.0.1:20241/ready; then
  echo
  echo "cloudflared: enabled=$(systemctl is-enabled cloudflared) active=$(systemctl is-active cloudflared); tunnel connected."
else
  echo "The tunnel did not report a connection; recent log:" >&2
  journalctl -u cloudflared -n 40 --no-pager >&2 || true
  exit 1
fi
