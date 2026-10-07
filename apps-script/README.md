# School Email Sync — Google Apps Script

This folder is the half of the system that runs **inside your school Google
account**. It is the only part that touches Gmail or holds Google credentials. Every
minute it:

1. polls the Linux backend for queued **send / reply** actions, performs them with
   `GmailApp`, and reports success or failure;
2. reads Gmail changes since the last run (Gmail history API) and pushes new
   messages plus read/label changes to the backend in batches of at most 20.

It only reads mail, sends mail and creates drafts it sends right away. It never
deletes, archives, labels or marks mail as read.

Files to copy:

| File | What it is |
| --- | --- |
| `Code.gs` | All of the script's code |
| `appsscript.json` | Manifest: enables the Gmail advanced service and lists the OAuth scopes |

You will need two values from the server:

* **BACKEND_URL**: `https://dannysemailsync.naclubs.net` (the Cloudflare public hostname, no trailing path)
* **SYNC_SECRET**: run this on the server and copy the value after `SYNC_SECRET=`:

  ```bash
  grep '^SYNC_SECRET=' ~/.config/school-email-sync/backend.env
  ```

  Never use `API_SECRET` here. It is for local tools on the server only.

---

## 1. Create the Apps Script project

1. Sign in to **your school Google account** (check the avatar in the top right).
2. Open <https://script.google.com> → **New project**.
3. Click "Untitled project" and rename it, e.g. `School Email Sync`.

A standalone project is right: it is not tied to any Doc or Sheet.

## 2. Copy the files

1. **Show the manifest:** ⚙️ **Project Settings** → tick **Show "appsscript.json"
   manifest file in editor**.
2. Back in **Editor** (`< >`), open `appsscript.json`, select all, and replace it with
   the contents of `apps-script/appsscript.json` from this repository.
3. Open `Code.gs`, select all, and replace it with the contents of
   `apps-script/Code.gs`.
4. Press **Ctrl+S** (Save).

Adding `appsscript.json` also turns on the **Gmail API advanced service**. Under
**Services** in the left sidebar you should now see `Gmail`. If it is missing,
click **+** next to Services, choose **Gmail API**, keep the identifier `Gmail`, and
click **Add**.

Optional: under Project Settings, set the time zone. It only affects log display;
the sync uses absolute timestamps.

## 3. Configure Script Properties

⚙️ **Project Settings** → **Script Properties** → **Add script property**:

| Property | Value |
| --- | --- |
| `BACKEND_URL` | `https://dannysemailsync.naclubs.net` |
| `SYNC_SECRET` | the 64-character value from the server |

Click **Save script properties**. Script Properties stay out of the code, so you can
update `Code.gs` later without re-entering them.

Optional overrides (same place, same names as `DEFAULTS` at the top of `Code.gs`):
`INITIAL_SYNC_DAYS` (default 30), `BACKFILL_DAYS` (365), `MAX_FETCH_PER_RUN` (80),
`ACTIONS_PER_RUN` (5), `SKIP_LABELS` (`DRAFT,SPAM,TRASH,CHAT`).

## 4. Authorize Gmail permissions

1. In the editor's function menu (next to ▶ Run), choose **`testGmailRead`** and click **Run**.
2. Google shows **Authorization required** → **Review permissions** → choose your
   school account.
3. If you see "Google hasn't verified this app": it is your own script. Click
   **Advanced** → **Go to School Email Sync (unsafe)**.
4. Review and **Allow** the scopes. They are exactly those in `appsscript.json`:
   * *Read, compose, send, and permanently delete all your email from Gmail*
     (`https://mail.google.com/`). Apps Script's `GmailApp` requires this full
     scope to send replies in a thread. The code never deletes anything.
   * *Connect to an external service*: calls to your backend.
   * *Allow this application to run when you are not present*: the trigger.

If your school's admin blocks Apps Script or Gmail access for scripts, the
authorization screen says so. That can only be changed by the Workspace admin.

## 5. Test Gmail reading

Run **`testGmailRead`**. The **Execution log** should show something like:

```
Gmail API OK: you@school.org, 4211 messages, historyId 1234567
GmailApp OK: 3 unread inbox threads
Newest message 19a3f... (thread 19a3f...) at 2026-10-06T21:14:03.000Z
  from: Ms. Rivera <rivera@school.org>
  subject: Field trip form
  labels: INBOX, UNREAD, CATEGORY_UPDATES
  text body: 812 chars, html body: 4122 chars, attachments: 1
```

It is read-only and logs only the newest message's headers, never its body.

## 6. Test Gmail sending

Run **`testGmailSend`**. It sends **one** message to **your own address** with the
subject `[School Email Sync] send test …`. Check your inbox, then delete it if you
like. Nothing else is sent.

## 7. Test the connection to the Linux backend

Run **`testBackendConnectivity`**. Expected log:

```
GET https://dannysemailsync.naclubs.net/healthz -> HTTP 200 {"ok":true}
Authenticated ping OK: backend <version>, server time 2026-10-06T21:15:00Z
Backend connectivity OK.
```

| What you see | Meaning |
| --- | --- |
| `HTTP 401 unauthorized` | `SYNC_SECRET` does not match the server's |
| `HTTP 502` / `HTTP 530` | tunnel connected but the backend is down, or the tunnel is down |
| `HTTP 403` with an HTML page | a Cloudflare security feature (Bot Fight Mode, WAF, Access) is blocking Apps Script; see the root README |
| `Address unavailable` / DNS error | wrong `BACKEND_URL`, or the public hostname is not set up yet |

Optional: run **`runFullSyncNow`** once to start the first import right away. It
works for up to about 5 minutes and logs a summary. Otherwise the trigger starts it.

## 8. Install the recurring trigger

Run **`installTrigger`**. It creates one time-driven trigger that runs `syncTick`
every minute. You can check it under ⏰ **Triggers** in the sidebar.

* Running `installTrigger` again is safe: it removes the existing `syncTick` trigger
  first, so there is never more than one.
* `removeTrigger` stops syncing. Sync position and unreported results are kept, so
  `installTrigger` later resumes where it stopped.

The first run imports the last 30 days (`INITIAL_SYNC_DAYS`) gradually, about 80
messages per minute. After that, an idle run takes about a second: one Gmail call and
one backend call.

## Other helpers

| Function | Purpose |
| --- | --- |
| `showStatus` | sync position, queue length, scans in progress, unreported actions, backoff (never prints the secret) |
| `runFullSyncNow` | one run now, up to about 5 minutes, ignoring backoff |
| `startBackfill` | import older mail (`BACKFILL_DAYS`, default 365) over the next runs; already-stored mail is skipped |
| `resetSyncState` | forget the local sync position; the next run re-scans 30 days, and the backend skips what it already has |

## How it behaves

* **Incremental:** Gmail's history API returns only what changed since the last run:
  new messages, read/unread, labels, deletions. If Gmail's history has expired
  (about a week offline), the script re-scans recent days automatically.
* **Deduplication:** the backend is authoritative. Before downloading a message the
  script asks the backend which IDs it lacks, and the backend stores each Gmail
  message ID once, however often it is delivered.
* **Bounded:** each trigger run stops starting new work after 45 s. Uploads are
  limited to 20 messages or about 2 MB. Text bodies are cut at 200,000 characters
  and HTML bodies at 400,000 (flagged as truncated). Attachments are recorded as
  metadata only: name, type, size, Gmail attachment ID.
* **Skipped:** drafts, spam, trash and chats (`SKIP_LABELS`).
* **Server offline:** nothing is lost. The script keeps its position, backs off (1,
  2, 4, 8, then 15 minutes) and catches up when the server returns.
* **Sending is at most once:** before sending, the script records the action in its
  Script Properties. If the "sent" report cannot reach the server, it is
  re-delivered later instead of sending again. If a run dies in the middle of
  sending, the action is reported as *outcome unknown* and not retried; check the
  Sent folder. Temporary Gmail errors (quota, rate limits) are reported as
  retryable and the backend retries with backoff. Permanent errors (bad address)
  fail immediately.
* **Replies** use Gmail's draft-reply functions, so they stay in the original
  conversation with the same subject and references.
* **Logging:** one summary line per run that did something, plus warnings and errors.
  Message bodies are never logged.

## Quotas

Google Workspace accounts allow 6 hours of trigger runtime per day. A one-minute
trigger that is idle about 1 s per run uses roughly 25 minutes a day. UrlFetch (about
1,500 calls a day when idle) and Gmail read quotas are far below their limits. GmailApp
sending is subject to your account's daily send limit. If it is reached, send actions
are reported as retryable and retried later.

## Updating

Paste the new `Code.gs` over the old one and save. Script Properties, the trigger
and the sync state are kept. If `appsscript.json` changed, update it too and run
any function once to accept new permissions.
