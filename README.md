# School Email Sync

Keeps a searchable copy of a school Gmail account on this Linux server and lets
local tools queue outgoing mail, without the server ever holding Google
credentials.

```
 School Gmail ──▶ Google Apps Script ──HTTPS──▶ Cloudflare edge ──Tunnel──▶ cloudflared ──▶ backend ──▶ SQLite
  (Google)        (school account,             (TLS, public               (outbound-only   127.0.0.1:8766  ~/.local/state/
                   1-minute trigger)            hostname)                  connection)      Go, 1 process    school-email-sync/mail.db
```

* **Apps Script** (`apps-script/`) runs under the school account. It is the only
  component that talks to Gmail. Every minute it reads Gmail changes (history API),
  pushes normalized messages to the backend in bounded batches, polls for queued
  send/reply actions, performs them with `GmailApp`, and reports results.
* **Backend** (`backend/`) is one Go binary on loopback only. It stores messages in
  SQLite (FTS5 full-text index), deduplicates by Gmail message ID, keeps the outbound
  action queue, and serves a small authenticated HTTP API.
* **cloudflared** connects out to Cloudflare. No router ports are open, and
  Cloudflare terminates TLS.

Always running: **the backend** (systemd user service) and **cloudflared**
(systemd system service). Nothing else: no Docker, Node, Chromium, Redis or database
server.

## Project layout

```
apps-script/        Code.gs, appsscript.json, README.md: copy into script.google.com
backend/            Go source, tests (go.mod; one dependency: mattn/go-sqlite3)
deploy/
  school-email-sync.service   systemd user unit for the backend
  install-backend.sh          install/update the backend (no root)
  cloudflared.service         systemd unit for the tunnel (token read from a root-only file)
  install-cloudflared.sh      install cloudflared and connect the existing tunnel (sudo)
  smoke-test.sh               non-destructive checks of a running deployment
  backup.sh                   consistent compressed DB snapshot with rotation
tests/apps-script/  runs Code.gs under Node against a real backend (fake Gmail)
.env.example        backend settings reference
Makefile            build / test / install
```

Files that live outside the repository (never committed):

| Path | Contents | Mode |
| --- | --- | --- |
| `~/.config/school-email-sync/backend.env` | `SYNC_SECRET`, `API_SECRET`, `LISTEN_ADDR` | 0600 |
| `~/.local/state/school-email-sync/mail.db` (+ `-wal`, `-shm`) | the database | 0600, dir 0700 |
| `~/.local/bin/school-email-sync` | installed binary | 0755 |
| `~/.config/systemd/user/school-email-sync.service` | backend unit | |
| `/etc/cloudflared/school-email-sync.env` | `TUNNEL_TOKEN` | root 0600 |
| `/etc/systemd/system/cloudflared.service` | tunnel unit | |

## Setup

Requirements on the server: `golang-go` (1.22+), `gcc` (cgo for SQLite), `curl`,
user lingering (`loginctl enable-linger $USER`, already enabled here). Node 18+
is only needed to run the Apps Script tests.

```bash
make test        # Go tests + Apps Script scenario tests
make install     # build, install binary, create secrets once, enable + start the user service
sudo deploy/install-cloudflared.sh   # once: installs cloudflared, asks for the tunnel token
deploy/smoke-test.sh https://dannysemailsync.naclubs.net
```

`make install` generates `SYNC_SECRET` and `API_SECRET` on first install only.
Show the one Apps Script needs with:

```bash
grep '^SYNC_SECRET=' ~/.config/school-email-sync/backend.env
```

Then follow [apps-script/README.md](apps-script/README.md).

## Cloudflare configuration

* **Tunnel:** `School Email Sync` (remotely managed, ID `0e9e0e6f-a472-4463-8f71-aa093343bedf`),
  Cloudflare account Computerclubnasd@gmail.com.
* **Public hostname:** `dannysemailsync.naclubs.net` → `http://127.0.0.1:8766`
  (tunnel ingress, with a `http_status:404` catch-all), plus a proxied DNS record
  `dannysemailsync` CNAME `0e9e0e6f-a472-4463-8f71-aa093343bedf.cfargotunnel.com`
  in the `naclubs.net` zone. Both are visible under Networking → Tunnels → School
  Email Sync. The domain must be in the same Cloudflare account as the tunnel.
* **Connector:** `deploy/install-cloudflared.sh` installs the `cloudflared` package
  from `pkg.cloudflare.com` and a `cloudflared.service` equivalent to what
  `cloudflared service install <token>` writes, except that the token is read from
  `/etc/cloudflared/school-email-sync.env` (root, 0600) rather than placed in
  `ExecStart`, where any local user could read it via `ps` or `systemctl show`.
  cloudflared's metrics and readiness endpoint is on `127.0.0.1:20241` (`/ready`).
* Replace the token (for example after rotating it in the dashboard):
  `sudo deploy/install-cloudflared.sh --new-token`.
* **Security features:** Apps Script requests come from Google servers with a
  `Google-Apps-Script` user agent. If the zone has **Bot Fight Mode**, aggressive
  WAF rules or **Cloudflare Access** on this hostname, Apps Script gets a 403 or
  redirect. Leave them off for this hostname, or add a WAF skip rule for it. The
  backend's own bearer-token authentication protects the API.

## API

All bodies are JSON. `Authorization: Bearer <secret>`.

| Endpoint | Secret | Purpose |
| --- | --- | --- |
| `GET /healthz` | none | `{"ok":true}` when the database answers |
| `GET /v1/sync/ping` | SYNC | auth check for Apps Script |
| `POST /v1/sync/messages` | SYNC | `{"messages":[…]}`, 1–100 per batch, ≤ 10 MiB; idempotent by Gmail ID |
| `POST /v1/sync/message` | SYNC | one message object |
| `POST /v1/sync/known` | SYNC | `{"ids":[…]}` → `{"unknown":[…]}` |
| `POST /v1/sync/state` | SYNC | label/read-state changes or deletions → `{"unknown":[…]}` |
| `POST /v1/sync/labels` | SYNC | label ID → name table |
| `POST /v1/actions/poll` | SYNC | claim up to `limit` (≤ 20) ready actions; 10-minute lease |
| `POST /v1/actions/{id}/complete` | SYNC | `{"claimId","result":{"messageId","threadId"}}`; repeatable |
| `POST /v1/actions/{id}/fail` | SYNC | `{"claimId","error","retryable"}`; retryable → backoff 30 s … 30 min, max 5 attempts |
| `POST /v1/actions` | API, local only | queue `send` or `reply` (below) |
| `GET /v1/actions[?status=]`, `GET /v1/actions/{id}` | API, local only | inspect the queue |
| `POST /v1/actions/{id}/cancel` | API, local only | cancel a pending action |
| `GET /v1/messages` | API, local only | search: `q` (words, `term*` prefix), `fts` (raw FTS5), `from`, `to`, `subject`, `label` (ID or name), `threadId`, `unread`, `starred`, `inbox`, `after`, `before` (RFC 3339, `YYYY-MM-DD` or Unix time), `includeTrash`, `limit` ≤ 100, `cursor` |
| `GET /v1/messages/{id}[?html=1]` | API, local only | full message with text body, attachments metadata, optional HTML |
| `GET /v1/threads/{id}` | API, local only | thread, oldest first, bodies capped by `bodyChars` |
| `GET /v1/stats` | API, local only | counts, newest/oldest message, last ingest/poll times, DB size |

"Local only" means requests that arrive through Cloudflare are refused with 403
even with the right secret, so nobody on the internet can search the mailbox or
queue mail (`ALLOW_REMOTE_CLIENT_API=true` lifts this). The Apps Script secret can
only ingest data and act on actions that local tools queued.

Examples from the server. The helper keeps the secret out of the process list:

```bash
ses() { printf 'Authorization: Bearer %s\n' "$(sed -n 's/^API_SECRET=//p' ~/.config/school-email-sync/backend.env)" \
          | curl -sS -H @- -H 'Content-Type: application/json' "$@"; }

ses 'http://127.0.0.1:8766/v1/messages?q=field%20trip&unread=true&limit=5'
ses http://127.0.0.1:8766/v1/stats
ses http://127.0.0.1:8766/v1/actions -d '{"type":"send","to":["teacher@school.org"],"subject":"Question","body":"Hi …","idempotencyKey":"q-2026-10-06"}'
ses http://127.0.0.1:8766/v1/actions -d '{"type":"reply","messageId":"19a3f…","replyAll":false,"body":"Thanks!"}'
```

Send/reply payload fields: `to`, `cc`, `bcc` (arrays of single addresses, at most 50
in total), `subject` (send only), `body` (plain text; derived from `htmlBody` if
omitted), `htmlBody`, `messageId` or `threadId` (reply), `replyAll`,
`idempotencyKey` (repeating a request with the same key returns the original
action). A queued action is sent within about a minute by the Apps Script trigger.

## Service management

```bash
systemctl --user status school-email-sync      # backend (no sudo)
systemctl --user restart school-email-sync
systemctl --user stop school-email-sync
systemctl status cloudflared                   # tunnel
sudo systemctl restart cloudflared
```

Both are enabled at boot. The backend starts with the user manager (lingering is
on, so no login is needed) and cloudflared starts after `network-online.target`.
Neither depends on the other: if the backend is not up yet, cloudflared answers
502 and Apps Script retries on its next run. Both restart on failure. The backend
restarts with increasing delays (2 s up to 2 min) and never gives up. It also has a
192 MiB memory guard and is sandboxed so only its state directory is writable.

## Logs

```bash
journalctl --user -u school-email-sync -f      # backend: one logfmt line per request plus ingest/action summaries
journalctl -u cloudflared -f                    # tunnel
```

Request lines log method, path, status, duration, sizes and the client IP
(`via=cf:<ip>` or `local`), never query strings or message contents. Idle polls
and health checks are logged only at `LOG_LEVEL=debug`. Apps Script logs live in
the Apps Script editor → Executions.

## Database

`~/.local/state/school-email-sync/mail.db` (SQLite, WAL mode). Tables: `messages`
(one row per Gmail message ID, with headers, text/HTML bodies, label IDs and derived
flags, sync timestamps), `attachments` (metadata; `fetch_state`/`stored_path`
reserved for fetching content later), `messages_fts` (FTS5 over subject, from,
to, cc, body), `labels`, `actions`, `kv`. The schema is versioned with
`PRAGMA user_version` and migrated on startup.

Ad-hoc queries (read-only):
`python3 -c "import sqlite3; c=sqlite3.connect('file:$HOME/.local/state/school-email-sync/mail.db?mode=ro', uri=True); print(c.execute('select count(*) from messages').fetchone())"`

## Backup and restore

```bash
deploy/backup.sh                     # → ~/backups/school-email-sync/mail-YYYYMMDD-HHMMSS.db.gz, keeps 14
school-email-sync backup /path/x.db  # one uncompressed snapshot
```

Both use SQLite `VACUUM INTO`, which is consistent while the service runs. For a
daily backup, add to `crontab -e`: `17 3 * * * $HOME/projects/school-gmail-mcp/deploy/backup.sh`.
Copy the snapshots off this machine as well.

Restore:

```bash
systemctl --user stop school-email-sync
cd ~/.local/state/school-email-sync && rm -f mail.db mail.db-wal mail.db-shm
gunzip -c ~/backups/school-email-sync/mail-….db.gz > mail.db && chmod 600 mail.db
systemctl --user start school-email-sync
```

Gmail is the source of truth: if the database is lost, Apps Script `resetSyncState`
(plus `startBackfill` for older mail) re-imports everything. Only the history of
outbound actions is not recoverable from Gmail.

## Updating

```bash
cd ~/projects/school-gmail-mcp && git pull
make test && make install            # rebuilds, swaps the binary atomically, restarts
deploy/smoke-test.sh https://dannysemailsync.naclubs.net
```

Schema migrations run automatically on start. If `apps-script/` changed, paste the new
`Code.gs` (and `appsscript.json` if changed) into the Apps Script editor. cloudflared
is updated with the system: `sudo apt upgrade cloudflared`, then
`sudo systemctl restart cloudflared`.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Apps Script: `HTTP 401` | `SYNC_SECRET` Script Property ≠ server's `backend.env` |
| Apps Script: `HTTP 502` | backend down: `systemctl --user status school-email-sync`, `journalctl --user -u school-email-sync` |
| Apps Script: `HTTP 530` / 1033 | tunnel down: `systemctl status cloudflared`, `curl 127.0.0.1:20241/ready` |
| Apps Script: `HTTP 403` HTML page | Cloudflare Bot Fight Mode / WAF / Access on the hostname (see Cloudflare configuration) |
| Apps Script: DNS / `Address unavailable` | `BACKEND_URL` typo or the public hostname is missing in the tunnel |
| Backend will not start | `journalctl --user -u school-email-sync -n 50`: missing or short secrets, non-loopback `LISTEN_ADDR`, unwritable state dir |
| Service gone after reboot | `loginctl show-user $USER -p Linger` must be `yes`; `systemctl --user is-enabled school-email-sync` |
| Mail not arriving | Apps Script `showStatus` (queue, scan, backoff), Executions log; backend `GET /v1/stats` → `lastIngestAt`, `lastPollAt` |
| Action stuck `claimed` | Apps Script has not reported; it is offered again after the 10-minute lease. `showStatus` lists unreported actions |
| Action `failed` with "outcome unknown" | the script died mid-send; check Gmail's Sent folder before re-queueing |
| `~/.local/state/school-email-sync` is a symlink | systemd's legacy-layout migration; stop the service, move `mail.db*` into a real directory, re-run `make install` |

`deploy/smoke-test.sh https://dannysemailsync.naclubs.net` runs the deployment checks in one go:
services, loopback binding, tunnel readiness, public health, authentication
enforcement, and client-API lockout. It writes nothing.

## Development

```bash
make test-go            # backend unit/HTTP tests (temp databases)
make test-apps-script   # Code.gs under Node with fake Gmail against a real backend
VERBOSE=1 make test-apps-script
```

The Apps Script tests can also run end to end through Cloudflare. Use this only on
a fresh, empty deployment, because it writes synthetic mail and actions:

```bash
SES_E2E_URL=https://dannysemailsync.naclubs.net SES_SYNC_SECRET=… SES_API_SECRET=… node --test tests/apps-script/code-gs.test.js
```
