/**
 * School Email Sync — Google Apps Script half.
 *
 * Runs under the school Google account. Every minute (installTrigger) it:
 *   1. reports any outbound-action results that could not be delivered earlier,
 *   2. polls the Linux backend for pending send/reply actions and performs them,
 *   3. reads Gmail changes since the last run (Gmail history API) and pushes new
 *      messages and label/read-state changes to the backend in bounded batches.
 *
 * Google credentials never leave Apps Script. The backend only sees normalized
 * message JSON over HTTPS, authenticated with SYNC_SECRET.
 *
 * Script Properties (Project Settings → Script Properties):
 *   BACKEND_URL   https://<your hostname>        (required, https only)
 *   SYNC_SECRET   shared secret from the server  (required)
 * Optional overrides use the names in DEFAULTS below (e.g. INITIAL_SYNC_DAYS).
 *
 * Manual helpers (select in the editor's function menu, then Run):
 *   testGmailRead, testGmailSend, testBackendConnectivity, runFullSyncNow,
 *   installTrigger, removeTrigger, showStatus, startBackfill, resetSyncState
 */

var VERSION = '1.0.0';

var DEFAULTS = {
  RUN_BUDGET_MS: 45000,          // trigger runs stop starting new work after this
  MANUAL_RUN_BUDGET_MS: 300000,  // runFullSyncNow (Apps Script hard limit is 6 min)
  INITIAL_SYNC_DAYS: 30,         // how far back the first run imports
  RECOVERY_SYNC_DAYS_MAX: 30,    // cap for the re-scan after Gmail history expires
  BACKFILL_DAYS: 365,            // startBackfill()
  MAX_FETCH_PER_RUN: 80,         // full message reads per run
  BATCH_MAX_MESSAGES: 20,        // messages per upload
  BATCH_MAX_CHARS: 2000000,      // approximate characters per upload
  MAX_TEXT_CHARS: 200000,        // plain-text body cap (rest is dropped, flagged truncated)
  MAX_HTML_CHARS: 400000,        // HTML body cap
  MAX_HEADER_CHARS: 8000,
  ACTIONS_PER_RUN: 5,
  LABEL_REFRESH_HOURS: 6,
  QUEUE_MAX: 400,                // pending message IDs kept in Script Properties
  SKIP_LABELS: 'DRAFT,SPAM,TRASH,CHAT'
};

var PROP = {
  STATE: 'SES_STATE',            // {historyId, sweep, labelsAt, lastOkAt}
  QUEUE: 'SES_QUEUE',            // message IDs waiting to be fetched and uploaded
  BACKOFF: 'SES_BACKOFF',        // {until, failures} while the backend is unreachable
  JOURNAL: 'SES_J_'              // + action ID: local record of outbound actions
};

var DAY_MS = 86400000;
var JOURNAL_MAX_AGE_MS = 14 * DAY_MS;
var ID_RE = /^[A-Za-z0-9_-]{1,128}$/;

// ---------------------------------------------------------------------------
// Errors

class BackendUnavailableError extends Error {
  constructor(message) { super(message); this.name = 'BackendUnavailableError'; }
}

class BackendRejectedError extends Error {
  constructor(status, code, message) {
    super('HTTP ' + status + (code ? ' ' + code : '') + ': ' + message);
    this.name = 'BackendRejectedError';
    this.status = status;
    this.code = code;
  }
}

class ConfigError extends Error {
  constructor(message) { super(message); this.name = 'ConfigError'; }
}

function errMsg_(e) {
  return String((e && e.message) || e);
}

/** Gmail/Apps Script errors that are worth retrying later. */
function isTransientGmailError_(e) {
  return /rate ?limit|too many|quota|service invoked|try again|temporar|timed? ?out|backend error|internal error|service error|unavailable|server error|empty response/i
    .test(errMsg_(e));
}

function isNotFound_(e) {
  return /not ?found|404/i.test(errMsg_(e)) && !isTransientGmailError_(e);
}

// ---------------------------------------------------------------------------
// Configuration and per-run context

function readConfig_(all) {
  var cfg = {};
  Object.keys(DEFAULTS).forEach(function (k) {
    var v = all[k];
    if (v === undefined || v === null || v === '') {
      cfg[k] = DEFAULTS[k];
    } else if (typeof DEFAULTS[k] === 'number') {
      var n = Number(v);
      if (!isFinite(n) || n <= 0) throw new ConfigError('Script Property ' + k + ' must be a positive number');
      cfg[k] = n;
    } else {
      cfg[k] = String(v);
    }
  });
  var url = String(all.BACKEND_URL || '').trim().replace(/\/+$/, '');
  if (!url) throw new ConfigError('Script Property BACKEND_URL is not set');
  if (!/^https:\/\/[^\s/]+$/.test(url)) {
    throw new ConfigError('BACKEND_URL must be https://<hostname> with no path (got "' + url + '")');
  }
  var secret = String(all.SYNC_SECRET || '').trim();
  if (secret.length < 32) throw new ConfigError('Script Property SYNC_SECRET is missing or shorter than 32 characters');
  cfg.BACKEND_URL = url;
  cfg.SYNC_SECRET = secret;
  cfg.skip = {};
  cfg.SKIP_LABELS.split(',').forEach(function (l) { if (l.trim()) cfg.skip[l.trim()] = true; });
  return cfg;
}

function newRun_(manual) {
  var props = PropertiesService.getScriptProperties();
  var all = props.getProperties();
  var cfg = readConfig_(all);
  var now = Date.now();
  return {
    props: props,
    all: all,
    cfg: cfg,
    manual: manual,
    start: now,
    deadline: now + (manual ? cfg.MANUAL_RUN_BUDGET_MS : cfg.RUN_BUDGET_MS),
    fetchCap: manual ? cfg.MAX_FETCH_PER_RUN * 5 : cfg.MAX_FETCH_PER_RUN,
    state: parseJSON_(all[PROP.STATE], {}),
    queue: parseJSON_(all[PROP.QUEUE], []),
    savedState: all[PROP.STATE] || '',
    savedQueue: all[PROP.QUEUE] || '',
    stats: {
      historyChanges: 0, stateUpdated: 0, fetched: 0, inserted: 0, updated: 0, unchanged: 0,
      rejected: 0, skipped: 0, actionsDone: 0, actionsFailed: 0
    }
  };
}

function timeLeft_(run) {
  return run.deadline - Date.now();
}

function parseJSON_(s, fallback) {
  if (!s) return fallback;
  try { return JSON.parse(s); } catch (e) { return fallback; }
}

/** Writes sync state and queue together, and only when they changed. */
function persist_(run) {
  var state = JSON.stringify(run.state);
  var queue = JSON.stringify(run.queue);
  if (state === run.savedState && queue === run.savedQueue) return;
  var update = {};
  update[PROP.STATE] = state;
  update[PROP.QUEUE] = queue;
  run.props.setProperties(update);
  run.savedState = state;
  run.savedQueue = queue;
}

// ---------------------------------------------------------------------------
// Backend HTTP

/**
 * Calls the backend. Retries network errors, 429 and 5xx (including Cloudflare's
 * 52x/530 when the server or tunnel is down) a couple of times within the run's
 * time budget, then throws BackendUnavailableError. Other statuses throw
 * BackendRejectedError immediately.
 */
function api_(run, method, path, body) {
  var params = {
    method: method,
    muteHttpExceptions: true,
    followRedirects: false,
    headers: { Authorization: 'Bearer ' + run.cfg.SYNC_SECRET }
  };
  if (body !== undefined) {
    params.contentType = 'application/json';
    params.payload = JSON.stringify(body);
  }
  var delays = [0, 1000, 4000];
  var last = '';
  for (var i = 0; i < delays.length; i++) {
    if (delays[i]) {
      if (timeLeft_(run) < delays[i] + 8000) break;
      Utilities.sleep(delays[i]);
    }
    var resp;
    try {
      resp = UrlFetchApp.fetch(run.cfg.BACKEND_URL + path, params);
    } catch (e) {
      last = errMsg_(e);
      continue;
    }
    var code = resp.getResponseCode();
    var text = resp.getContentText() || '';
    if (code >= 200 && code < 300) return text ? JSON.parse(text) : {};
    if (code === 429 || code >= 500) {
      last = 'HTTP ' + code;
      continue;
    }
    var info = parseJSON_(text, {}).error || {};
    var msg = info.message || text.replace(/\s+/g, ' ').slice(0, 200);
    if (code >= 300 && code < 400) msg = 'unexpected redirect (is Cloudflare Access or a redirect rule in front of the backend?)';
    throw new BackendRejectedError(code, info.code || '', msg);
  }
  throw new BackendUnavailableError(method.toUpperCase() + ' ' + path + ': ' + last);
}

// ---------------------------------------------------------------------------
// Trigger entry point

/** Installed as a one-minute time-driven trigger by installTrigger(). */
function syncTick() {
  var lock = LockService.getScriptLock();
  if (!lock.tryLock(500)) return; // the previous run is still working
  try {
    runSync_(false);
  } finally {
    lock.releaseLock();
  }
}

function runSync_(manual) {
  var run;
  try {
    run = newRun_(manual);
  } catch (e) {
    if (manual || !(e instanceof ConfigError)) throw e;
    console.error('sync: ' + errMsg_(e));
    return null;
  }
  if (!manual) {
    var b = parseJSON_(run.all[PROP.BACKOFF], null);
    if (b && b.until > Date.now()) return null;
  }
  try {
    flushJournal_(run);
    processActions_(run);
    refreshLabelsIfDue_(run);
    syncMail_(run);
    if (run.all[PROP.BACKOFF]) run.props.deleteProperty(PROP.BACKOFF);
  } catch (e) {
    persist_(run);
    if (e instanceof BackendUnavailableError) {
      var wait = backoff_(run, 0);
      console.warn('sync: backend unreachable (' + e.message + '); next try in ' + Math.round(wait / 1000) + 's');
    } else if (e instanceof BackendRejectedError && (e.status === 401 || e.status === 403)) {
      backoff_(run, 15 * 60000);
      console.error('sync: backend refused the request (' + e.message + '). Check SYNC_SECRET and Cloudflare settings; retrying in 15 min.');
    } else if (isTransientGmailError_(e)) {
      console.warn('sync: Gmail temporarily unavailable: ' + errMsg_(e));
    } else {
      logSummary_(run, true);
      throw e; // a real bug: let Apps Script record the failure
    }
    if (manual) throw e;
  }
  logSummary_(run, manual);
  return run.stats;
}

/** Exponential backoff while the backend is unreachable: 1, 2, 4, 8, then 15 min. */
function backoff_(run, fixedMs) {
  var prev = parseJSON_(run.all[PROP.BACKOFF], { failures: 0 });
  var failures = (prev.failures || 0) + 1;
  var wait = fixedMs || Math.min(15 * 60000, 60000 * Math.pow(2, failures - 1));
  run.props.setProperty(PROP.BACKOFF, JSON.stringify({ until: Date.now() + wait, failures: failures }));
  return wait;
}

function logSummary_(run, force) {
  var s = run.stats;
  var busy = s.historyChanges || s.inserted || s.updated || s.stateUpdated || s.rejected ||
    s.actionsDone || s.actionsFailed || run.queue.length || run.state.sweep;
  if (!busy && !force) return;
  var parts = [
    'new ' + s.inserted,
    'state ' + (s.updated + s.stateUpdated),
    'fetched ' + s.fetched
  ];
  if (s.unchanged) parts.push('already stored ' + s.unchanged);
  if (s.skipped) parts.push('skipped ' + s.skipped);
  if (s.rejected) parts.push('rejected ' + s.rejected);
  if (s.actionsDone || s.actionsFailed) parts.push('actions ok ' + s.actionsDone + ' failed ' + s.actionsFailed);
  if (run.queue.length) parts.push('queued ' + run.queue.length);
  if (run.state.sweep) parts.push(run.state.sweep.kind + ' scan in progress (' + run.state.sweep.scanned + ' checked)');
  console.log('sync: ' + parts.join(', ') + ' [' + ((Date.now() - run.start) / 1000).toFixed(1) + 's]');
}

// ---------------------------------------------------------------------------
// Outbound actions
//
// Before performing an action the script writes a journal entry ("attempting"),
// and after Gmail accepts it, "sent" with the new message ID. Reports to the
// backend are retried from the journal, so a lost report never causes a second
// email: an action whose journal says "sent" is only ever reported, and one
// still "attempting" (the script died mid-send) is reported as an uncertain
// failure instead of being retried.

function processActions_(run) {
  if (timeLeft_(run) < 15000) return;
  var resp = api_(run, 'post', '/v1/actions/poll', { limit: run.cfg.ACTIONS_PER_RUN });
  var actions = resp.actions || [];
  for (var i = 0; i < actions.length; i++) {
    if (timeLeft_(run) < 10000) {
      console.warn('actions: out of time; ' + (actions.length - i) + ' action(s) will be offered again after their lease');
      return;
    }
    executeAction_(run, actions[i]);
  }
}

function journalKey_(id) {
  return PROP.JOURNAL + id;
}

function executeAction_(run, action) {
  var key = journalKey_(action.id);
  var prior = parseJSON_(run.props.getProperty(key), null);
  if (prior && prior.s === 'failed') {
    // A failure report never reached the backend and the lease expired: start fresh.
    run.props.deleteProperty(key);
    prior = null;
  }
  if (prior) {
    prior.c = action.claimId;
    run.props.setProperty(key, JSON.stringify(prior));
    reportJournal_(run, action.id, prior);
    return;
  }

  var entry = { s: 'attempting', c: action.claimId, type: action.type, t: Date.now() };
  run.props.setProperty(key, JSON.stringify(entry));
  try {
    entry.r = performAction_(action);
    entry.s = 'sent';
  } catch (e) {
    entry.s = 'failed';
    entry.e = errMsg_(e).slice(0, 500);
    entry.retry = isTransientGmailError_(e);
  }
  entry.t = Date.now();
  run.props.setProperty(key, JSON.stringify(entry));
  reportJournal_(run, action.id, entry);
}

/** Sends one journal entry's outcome to the backend; removes it once acknowledged. */
function reportJournal_(run, id, entry) {
  var key = journalKey_(id);
  var path = '/v1/actions/' + encodeURIComponent(id);
  try {
    if (entry.s === 'sent') {
      api_(run, 'post', path + '/complete', { claimId: entry.c || '', result: entry.r || {} });
      run.stats.actionsDone++;
      console.log('action ' + id + ' (' + entry.type + '): sent as message ' + (entry.r && entry.r.messageId));
    } else {
      var uncertain = entry.s === 'attempting';
      var msg = uncertain
        ? 'outcome unknown: the script stopped while performing this action, so it was not retried (check the Sent folder)'
        : entry.e;
      api_(run, 'post', path + '/fail', { claimId: entry.c, error: msg, retryable: !uncertain && !!entry.retry });
      run.stats.actionsFailed++;
      console.warn('action ' + id + ' (' + entry.type + '): ' + (uncertain ? 'uncertain' : 'failed') + ' - ' + msg);
    }
    run.props.deleteProperty(key);
  } catch (e) {
    if (e instanceof BackendRejectedError && e.status === 404) {
      run.props.deleteProperty(key);
      console.warn('action ' + id + ': unknown to the backend; dropping its journal entry');
    } else if (e instanceof BackendRejectedError && e.status === 409) {
      // The claim expired. Keep an "attempting" entry so the next delivery of this
      // action is reported, not re-sent; drop a "failed" one so it is retried.
      if (entry.s === 'failed') run.props.deleteProperty(key);
    } else {
      throw e; // backend unreachable: the entry stays and is retried next run
    }
  }
}

/** Retries reports that could not be delivered in an earlier run. */
function flushJournal_(run) {
  Object.keys(run.all).forEach(function (k) {
    if (k.indexOf(PROP.JOURNAL) !== 0) return;
    var id = k.slice(PROP.JOURNAL.length);
    var entry = parseJSON_(run.all[k], null);
    if (!entry) {
      run.props.deleteProperty(k);
      return;
    }
    if (Date.now() - (entry.t || 0) > JOURNAL_MAX_AGE_MS) {
      console.error('action ' + id + ': result (' + entry.s + ') could not be reported for 14 days; dropping it');
      run.props.deleteProperty(k);
      return;
    }
    reportJournal_(run, id, entry);
  });
}

function performAction_(action) {
  var p = action.payload || {};
  var opts = {};
  if (p.cc && p.cc.length) opts.cc = p.cc.join(',');
  if (p.bcc && p.bcc.length) opts.bcc = p.bcc.join(',');
  if (p.htmlBody) opts.htmlBody = p.htmlBody;
  var draft;
  if (action.type === 'send') {
    if (!p.to || !p.to.length) throw new Error('send: no recipients');
    draft = GmailApp.createDraft(p.to.join(','), p.subject || '', p.body || '', opts);
  } else if (action.type === 'reply') {
    var target = findReplyTarget_(p);
    // Draft replies stay in the original Gmail thread (same subject and references).
    draft = p.replyAll ? target.createDraftReplyAll(p.body || '', opts) : target.createDraftReply(p.body || '', opts);
  } else {
    throw new Error('unsupported action type: ' + action.type);
  }
  var sent = draft.send();
  return { messageId: sent.getId(), threadId: sent.getThread().getId() };
}

function findReplyTarget_(p) {
  if (p.messageId) {
    var m = null;
    try {
      m = GmailApp.getMessageById(p.messageId);
    } catch (e) {
      if (isTransientGmailError_(e)) throw e;
    }
    if (m) return m;
    if (!p.threadId) throw new Error('reply: message ' + p.messageId + ' was not found in Gmail');
  }
  var thread = null;
  try {
    thread = GmailApp.getThreadById(p.threadId);
  } catch (e) {
    if (isTransientGmailError_(e)) throw e;
  }
  if (!thread) throw new Error('reply: thread ' + p.threadId + ' was not found in Gmail');
  var msgs = thread.getMessages().filter(function (x) { return !x.isDraft(); });
  if (!msgs.length) throw new Error('reply: thread ' + p.threadId + ' has no messages');
  // Reply to the newest message someone else sent, so the reply is not addressed to ourselves.
  var me = String(Gmail.Users.getProfile('me').emailAddress || '').toLowerCase();
  for (var i = msgs.length - 1; i >= 0; i--) {
    if (String(msgs[i].getFrom()).toLowerCase().indexOf(me) === -1) return msgs[i];
  }
  return msgs[msgs.length - 1];
}

// ---------------------------------------------------------------------------
// Incoming mail

function refreshLabelsIfDue_(run) {
  var at = run.state.labelsAt || 0;
  if (Date.now() - at < run.cfg.LABEL_REFRESH_HOURS * 3600000 || timeLeft_(run) < 20000) return;
  var resp = Gmail.Users.Labels.list('me');
  var labels = (resp.labels || [])
    .filter(function (l) { return ID_RE.test(l.id) && l.name; })
    .slice(0, 2000)
    .map(function (l) {
      return { id: l.id, name: String(l.name).slice(0, 500), type: l.type === 'system' ? 'system' : 'user' };
    });
  api_(run, 'post', '/v1/sync/labels', { labels: labels });
  run.state.labelsAt = Date.now();
  persist_(run);
}

function syncMail_(run) {
  var st = run.state;
  if (!st.historyId) {
    var profile = Gmail.Users.getProfile('me');
    st.historyId = String(profile.historyId);
    st.sweep = newSweep_(run.cfg.INITIAL_SYNC_DAYS, 'initial');
    persist_(run);
    console.log('sync: first run for ' + profile.emailAddress + '; importing the last ' +
      run.cfg.INITIAL_SYNC_DAYS + ' days of mail over the next runs');
  }
  if (!readHistory_(run)) {
    persist_(run);
    return;
  }
  drainQueue_(run);
  while (run.state.sweep && run.queue.length < run.cfg.QUEUE_MAX && timeLeft_(run) > 15000 &&
    run.stats.fetched < run.fetchCap) {
    sweepPage_(run);
    drainQueue_(run);
  }
  run.state.lastOkAt = Date.now();
  persist_(run);
}

function newSweep_(days, kind) {
  var after = Math.floor((Date.now() - days * DAY_MS) / 1000);
  return { kind: kind, q: 'after:' + after + ' -in:drafts -in:chats', pageToken: '', scanned: 0 };
}

function isSkippable_(run, labelIds) {
  return (labelIds || []).some(function (l) { return run.cfg.skip[l]; });
}

function enqueue_(run, ids) {
  var have = {};
  run.queue.forEach(function (id) { have[id] = true; });
  var overflow = false;
  ids.forEach(function (id) {
    if (have[id]) return;
    if (run.queue.length >= run.cfg.QUEUE_MAX) {
      overflow = true;
      return;
    }
    run.queue.push(id);
    have[id] = true;
  });
  if (overflow && !run.state.sweep) {
    // Too much to remember individually; a scan will find whatever was dropped.
    run.state.sweep = newSweep_(recoveryDays_(run), 'catch-up');
  }
}

function recoveryDays_(run) {
  var since = run.state.lastOkAt ? Math.ceil((Date.now() - run.state.lastOkAt) / DAY_MS) + 1 : 7;
  return Math.max(1, Math.min(run.cfg.RECOVERY_SYNC_DAYS_MAX, since));
}

/**
 * Reads Gmail history since the stored historyId: new messages, label changes
 * (read/unread, archive, labels) and deletions. Label changes go straight to the
 * backend; IDs the backend does not have are queued for a full fetch. Returns
 * false if out of time (nothing is committed, the next run re-reads).
 */
function readHistory_(run) {
  var st = run.state;
  var touched = {};
  var order = [];
  var touch = function (msg, deleted) {
    if (!msg || !msg.id) return;
    var t = touched[msg.id];
    if (!t) {
      t = touched[msg.id] = {};
      order.push(msg.id);
    }
    if (deleted) t.deleted = true;
    else if (msg.labelIds) t.labelIds = msg.labelIds;
  };

  var pageToken = '';
  var newest = st.historyId;
  var pages = 0;
  do {
    if (timeLeft_(run) < 15000) return false;
    var resp;
    try {
      var opts = { startHistoryId: st.historyId, maxResults: 500 };
      if (pageToken) opts.pageToken = pageToken;
      resp = Gmail.Users.History.list('me', opts);
    } catch (e) {
      if (isNotFound_(e)) return restartFromNow_(run, 'Gmail history for the stored position has expired');
      throw e;
    }
    (resp.history || []).forEach(function (h) {
      (h.messagesAdded || []).forEach(function (x) { touch(x.message, false); });
      (h.labelsAdded || []).forEach(function (x) { touch(x.message, false); });
      (h.labelsRemoved || []).forEach(function (x) { touch(x.message, false); });
      (h.messagesDeleted || []).forEach(function (x) { touch(x.message, true); });
    });
    if (resp.historyId) newest = String(resp.historyId);
    pageToken = resp.nextPageToken || '';
    pages++;
  } while (pageToken && pages < 20);
  if (pageToken) return restartFromNow_(run, 'more than 10,000 Gmail changes since the last run');

  run.stats.historyChanges = order.length;
  var updates = [];
  var needCheck = [];
  order.forEach(function (id) {
    var t = touched[id];
    if (!ID_RE.test(id)) return;
    if (t.deleted) updates.push({ id: id, deleted: true });
    else if (t.labelIds) updates.push({ id: id, labelIds: cleanLabels_(t.labelIds) });
    else needCheck.push(id);
  });
  for (var i = 0; i < updates.length; i += 500) {
    var res = api_(run, 'post', '/v1/sync/state', { updates: updates.slice(i, i + 500) });
    run.stats.stateUpdated += res.updated || 0;
    enqueue_(run, (res.unknown || []).filter(function (id) {
      return !isSkippable_(run, touched[id] && touched[id].labelIds);
    }));
  }
  if (needCheck.length) {
    var known = api_(run, 'post', '/v1/sync/known', { ids: needCheck.slice(0, 1000) });
    enqueue_(run, known.unknown || []);
  }
  st.historyId = newest;
  persist_(run);
  return true;
}

function restartFromNow_(run, why) {
  var profile = Gmail.Users.getProfile('me');
  var days = recoveryDays_(run);
  run.state.historyId = String(profile.historyId);
  run.state.sweep = newSweep_(days, 'recovery');
  persist_(run);
  console.warn('sync: ' + why + '; re-scanning the last ' + days + ' days (already stored mail is skipped)');
  return true;
}

/** One page of a backfill/recovery scan: list IDs, keep the ones the backend lacks. */
function sweepPage_(run) {
  var sw = run.state.sweep;
  var opts = { q: sw.q, maxResults: 200, includeSpamTrash: false };
  if (sw.pageToken) opts.pageToken = sw.pageToken;
  var resp = Gmail.Users.Messages.list('me', opts);
  var ids = (resp.messages || []).map(function (m) { return m.id; }).filter(function (id) { return ID_RE.test(id); });
  if (ids.length) {
    var known = api_(run, 'post', '/v1/sync/known', { ids: ids });
    enqueue_(run, known.unknown || []);
    sw.scanned += ids.length;
  }
  sw.pageToken = resp.nextPageToken || '';
  if (!sw.pageToken) {
    console.log('sync: ' + sw.kind + ' scan finished (' + sw.scanned + ' messages checked)');
    run.state.sweep = null;
  }
  persist_(run);
}

/** Fetches queued messages, normalizes them and uploads them in bounded batches. */
function drainQueue_(run) {
  var ids = run.queue.slice(0, Math.max(0, run.fetchCap - run.stats.fetched));
  var batch = [];
  var done = [];
  var chars = 0;

  var flush = function () {
    if (batch.length) {
      var res = api_(run, 'post', '/v1/sync/messages', { messages: batch });
      run.stats.inserted += res.inserted || 0;
      run.stats.updated += res.updated || 0;
      run.stats.unchanged += res.unchanged || 0;
      run.stats.rejected += res.rejected || 0;
      (res.results || []).forEach(function (r) {
        if (r.status === 'rejected') console.warn('sync: backend rejected message ' + r.id + ': ' + r.error);
      });
    }
    if (done.length) {
      var gone = {};
      done.forEach(function (id) { gone[id] = true; });
      run.queue = run.queue.filter(function (id) { return !gone[id]; });
      persist_(run);
    }
    batch = [];
    done = [];
    chars = 0;
  };

  for (var i = 0; i < ids.length; i++) {
    if (timeLeft_(run) < 12000) break;
    var id = ids[i];
    var msg;
    try {
      msg = Gmail.Users.Messages.get('me', id, { format: 'full' });
      run.stats.fetched++;
    } catch (e) {
      if (isTransientGmailError_(e)) throw e;
      // Deleted before we got to it, or unreadable: drop it from the queue.
      console.warn('sync: cannot read message ' + id + ' (' + errMsg_(e) + '); skipping');
      run.stats.skipped++;
      done.push(id);
      continue;
    }
    if (isSkippable_(run, msg.labelIds)) {
      run.stats.skipped++;
      done.push(id);
      continue;
    }
    var n = normalizeMessage_(msg, run.cfg);
    var size = (n.bodyText || '').length + (n.bodyHtml || '').length + 4000;
    if (batch.length && (batch.length >= run.cfg.BATCH_MAX_MESSAGES || chars + size > run.cfg.BATCH_MAX_CHARS)) flush();
    batch.push(n);
    done.push(id);
    chars += size;
  }
  flush();
}

// ---------------------------------------------------------------------------
// Normalization (Gmail API message resource → backend JSON)

function cleanLabels_(ids) {
  return (ids || []).filter(function (l) { return ID_RE.test(l); }).slice(0, 100);
}

function clip_(s, max) {
  s = String(s || '');
  return s.length > max ? { text: s.slice(0, max), truncated: true } : { text: s, truncated: false };
}

function headerMap_(headers) {
  var m = {};
  (headers || []).forEach(function (h) {
    var k = String(h.name || '').toLowerCase();
    if (k && !(k in m)) m[k] = String(h.value || '');
  });
  return m;
}

function normalizeMessage_(msg, cfg) {
  var payload = msg.payload || {};
  var hdr = headerMap_(payload.headers);
  var h = function (name) { return clip_(hdr[name] || '', cfg.MAX_HEADER_CHARS).text; };
  var out = {
    id: msg.id,
    threadId: msg.threadId,
    historyId: String(msg.historyId || ''),
    internalDate: Number(msg.internalDate),
    labelIds: cleanLabels_(msg.labelIds),
    snippet: clip_(msg.snippet || '', 1000).text,
    sizeEstimate: Number(msg.sizeEstimate || 0),
    from: h('from'),
    to: h('to'),
    cc: h('cc'),
    bcc: h('bcc'),
    replyTo: h('reply-to'),
    subject: h('subject'),
    dateHeader: h('date'),
    messageIdHeader: h('message-id'),
    inReplyTo: h('in-reply-to'),
    references: h('references'),
    bodyText: null,
    bodyTextTruncated: false,
    bodyHtml: null,
    bodyHtmlTruncated: false,
    attachments: []
  };
  var texts = [];
  var htmls = [];
  walkParts_(msg.id, payload, texts, htmls, out.attachments, 0);
  if (texts.length) {
    var t = clip_(texts.join('\n'), cfg.MAX_TEXT_CHARS);
    out.bodyText = t.text;
    out.bodyTextTruncated = t.truncated;
  }
  if (htmls.length) {
    var x = clip_(htmls.join('\n'), cfg.MAX_HTML_CHARS);
    out.bodyHtml = x.text;
    out.bodyHtmlTruncated = x.truncated;
  }
  var seenParts = {};
  out.attachments = out.attachments.slice(0, 100).map(function (a) {
    if (a.partId && seenParts[a.partId]) a.partId = ''; // backend keys these by position instead
    seenParts[a.partId] = true;
    return a;
  });
  return out;
}

function walkParts_(msgId, part, texts, htmls, atts, depth) {
  if (!part || depth > 20) return;
  var mime = String(part.mimeType || '').toLowerCase();
  var body = part.body || {};
  var filename = String(part.filename || '');
  var ph = headerMap_(part.headers);
  var disposition = (ph['content-disposition'] || '').toLowerCase();

  if (mime.indexOf('multipart/') === 0) {
    (part.parts || []).forEach(function (p) { walkParts_(msgId, p, texts, htmls, atts, depth + 1); });
    return;
  }
  var isText = mime === 'text/plain' || mime === 'text/html';
  if (filename || disposition.indexOf('attachment') === 0 || (body.attachmentId && !isText)) {
    // Metadata only. The attachment content stays in Gmail.
    atts.push({
      partId: String(part.partId || '').replace(/[^0-9A-Za-z.]/g, '').slice(0, 64),
      filename: filename.slice(0, 500),
      mimeType: mime.slice(0, 200),
      size: Number(body.size || 0),
      attachmentId: String(body.attachmentId || '').slice(0, 4000),
      contentId: (ph['content-id'] || '').replace(/[<>\s]/g, '').slice(0, 900),
      inline: disposition.indexOf('inline') === 0
    });
    return;
  }
  if (isText) {
    try {
      var s = decodePartBody_(msgId, part, charsetOf_(ph['content-type']));
      if (s) (mime === 'text/plain' ? texts : htmls).push(s);
    } catch (e) {
      if (isTransientGmailError_(e)) throw e;
      console.warn('sync: could not decode a ' + mime + ' part of message ' + msgId + ': ' + errMsg_(e));
    }
    return;
  }
  // e.g. message/rfc822 without a filename: include its readable parts.
  (part.parts || []).forEach(function (p) { walkParts_(msgId, p, texts, htmls, atts, depth + 1); });
}

function charsetOf_(contentType) {
  var m = /charset\s*=\s*"?([^";\s]+)"?/i.exec(contentType || '');
  return m ? m[1] : 'UTF-8';
}

function decodePartBody_(msgId, part, charset) {
  var body = part.body || {};
  var data = body.data;
  if (!data && body.attachmentId) {
    // Gmail moves large text bodies out of line; fetch them unless absurdly big.
    if (Number(body.size || 0) > 5000000) return '';
    var att = Gmail.Users.Messages.Attachments.get('me', msgId, body.attachmentId);
    data = att && att.data;
  }
  if (!data || !data.length) return '';
  return bytesToString_(data, charset);
}

/** Accepts the body data as a byte array or as (URL-safe) base64 text. */
function bytesToString_(data, charset) {
  var bytes = data;
  if (typeof data === 'string') {
    var b64 = data.replace(/-/g, '+').replace(/_/g, '/').replace(/\s+/g, '');
    while (b64.length % 4) b64 += '=';
    bytes = Utilities.base64Decode(b64);
  }
  try {
    return Utilities.newBlob(bytes).getDataAsString(charset);
  } catch (e) {
    return Utilities.newBlob(bytes).getDataAsString('UTF-8');
  }
}

// ---------------------------------------------------------------------------
// Manual helpers (run from the Apps Script editor)

/** Checks that this account can read Gmail. Read-only; logs no message bodies. */
function testGmailRead() {
  var profile = Gmail.Users.getProfile('me');
  console.log('Gmail API OK: ' + profile.emailAddress + ', ' + profile.messagesTotal + ' messages, historyId ' + profile.historyId);
  console.log('GmailApp OK: ' + GmailApp.getInboxUnreadCount() + ' unread inbox threads');
  var list = Gmail.Users.Messages.list('me', { maxResults: 1, q: '-in:drafts -in:chats' });
  if (!list.messages || !list.messages.length) {
    console.log('No messages to sample.');
    return;
  }
  var cfg = readConfigLenient_();
  var n = normalizeMessage_(Gmail.Users.Messages.get('me', list.messages[0].id, { format: 'full' }), cfg);
  console.log('Newest message ' + n.id + ' (thread ' + n.threadId + ') at ' + new Date(n.internalDate).toISOString() +
    '\n  from: ' + n.from +
    '\n  subject: ' + n.subject +
    '\n  labels: ' + n.labelIds.join(', ') +
    '\n  text body: ' + (n.bodyText === null ? 'none' : n.bodyText.length + ' chars') +
    ', html body: ' + (n.bodyHtml === null ? 'none' : n.bodyHtml.length + ' chars') +
    ', attachments: ' + n.attachments.length);
}

/** Sends one test email to your own address (and nowhere else). */
function testGmailSend() {
  var me = Gmail.Users.getProfile('me').emailAddress;
  var subject = '[School Email Sync] send test ' + new Date().toISOString();
  var sent = GmailApp.createDraft(me, subject,
    'This test message was sent by testGmailSend() to your own address. You can delete it.').send();
  console.log('Sent a test email to ' + me + ' (message ' + sent.getId() + '). Check your inbox.');
}

/** Checks HTTPS reachability, the shared secret, and the backend API. Sends nothing to Gmail. */
function testBackendConnectivity() {
  var run = newRun_(true);
  var health = UrlFetchApp.fetch(run.cfg.BACKEND_URL + '/healthz', { muteHttpExceptions: true, followRedirects: false });
  var code = health.getResponseCode();
  console.log('GET ' + run.cfg.BACKEND_URL + '/healthz -> HTTP ' + code + ' ' + health.getContentText().slice(0, 120));
  if (code !== 200) {
    throw new Error('Backend health check failed (HTTP ' + code + '). 502/530 usually means the server or tunnel is down; ' +
      '403 or an HTML page usually means a Cloudflare security setting is blocking Apps Script.');
  }
  var ping = api_(run, 'get', '/v1/sync/ping');
  console.log('Authenticated ping OK: backend ' + ping.version + ', server time ' + ping.serverTime);
  api_(run, 'post', '/v1/sync/known', { ids: [] });
  console.log('Backend connectivity OK.');
}

/** Runs one sync now (up to ~5 minutes of work), ignoring any backoff. */
function runFullSyncNow() {
  var lock = LockService.getScriptLock();
  if (!lock.tryLock(30000)) throw new Error('A sync run is in progress; try again in a minute.');
  try {
    var stats = runSync_(true);
    console.log('Run finished: ' + JSON.stringify(stats));
  } finally {
    lock.releaseLock();
  }
}

/** Installs the one-minute trigger. Safe to run again: it replaces any existing one. */
function installTrigger() {
  readConfig_(PropertiesService.getScriptProperties().getProperties()); // fail early on bad config
  removeTrigger();
  ScriptApp.newTrigger('syncTick').timeBased().everyMinutes(1).create();
  console.log('Installed: syncTick now runs every minute.');
}

/** Removes the sync trigger(s). Sync state is kept, so installTrigger resumes where it stopped. */
function removeTrigger() {
  var n = 0;
  ScriptApp.getProjectTriggers().forEach(function (t) {
    if (t.getHandlerFunction() === 'syncTick') {
      ScriptApp.deleteTrigger(t);
      n++;
    }
  });
  console.log('Removed ' + n + ' syncTick trigger(s).');
}

/** Logs the local sync state (no secrets). */
function showStatus() {
  var all = PropertiesService.getScriptProperties().getProperties();
  var st = parseJSON_(all[PROP.STATE], {});
  var queue = parseJSON_(all[PROP.QUEUE], []);
  var backoff = parseJSON_(all[PROP.BACKOFF], null);
  var journal = Object.keys(all).filter(function (k) { return k.indexOf(PROP.JOURNAL) === 0; });
  var triggers = ScriptApp.getProjectTriggers().filter(function (t) { return t.getHandlerFunction() === 'syncTick'; });
  console.log([
    'version ' + VERSION,
    'BACKEND_URL ' + (all.BACKEND_URL || '(not set)') + ', SYNC_SECRET ' + (all.SYNC_SECRET ? 'set' : '(not set)'),
    'triggers: ' + triggers.length,
    'historyId: ' + (st.historyId || '(not started)'),
    'last successful run: ' + (st.lastOkAt ? new Date(st.lastOkAt).toISOString() : 'never'),
    'scan: ' + (st.sweep ? st.sweep.kind + ', ' + st.sweep.scanned + ' checked' : 'none'),
    'queued messages: ' + queue.length,
    'unreported actions: ' + journal.length,
    'backoff: ' + (backoff && backoff.until > Date.now() ? 'until ' + new Date(backoff.until).toISOString() : 'none')
  ].join('\n'));
}

/** Imports older mail (BACKFILL_DAYS, default 365) gradually over the next trigger runs. */
function startBackfill() {
  var lock = LockService.getScriptLock();
  if (!lock.tryLock(30000)) throw new Error('A sync run is in progress; try again in a minute.');
  try {
    var run = newRun_(true);
    if (!run.state.historyId) throw new Error('Run runFullSyncNow or install the trigger first.');
    run.state.sweep = newSweep_(run.cfg.BACKFILL_DAYS, 'backfill');
    persist_(run);
    console.log('Backfill of the last ' + run.cfg.BACKFILL_DAYS + ' days scheduled; it continues with each trigger run.');
  } finally {
    lock.releaseLock();
  }
}

/** Forgets the local sync position. The next run re-scans INITIAL_SYNC_DAYS; the backend skips what it has. */
function resetSyncState() {
  var lock = LockService.getScriptLock();
  if (!lock.tryLock(30000)) throw new Error('A sync run is in progress; try again in a minute.');
  try {
    var props = PropertiesService.getScriptProperties();
    [PROP.STATE, PROP.QUEUE, PROP.BACKOFF].forEach(function (k) { props.deleteProperty(k); });
    console.log('Sync state cleared (unreported action results were kept).');
  } finally {
    lock.releaseLock();
  }
}

/** Config for read-only helpers that should work before the backend is set up. */
function readConfigLenient_() {
  var cfg = {};
  Object.keys(DEFAULTS).forEach(function (k) { cfg[k] = DEFAULTS[k]; });
  return cfg;
}
