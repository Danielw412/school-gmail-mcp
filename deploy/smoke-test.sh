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
fi

echo "$pass passed, $fail failed"
[[ $fail -eq 0 ]]
