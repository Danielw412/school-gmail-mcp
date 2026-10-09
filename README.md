# School Email Sync

Keeps a searchable copy of a school Gmail account on this Linux server, lets
ChatGPT search, read and send that mail through MCP, and lets local tools queue
outgoing mail, without the server ever holding Google credentials.

```
 School Gmail ──▶ Google Apps Script ──HTTPS──▶ Cloudflare edge ──Tunnel──▶ cloudflared ──▶ backend :8766 ──▶ SQLite
  (Google)        (school account,             (TLS, public               (outbound-only   (Apps Script API) ~/.local/state/
                   1-minute trigger)            hostname)                  connection)            │          school-email-sync/mail.db
                                                                                           same process
 ChatGPT ──▶ OpenAI control plane ◀──outbound── tunnel-client ──────────────────────────▶ backend :8767
             (Secure MCP Tunnel)                (profile school-gmail)                     (MCP endpoint)
```

* **Apps Script** (`apps-script/`) runs under the school account. It is the only
  component that talks to Gmail. Every minute it reads Gmail changes (history API),
  pushes normalized messages to the backend in bounded batches, polls for queued
  send/reply actions, performs them with `GmailApp`, and reports results. On request
  it also downloads a single attachment for the backend (see [Attachments](#attachments)).
* **Backend** (`backend/`) is one Go binary on loopback only. It stores messages and
  attachment metadata in SQLite (FTS5 full-text index), deduplicates by Gmail message
  ID, keeps the outbound action queue, and serves a small authenticated HTTP API.
  Attachment content is never stored: it is fetched when an agent asks, converted in
  memory and discarded.
* **cloudflared** connects out to Cloudflare. No router ports are open, and
  Cloudflare terminates TLS.
* **MCP endpoint** (same backend process, separate loopback port `127.0.0.1:8767`)
  gives ChatGPT tools to search, read and send mail. Cloudflare never routes to
  it; ChatGPT reaches it only through an **OpenAI Secure MCP Tunnel**
  (`tunnel-client`, outbound-only), the same way the google-tasks MCP server on
  this machine is connected. See [ChatGPT (MCP)](#chatgpt-mcp).

Always running: **the backend** (systemd user service), **cloudflared** (systemd
system service) and, once set up, **the ChatGPT tunnel** (systemd user service).
Nothing else: no Docker, Node, Chromium, Redis or database server.

## Project layout

```
apps-script/        Code.gs, appsscript.json, README.md: copy into script.google.com
backend/            Go source, tests (go.mod; one dependency: mattn/go-sqlite3)
deploy/
  school-email-sync.service   systemd user unit for the backend
  install-backend.sh          install/update the backend (no root)
  cloudflared.service         systemd unit for the tunnel (token read from a root-only file)
  install-cloudflared.sh      install cloudflared and connect the existing tunnel (sudo)
  school-email-sync-chatgpt.service  systemd user unit for the ChatGPT tunnel (tunnel-client)
  install-chatgpt-tunnel.sh   create the tunnel-client profile, store the OpenAI key, start the tunnel
  smoke-test.sh               non-destructive checks of a running deployment
  backup.sh                   consistent compressed DB snapshot with rotation
tests/apps-script/  runs Code.gs under Node against a real backend (fake Gmail)
.env.example        backend settings reference
Makefile            build / test / install
```

Files that live outside the repository (never committed):

| Path | Contents | Mode |
| --- | --- | --- |
| `~/.config/school-email-sync/backend.env` | `SYNC_SECRET`, `API_SECRET`, `LISTEN_ADDR`, optional `ATTACHMENT_FETCH_URL` / `ATTACHMENT_FETCH_SECRET` | 0600 |
| `~/.local/state/school-email-sync/mail.db` (+ `-wal`, `-shm`) | the database | 0600, dir 0700 |
| `~/.local/bin/school-email-sync` | installed binary | 0755 |
| `~/.config/systemd/user/school-email-sync.service` | backend unit | |
| `/etc/cloudflared/school-email-sync.env` | `TUNNEL_TOKEN` | root 0600 |
| `/etc/systemd/system/cloudflared.service` | tunnel unit | |
| `~/.config/school-email-sync/chatgpt-tunnel.env` | `CONTROL_PLANE_API_KEY` (OpenAI runtime key) | 0600 |
| `~/.config/tunnel-client/school-gmail.yaml` | tunnel-client profile (tunnel id, MCP URL, health port 8081) | 0600 |
| `~/.config/systemd/user/school-email-sync-chatgpt.service` | ChatGPT tunnel unit | |

## Setup

Requirements on the server: `golang-go` (1.22+), `gcc` (cgo for SQLite), `curl`,
user lingering (`loginctl enable-linger $USER`, already enabled here). For reading
attachments: `poppler-utils` (PDF text and page images) and `bubblewrap` (sandbox for
the PDF tools); both are installed here. Node 18+ is only needed to run the Apps
Script tests.

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

## ChatGPT (MCP)

### How it works

The backend serves the [Model Context Protocol](https://modelcontextprotocol.io)
(Streamable HTTP, stateless JSON) at `http://127.0.0.1:8767/mcp`. `tunnel-client`
(OpenAI's Secure MCP Tunnel daemon, already installed at `/usr/local/bin/tunnel-client`
for google-tasks) holds an outbound connection to OpenAI and forwards ChatGPT's MCP
calls to that URL. Nothing new is exposed to the internet:

* the MCP port is loopback only, is not part of the Cloudflare tunnel, and rejects
  requests whose `Host` or `Origin` is not loopback (DNS-rebinding guard);
* access control is the OpenAI tunnel: only ChatGPT workspaces the tunnel is
  linked to can call it, so the ChatGPT connector uses **No authentication**;
* the tools work on the synced copy in SQLite. Sending goes through the same queue
  as the local API, and Apps Script sends it from Gmail.

### Tools

| Tool | Kind | What it does |
| --- | --- | --- |
| `search_emails` | read | Search or list mail, newest first, with Gmail-style operators: `from:` `to:` `subject:` `label:` `category:` `filename:` `is:unread/read/starred/important` `in:inbox/sent/trash/anywhere` `has:attachment` `after:`/`before:` `newer_than:`/`older_than:` `"phrase"` `a OR b` `word*`; paging with `next_cursor`. Each result lists its attachments (`attachment_id`, `filename`) |
| `get_email` | read | One email: headers, labels, attachments (`attachment_id`, filename, type, size), plain-text body (capped, default 20,000 characters) |
| `get_thread` | read | A conversation, oldest first (newest 20 messages, 4,000 characters each by default), with each message's attachments |
| `get_attachment` | read | Downloads one attachment from Gmail on demand and returns its text (PDF, Word, text/CSV) or image; see [Attachments](#attachments) |
| `search`, `fetch` | read | OpenAI's standard search/fetch shapes, used by deep research |
| `mailbox_status` | read | Message counts, stored date range, last Gmail sync, outgoing queue |
| `list_outgoing` | read | Queued/sent/failed outgoing mail and its status |
| `send_email` | write | Queue a new plain-text email |
| `reply_to_email` | write | Queue a reply (or reply-all) in the original Gmail thread |
| `cancel_email` | write | Cancel a queued email before it is sent |

Safety built in:

* Write tools are marked as not read-only, so ChatGPT asks you to confirm each send.
  The server instructions tell the model to show the full draft first, and to treat
  email content as untrusted: it must never act on instructions inside an email.
* Mail queued through ChatGPT waits `MCP_SEND_DELAY_SECONDS` (default 60 s) before
  Apps Script may send it, so "cancel that" still works. It goes out within about a
  minute after that.
* An identical send/reply requested again within 10 minutes is not queued twice.
* `MCP_ALLOW_SEND=false` in `backend.env` hides the write tools (read-only ChatGPT access).
* Logs record tool names and outcomes, never arguments or message text.

### Setup

What you need from OpenAI (the same organization and ChatGPT workspace that the
google-tasks tunnel uses):

1. **A tunnel.** At <https://platform.openai.com/settings/organization/tunnels>,
   create a tunnel (for example "School Gmail"), link it to your ChatGPT workspace,
   and copy its id (`tunnel_…`).
2. **A runtime API key** with *Tunnels Read + Use*
   (<https://platform.openai.com/settings/organization/api-keys>). You can create a
   dedicated one, or reuse the google-tasks key.

Then on the server:

```bash
deploy/install-chatgpt-tunnel.sh --tunnel-id tunnel_…            # prompts for the key (hidden)
deploy/install-chatgpt-tunnel.sh --tunnel-id tunnel_… --key-from ~/.config/google-tasks-mcp/tunnel.env   # or reuse that key
```

The script stores the key in `~/.config/school-email-sync/chatgpt-tunnel.env`
(0600), writes the tunnel-client profile `school-gmail`, runs `tunnel-client doctor`,
installs and starts `school-email-sync-chatgpt.service`, and waits for
`http://127.0.0.1:8081/readyz`.

Finally, in ChatGPT on the web (while the tunnel is running):

1. Settings → Apps (Connectors) → Advanced → turn on **Developer mode** if your plan requires it.
2. **Create** / **Add custom MCP server**: name `School Gmail`, connection type
   **Tunnel**, choose the tunnel (or paste its id), authentication **No authentication**.
3. In a chat, enable School Gmail from the tools menu (or mention it with `@`) and
   ask, for example, "What did my teachers email me this week?"
4. After a server update that changes tools, use **Refresh** on the connector.

OpenAI renames these screens from time to time; see
<https://developers.openai.com/api/docs/guides/secure-mcp-tunnels> for the current steps.

### Settings (`backend.env`)

| Variable | Default | Meaning |
| --- | --- | --- |
| `MCP_LISTEN_ADDR` | `127.0.0.1:8767` | MCP endpoint (loopback only, must differ from `LISTEN_ADDR`); `off` disables it |
| `MCP_ALLOW_SEND` | `true` | `false` makes ChatGPT access read-only |
| `MCP_SEND_DELAY_SECONDS` | `60` | cancel window for mail queued through ChatGPT (0–900) |
| `ATTACHMENT_*` | | see [Attachments](#attachments) |

Restart the backend after changing them: `systemctl --user restart school-email-sync`.

### Testing without OpenAI

`tunnel-client dev proxy` runs an in-memory control plane plus tunnel-client, the
same path ChatGPT uses:

```bash
tunnel-client dev proxy --mcp-server-url http://127.0.0.1:8767/mcp --print-json
```

It prints a local MCP URL that any MCP client can use. `tunnel-client doctor`
logs `OAuth discovery failed ... status 404` for this server. That is expected
for a server without OAuth (the google-tasks tunnel logs the same) and does not
affect readiness.

## Attachments

Agents see which emails have attachments (`search_emails`, `get_email`, `get_thread`
list `attachment_id` and `filename`) and read one with `get_attachment`. The server
keeps **metadata only** (name, MIME type, size, MIME part ID). The content stays in
Gmail until an agent asks for it; then:

```
agent ─get_attachment─▶ backend ─signed request─▶ Apps Script (doPost) ─▶ Gmail API
                           ▲                       or, without a web app:
                           │                       next 1-minute trigger run (relay)
                           └── file (in memory) ── converted to text / image ── response ── discarded
```

Nothing is written to the database, disk, logs or any cache: the file lives in memory
for the request and is dropped as soon as the response is written. Logs record only
sizes and durations.

### What agents get

| File | Result |
| --- | --- |
| PDF | text, page by page (`pdftotext`); pages without a text layer (scans) are rendered as JPEG images (first 3) with a note that OCR is not available |
| Word `.docx` | text (paragraphs, tables as tab-separated rows) |
| Text, CSV, TSV, JSON, Markdown, HTML, `.ics` | text (UTF-8, UTF-16 or Latin-1 detected); HTML converted to plain text |
| PNG, JPEG, GIF, WebP | MCP image content; images larger than 1 MB or 1,960 px are scaled to 1,568 px (JPEG) |
| anything else (XLSX, PPTX, ZIP, HEIC, audio, video, …) | a note saying it cannot be converted (audio/video are not even downloaded) |

The first content block is always JSON metadata (`filename`, `mime_type`, `size_bytes`,
`format`, `pages`, `notes`, `gmail_url`, `resource_uri`); text follows as a plain text
block, never as base64. Long text comes in parts (`max_chars`, default 40,000; call again
with `offset` = `next_offset`). The file type is detected from the content, not the
declared MIME type.

`attachment_id` is Gmail's MIME part ID (for example `1` or `0.1.2`; `#<n>` for a part
without one). It is stable for the message, so two attachments with the same file name
have different IDs. Gmail's own attachment IDs change on every API read and are not
used for lookups.

**Binary resources.** The server also offers each attachment's original file as an MCP
resource (`gmail-attachment://{message_id}/{attachment_id}`, `resources/read` returns a
base64 `blob`; `resources/list` shows the 50 newest). Only clients that read MCP
resources use this, and only when asked; `get_attachment` never returns the raw file.
Note that some clients store what they read: Claude Code saves a resource blob as a
file in its own session folder.

**Inline images.** Images that the HTML body shows inline (a `cid:` reference, or an
inline image with a Content-ID), which is how signature logos are sent, and S/MIME/PGP
signature parts are marked `hidden`: they are left out of attachment lists, counts and
`has:attachment`, as in Gmail. Non-images are always listed, even with an inline
disposition. `get_email` lists inline images of 64 KB or more separately as
`inline_images` (usually pasted pictures, not logos); `get_attachment` reads them too.

### Client support (verified)

| Client | Text | Images | Binary resource |
| --- | --- | --- | --- |
| Official MCP TypeScript SDK client 1.30.1 (validates every result against the schema) | ✓ | ✓ | ✓ |
| Through `tunnel-client dev proxy` (the path ChatGPT uses), including a 9.5 MB resource | ✓ | ✓ | ✓ |
| Claude Code (headless), a real model client | ✓ read the PDF, DOCX and CSV text | ✓ read the text in a JPEG, a scaled PNG and a scanned PDF page | saves the blob to a file in its session folder |
| ChatGPT | expected ✓ (plain text blocks) | **not guaranteed**: OpenAI says connector image output "should not currently be treated as a guaranteed" vision input ([forum, 2026](https://community.openai.com/t/chatgpt-connector-returns-empty-for-mcp-tool-results-with-type-image-while-same-tool-works-in-mcp-inspector/1375446)) | not used |

ChatGPT was not tested end to end: that needs the updated Apps Script deployed and a
ChatGPT session. For ChatGPT, text formats are the reliable path; for images and
scanned pages the metadata tells the model to say so and point to `gmail_url` if it
cannot see them. Text recognition (OCR) would need `tesseract`, which this server does
not run (CPU).

### Retrieval: web app or relay

Gmail credentials exist only in Apps Script, so Apps Script downloads the file. Two
ways, chosen by `ATTACHMENT_FETCH_URL`:

* **Web app (recommended, about 2–5 s).** `Code.gs` has a `doPost` endpoint. Deploy the
  script as a web app (*Execute as: Me*, *Who has access: Anyone*; steps in
  [apps-script/README.md](apps-script/README.md#9-attachments-web-app)), then set in
  `backend.env`:

  ```bash
  ATTACHMENT_FETCH_URL=https://script.google.com/macros/s/<deployment id>/exec
  ATTACHMENT_FETCH_SECRET=<school-email-sync gen-secret; same value as the Script Property>
  ```

* **Relay (no deployment; up to about a minute).** With `ATTACHMENT_FETCH_URL` empty, the
  request is handed to Apps Script with its next action poll (once a minute) and the
  file is uploaded over the existing `SYNC_SECRET` channel. `get_attachment` waits up to
  `ATTACHMENT_FETCH_TIMEOUT_SECONDS` (40 s, since ChatGPT gives up after about 60 s); if
  the trigger has not run by then, the agent is told to call again and the retry joins
  the queued request. A file that arrives after everyone stopped waiting is dropped.

**Google Workspace restrictions.** A school domain may not let you deploy a web app
for *Anyone*: when the admin blocks sharing outside the organization, that option is
missing from the deploy dialog. *Anyone within <school>* does not help, because the
server would need a Google sign-in (and so Google credentials). Use relay mode then.
If the backend reaches a deployment that requires sign-in, `get_attachment` says so
instead of failing silently. Admins can also turn Apps Script off entirely; the sync
would stop too, so you would notice.

**Security of the web app.**

* Requests are signed with HMAC-SHA256 over a version label, timestamp, random nonce,
  message ID, attachment ID and size limit, using `ATTACHMENT_FETCH_SECRET`. The secret
  itself never travels; it differs from `SYNC_SECRET` and `API_SECRET`.
* Apps Script checks the signature before touching Gmail, refuses requests more than
  2 minutes old, and remembers nonces for 10 minutes (Apps Script cache), so a captured
  request cannot be replayed. The signature covers the attachment ID, so a request
  cannot be redirected to another file.
* The response is signed too (nonce, size, SHA-256 of the data): a wrong URL or altered
  data is rejected.
* The endpoint does one thing: return one attachment of one message, up to the
  requested size. It cannot search, list, read bodies or send. Errors before
  authentication are generic.
* The backend only follows redirects to `script.google.com` and
  `script.googleusercontent.com` (Apps Script answers a POST with a redirect).

**Safe processing.** The file size is checked against Gmail's metadata before the
download and again afterwards (`ATTACHMENT_MAX_BYTES`, default 10 MB, at most two
downloads at a time). `pdftotext`/`pdftoppm` read the file on stdin and write to stdout
(no temporary files), with a timeout and an output cap. They run inside `bwrap`: no
network, no access to the home directory, database or secrets, no capabilities; the
backend logs a warning and runs them unsandboxed if `bwrap` is missing. They are also
the preferred victims if memory runs out (`oom_score_adj`), and the unit has
`OOMPolicy=continue`. Word files are read in Go with a cap on the decompressed size
(zip bombs); images are checked for pixel count before decoding.

### Settings (`backend.env`)

| Variable | Default | Meaning |
| --- | --- | --- |
| `ATTACHMENT_FETCH_URL` | empty (relay) | Apps Script web app `/exec` URL |
| `ATTACHMENT_FETCH_SECRET` | | required with `ATTACHMENT_FETCH_URL`; same value as the Script Property `ATTACHMENT_FETCH_SECRET` |
| `ATTACHMENT_MAX_BYTES` | `10485760` | largest attachment downloaded (1 KiB – 25 MiB) |
| `ATTACHMENT_FETCH_TIMEOUT_SECONDS` | `40` | wait for Gmail / the relay (5–300) |
| `ATTACHMENT_CONVERT_TIMEOUT_SECONDS` | `15` | text extraction and image conversion (5–300) |

`mailbox_status` shows the retrieval mode and the size limit.

### Metadata of mail synced earlier

* Schema v2 (applied on the first start after updating) re-classifies stored attachment
  rows from data already in the database: inline logos become hidden, inline PDFs
  visible. No Gmail access needed.
* Messages stored without attachment metadata (for example by an early version of the
  script) get it from the Apps Script helper `startAttachmentBackfill`: it lists mail
  with attachments over `BACKFILL_DAYS` (default 365), keeps the messages the backend
  already stores, and reads only their MIME structure (Gmail partial response without
  bodies or attachment data). It runs over the next trigger runs; `showStatus` shows
  progress.

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
| `POST /v1/actions/poll` | SYNC | claim up to `limit` (≤ 20) ready actions; 10-minute lease. In relay mode the response also carries `attachmentRequests` |
| `POST /v1/attachments/relay/{id}` | SYNC | relay mode: the requested attachment (`{"ok":true,"data":<base64>,"size":n}` or `{"ok":false,"error":{…}}`); 410 when nobody waits any more |
| `POST /v1/sync/attachments` | SYNC | `{"messages":[{"id","attachments":[…]}]}`: replace attachment metadata of stored messages (backfill) → `{"updated","unchanged","unknown":[…]}` |
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
systemctl status cloudflared                   # Cloudflare tunnel
sudo systemctl restart cloudflared
systemctl --user status school-email-sync-chatgpt   # ChatGPT tunnel (once installed)
systemctl --user restart school-email-sync-chatgpt
```

All are enabled at boot. The ChatGPT tunnel starts after the backend and restarts
whenever it exits. The backend starts with the user manager (lingering is
on, so no login is needed) and cloudflared starts after `network-online.target`.
Neither depends on the other: if the backend is not up yet, cloudflared answers
502 and Apps Script retries on its next run. Both restart on failure. The backend
restarts with increasing delays (2 s up to 2 min) and never gives up. It also has a
192 MiB memory guard and is sandboxed so only its state directory is writable.

## Logs

```bash
journalctl --user -u school-email-sync -f      # backend: one logfmt line per request plus ingest/action summaries
journalctl -u cloudflared -f                    # Cloudflare tunnel
journalctl --user -u school-email-sync-chatgpt -f   # ChatGPT tunnel (JSON lines); status page http://127.0.0.1:8081/ui
```

Request lines log method, path, status, duration, sizes and the client IP
(`via=cf:<ip>` or `local`), never query strings or message contents. Idle polls
and health checks are logged only at `LOG_LEVEL=debug`. Apps Script logs live in
the Apps Script editor → Executions.

## Database

`~/.local/state/school-email-sync/mail.db` (SQLite, WAL mode). Tables: `messages`
(one row per Gmail message ID, with headers, text/HTML bodies, label IDs and derived
flags, sync timestamps), `attachments` (metadata only: part ID, name, type, size,
Content-ID, inline/hidden flags; content is never stored), `messages_fts` (FTS5 over
subject, from, to, cc, body), `labels`, `actions`, `kv`. The schema is versioned with
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
`Code.gs` (and `appsscript.json` if changed) into the Apps Script editor. With a web app
deployment, also publish a new version of it (Deploy → Manage deployments → Edit →
Version: New version); the web app keeps running the old code until then. cloudflared
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
| ChatGPT: connector cannot be created / tools missing | `systemctl --user status school-email-sync-chatgpt`, `curl 127.0.0.1:8081/readyz`; the tunnel must be linked to your ChatGPT workspace, and the key needs Tunnels Read + Use; Refresh the connector after updates |
| ChatGPT: `sending is disabled` | `MCP_ALLOW_SEND=false` in `backend.env` |
| ChatGPT: sent mail not arriving | `list_outgoing` in ChatGPT, or `ses http://127.0.0.1:8766/v1/actions`; Apps Script must be running (`mailbox_status` → `sync`) |
| `get_attachment`: "asks for a Google sign-in" | the web app is not deployed for *Anyone* (or the Workspace forbids it): redeploy, or remove `ATTACHMENT_FETCH_URL` to use the relay |
| `get_attachment`: "rejected the request signature" | `ATTACHMENT_FETCH_SECRET` in `backend.env` ≠ the Script Property |
| `get_attachment`: "answered HTTP … instead of JSON" / "doPost" | wrong `/exec` URL, or the deployed version predates `doPost`: publish a new version |
| `get_attachment`: "has not picked up the request yet" (relay) | the trigger runs once a minute: call again; if it persists, `mailbox_status` → `sync`, and Code.gs must be 1.1.0+ |
| `get_attachment`: "rejected … as expired" | this server's clock is off: `timedatectl` |
| PDF returns "not installed" / runs unsandboxed | `sudo apt install poppler-utils bubblewrap`; the backend logs which mode it uses |
| `~/.local/state/school-email-sync` is a symlink | systemd's legacy-layout migration; stop the service, move `mail.db*` into a real directory, re-run `make install` |

`deploy/smoke-test.sh https://dannysemailsync.naclubs.net` runs the deployment checks in one go:
services, loopback binding, tunnel readiness, the MCP endpoint and ChatGPT tunnel,
public health, authentication enforcement, client-API lockout, and that `/mcp` is
not reachable publicly. It writes nothing.

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
