#!/usr/bin/env bash
# Non-destructive checks of a running deployment. Writes nothing to the database.
#
#   deploy/smoke-test.sh https://your-hostname
#
# Checks: services enabled and running, backend bound to loopback only, local
# health, cloudflared connected, and through the public hostname: health,
# authentication enforced, Apps Script credentials accepted, and the client API
# (search / sending) refused from the internet.
# Secrets are read from ~/.config/school-email-sync/backend.env and passed to
# curl on stdin, never on its command line.
set -uo pipefail

PUBLIC="${1:-${PUBLIC_URL:-}}"
PUBLIC="${PUBLIC%/}"
ENV_FILE="$HOME/.config/school-email-sync/backend.env"
pass=0
fail=0

ok() { echo "  ok    $*"; pass=$((pass + 1)); }
bad() { echo "  FAIL  $*"; fail=$((fail + 1)); }
check() { # check "description" expected actual
  if [[ $2 == "$3" ]]; then ok "$1"; else bad "$1 (expected $2, got $3)"; fi
}
setting() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1; }

# status URL [auth-header] [curl args...]: prints the HTTP status code.
# DOH_URL=https://cloudflare-dns.com/dns-query resolves through DNS-over-HTTPS,
# useful right after creating the DNS record while a local resolver still caches NXDOMAIN.
status() {
  local url=$1 auth=${2:-}
  shift 2 || shift $#
  printf '%s\n' "$auth" | curl -s -o /dev/null -w '%{http_code}' --max-time 20 ${DOH_URL:+--doh-url "$DOH_URL"} -H @- "$@" "$url"
}

[[ -r $ENV_FILE ]] || { echo "Cannot read $ENV_FILE" >&2; exit 1; }
SYNC="Authorization: Bearer $(setting SYNC_SECRET)"
API="Authorization: Bearer $(setting API_SECRET)"
LOCAL="http://$(setting LISTEN_ADDR)"
[[ $LOCAL == "http://" ]] && LOCAL="http://127.0.0.1:8766"

echo "Local"
check "backend service enabled" enabled "$(systemctl --user is-enabled school-email-sync 2>/dev/null)"
check "backend service active" active "$(systemctl --user is-active school-email-sync 2>/dev/null)"
check "cloudflared service enabled" enabled "$(systemctl is-enabled cloudflared 2>/dev/null)"
check "cloudflared service active" active "$(systemctl is-active cloudflared 2>/dev/null)"
listen="$(ss -Htln "sport = :${LOCAL##*:}" | awk '{print $4}' | sort -u | tr '\n' ' ')"
check "backend listens on loopback only" "${LOCAL#http://} " "$listen"
check "local /healthz" 200 "$(status "$LOCAL/healthz" '')"
check "cloudflared /ready (tunnel connected)" 200 "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:20241/ready)"

echo "ChatGPT (MCP)"
MCP_ADDR="$(setting MCP_LISTEN_ADDR)"
MCP_ADDR="${MCP_ADDR:-127.0.0.1:8767}"
if [[ $MCP_ADDR == off ]]; then
  echo "  skipped (MCP_LISTEN_ADDR=off)"
else
  mlisten="$(ss -Htln "sport = :${MCP_ADDR##*:}" | awk '{print $4}' | sort -u | tr '\n' ' ')"
  check "MCP endpoint listens on loopback only" "$MCP_ADDR " "$mlisten"
  init="$(curl -s --max-time 5 -H 'Content-Type: application/json' "http://$MCP_ADDR/mcp" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke-test","version":"1"}}}')"
  check "MCP initialize answers as school-gmail" yes "$(grep -q '"name":"school-gmail"' <<<"$init" && echo yes || echo no)"
  check "MCP refuses a foreign Host (DNS rebinding)" 403 "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H 'Host: evil.example' -H 'Content-Type: application/json' -d '{}' "http://$MCP_ADDR/mcp")"
  tools="$(curl -s --max-time 5 -H 'Content-Type: application/json' "http://$MCP_ADDR/mcp" \
    -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')"
  check "MCP offers get_attachment" yes "$(grep -q '"name":"get_attachment"' <<<"$tools" && echo yes || echo no)"
  if systemctl --user cat school-email-sync-chatgpt >/dev/null 2>&1; then
    check "ChatGPT tunnel service enabled" enabled "$(systemctl --user is-enabled school-email-sync-chatgpt 2>/dev/null)"
    check "ChatGPT tunnel service active" active "$(systemctl --user is-active school-email-sync-chatgpt 2>/dev/null)"
    check "ChatGPT tunnel ready" 200 "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:8081/readyz)"
  else
    echo "  ChatGPT tunnel not installed yet (deploy/install-chatgpt-tunnel.sh)"
  fi
fi

echo "Attachments"
command -v pdftotext >/dev/null && ok "pdftotext installed (PDF text)" || bad "pdftotext missing: sudo apt install poppler-utils"
command -v bwrap >/dev/null && ok "bwrap installed (converter sandbox)" || bad "bwrap missing: sudo apt install bubblewrap"
FETCH_URL="$(setting ATTACHMENT_FETCH_URL)"
if [[ -z $FETCH_URL ]]; then
  echo "  ATTACHMENT_FETCH_URL not set: attachments are relayed through the sync trigger"
else
  # GET answers with doGet's JSON only when the deployment allows anonymous access.
  page="$(curl -sL --max-time 20 ${DOH_URL:+--doh-url "$DOH_URL"} "$FETCH_URL")"
  check "Apps Script web app reachable without sign-in" yes "$(grep -q 'attachment endpoint' <<<"$page" && echo yes || echo no)"
fi

if [[ -z $PUBLIC ]]; then
  echo "Public checks skipped (usage: $0 https://your-hostname)"
else
  echo "Public ($PUBLIC)"
  check "GET /healthz" 200 "$(status "$PUBLIC/healthz" '')"
  check "no credentials -> 401" 401 "$(status "$PUBLIC/v1/sync/ping" '')"
  check "wrong secret -> 401" 401 "$(status "$PUBLIC/v1/sync/ping" 'Authorization: Bearer not-the-secret-not-the-secret-0000')"
  check "Apps Script secret accepted" 200 "$(status "$PUBLIC/v1/sync/ping" "$SYNC")"
  check "read-only sync call" 200 "$(status "$PUBLIC/v1/sync/known" "$SYNC" -X POST -H 'Content-Type: application/json' --data '{"ids":[]}')"
  check "client API refused remotely (search)" 403 "$(status "$PUBLIC/v1/messages" "$API")"
  check "client API refused remotely (send)" 403 "$(status "$PUBLIC/v1/actions" "$API" -X POST -H 'Content-Type: application/json' \
    --data '{"type":"send","to":["nobody@example.org"],"subject":"x","body":"x"}')"
  check "malformed payload -> 400" 400 "$(status "$PUBLIC/v1/sync/messages" "$SYNC" -X POST -H 'Content-Type: application/json' --data '{"messages":')"
  check "attachment relay needs the Apps Script secret" 401 "$(status "$PUBLIC/v1/attachments/relay/att_x" '' -X POST -H 'Content-Type: application/json' --data '{}')"
  check "MCP endpoint not exposed publicly" 404 "$(status "$PUBLIC/mcp" '' -X POST -H 'Content-Type: application/json' --data '{"jsonrpc":"2.0","id":1,"method":"ping"}')"
fi

echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
