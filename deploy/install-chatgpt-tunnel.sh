#!/usr/bin/env bash
# Connects the backend's MCP endpoint to ChatGPT through an OpenAI Secure MCP
# Tunnel, as a systemd user service. No root needed. Safe to re-run.
#
#   deploy/install-chatgpt-tunnel.sh --tunnel-id tunnel_XXXX              asks for the key (hidden)
#   deploy/install-chatgpt-tunnel.sh --tunnel-id tunnel_XXXX --key-from ~/.config/google-tasks-mcp/tunnel.env
#   deploy/install-chatgpt-tunnel.sh --tunnel-id tunnel_XXXX --key-from-env   uses $CONTROL_PLANE_API_KEY
#   deploy/install-chatgpt-tunnel.sh --new-key ...                          replace the stored key
#
# Needs, from the OpenAI platform (see README, "ChatGPT"):
#   * a tunnel:  https://platform.openai.com/settings/organization/tunnels  -> tunnel_...
#   * a runtime API key with Tunnels Read + Use:
#                https://platform.openai.com/settings/organization/api-keys
#
# It writes ~/.config/school-email-sync/chatgpt-tunnel.env (0600, the key),
# ~/.config/tunnel-client/school-gmail.yaml (the profile) and the user unit
# school-email-sync-chatgpt.service, then starts it and waits for readiness.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TC="${TUNNEL_CLIENT:-/usr/local/bin/tunnel-client}"
PROFILE=school-gmail
UNIT=school-email-sync-chatgpt.service
CONF_DIR="$HOME/.config/school-email-sync"
KEY_FILE="$CONF_DIR/chatgpt-tunnel.env"
HEALTH_ADDR=127.0.0.1:8081

tunnel_id=""
key_from=""
key_from_env=""
new_key=""
while [[ $# -gt 0 ]]; do
  case $1 in
    --tunnel-id) tunnel_id=${2:-}; shift 2 ;;
    --key-from) key_from=${2:-}; shift 2 ;;
    --key-from-env) key_from_env=1; shift ;;
    --new-key) new_key=1; shift ;;
    -h | --help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
  esac
done

if [[ $EUID -eq 0 ]]; then
  echo "Run this as your normal user, not root." >&2
  exit 1
fi
if [[ ! -x $TC ]]; then
  echo "tunnel-client not found at $TC" >&2
  exit 1
fi
existing_id="$(sed -n 's/^ *tunnel_id: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$HOME/.config/tunnel-client/$PROFILE.yaml" 2>/dev/null || true)"
tunnel_id="${tunnel_id:-$existing_id}"
if [[ ! $tunnel_id =~ ^tunnel_[A-Za-z0-9]{8,64}$ ]]; then
  echo "Pass --tunnel-id tunnel_... (create one at https://platform.openai.com/settings/organization/tunnels)." >&2
  exit 2
fi

# The MCP endpoint must be up (backend >= MCP support, MCP_LISTEN_ADDR not "off").
mcp_addr="$(sed -n 's/^MCP_LISTEN_ADDR=//p' "$CONF_DIR/backend.env" 2>/dev/null | tail -n1 || true)"
mcp_addr="${mcp_addr:-127.0.0.1:8767}"
if [[ $mcp_addr == off ]]; then
  echo "MCP_LISTEN_ADDR=off in $CONF_DIR/backend.env; remove it first." >&2
  exit 1
fi
mcp_url="http://$mcp_addr/mcp"
if ! curl -fsS --max-time 5 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"ping"}' "$mcp_url" >/dev/null; then
  echo "The MCP endpoint $mcp_url is not answering; run 'make install' first." >&2
  exit 1
fi

# 1. Runtime API key: stored once, never printed.
install -d -m 0700 "$CONF_DIR"
if [[ -s $KEY_FILE && -z $new_key ]]; then
  echo "Using the key already stored in $KEY_FILE (pass --new-key to replace it)."
else
  if [[ -n $key_from ]]; then
    key="$(sed -n 's/^CONTROL_PLANE_API_KEY=//p' "$key_from" | tail -n1)"
  elif [[ -n $key_from_env ]]; then
    key="${CONTROL_PLANE_API_KEY:-}"
  elif [[ -t 0 ]]; then
    read -rsp "Paste the OpenAI runtime API key (Tunnels Read + Use; input is hidden): " key
    echo
  else
    read -r key
  fi
  key="${key%\"}"
  key="${key#\"}"
  key="$(tr -d '[:space:]' <<<"$key")"
  if [[ -z $key ]]; then
    echo "No key given." >&2
    exit 1
  fi
  if [[ $key == sk-admin-* ]]; then
    echo "That is an admin key. The long-running tunnel needs a runtime key (Tunnels Read + Use), not an admin key." >&2
    exit 1
  fi
  (
    umask 077
    printf 'CONTROL_PLANE_API_KEY=%s\n' "$key" >"$KEY_FILE.tmp"
    mv -f "$KEY_FILE.tmp" "$KEY_FILE"
  )
  unset key
  chmod 600 "$KEY_FILE"
  echo "Stored the runtime key in $KEY_FILE (0600)."
fi

# 2. Profile.
"$TC" init --sample sample_mcp_remote_no_auth --profile "$PROFILE" --tunnel-id "$tunnel_id" \
  --mcp-server-url "$mcp_url" --health-listen-addr "$HEALTH_ADDR" --force >/dev/null
echo "Wrote profile ~/.config/tunnel-client/$PROFILE.yaml (tunnel $tunnel_id -> $mcp_url)."

# 3. Preflight with the real key: control plane reachable, tunnel usable, MCP answers.
echo "Running tunnel-client doctor ..."
if ! (set -a && . "$KEY_FILE" && set +a && "$TC" doctor --profile "$PROFILE" --explain); then
  echo "doctor reported problems (above). Common causes: the key lacks Tunnels Read + Use, the tunnel id is wrong," >&2
  echo "or the tunnel is not linked to your ChatGPT workspace. The service is installed anyway and keeps retrying." >&2
fi

# 4. Service.
unit_dir="$HOME/.config/systemd/user"
install -d -m 0755 "$unit_dir"
sed "s|@REPO@|$REPO|g" "$REPO/deploy/$UNIT" >"$unit_dir/$UNIT.tmp"
mv -f "$unit_dir/$UNIT.tmp" "$unit_dir/$UNIT"
systemctl --user daemon-reload
systemctl --user enable "$UNIT" >/dev/null
systemctl --user restart "$UNIT"

# 5. Readiness.
if curl -fs --retry 30 --retry-delay 1 --retry-all-errors --max-time 2 "http://$HEALTH_ADDR/readyz" >/dev/null; then
  echo "$UNIT is running and the tunnel is ready (status page: http://$HEALTH_ADDR/ui)."
  echo "Next: add the connector in ChatGPT (README, \"ChatGPT\")."
else
  echo "$UNIT did not become ready; recent log:" >&2
  journalctl --user -u "$UNIT" -n 30 --no-pager -o cat >&2 || true
  exit 1
fi
