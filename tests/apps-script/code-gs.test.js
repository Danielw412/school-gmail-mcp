'use strict';
// Exercises apps-script/Code.gs against a real backend.
//
// Local mode (default): starts backend/bin/school-email-sync on a free loopback
// port with a throwaway database.
//   node --test tests/apps-script/code-gs.test.js
//
// End-to-end mode: Code.gs talks to the public Cloudflare hostname; assertions
// use the local client API. Only run this against a fresh, empty deployment:
// it writes synthetic messages and actions (see README, "Deployment smoke test").
//   SES_E2E_URL=https://host SES_SYNC_SECRET=... SES_API_SECRET=... node --test tests/apps-script/code-gs.test.js

const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const { spawn } = require('child_process');
const crypto = require('crypto');
const fs = require('fs');
const net = require('net');
const os = require('os');
const path = require('path');
const { createRuntime } = require('./gas-env');

const REPO = path.join(__dirname, '..', '..');
const E2E_URL = process.env.SES_E2E_URL || '';
const FAKE_URL = 'https://backend.test';
const DAY = 86400000;

let localBase;
let syncSecret;
let apiSecret;
let backend;
let tmpDir;
let rt;
let baseline = 0;
const ids = {};

function freePort() {
  return new Promise((resolve, reject) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
    s.on('error', reject);
  });
}

async function waitHealthy(timeoutMs = 10000) {
  const until = Date.now() + timeoutMs;
  while (Date.now() < until) {
    try {
      const r = await fetch(localBase + '/healthz');
      if (r.ok) return;
    } catch (e) { /* not up yet */ }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error('backend did not become healthy');
}

async function startBackend() {
  const bin = process.env.SES_BACKEND_BIN || path.join(REPO, 'backend', 'bin', 'school-email-sync');
  backend = spawn(bin, ['serve'], {
    env: {
      PATH: process.env.PATH, LISTEN_ADDR: new URL(localBase).host, DB_PATH: path.join(tmpDir, 'mail.db'),
      SYNC_SECRET: syncSecret, API_SECRET: apiSecret, LOG_LEVEL: process.env.VERBOSE ? 'debug' : 'error',
    },
    stdio: ['ignore', 'inherit', 'inherit'],
  });
  await waitHealthy();
}

function stopBackend() {
  return new Promise((resolve) => {
    if (!backend || backend.exitCode !== null) return resolve();
    backend.once('exit', () => resolve());
    backend.kill('SIGTERM');
  });
}

/** Client API (local only), used for assertions and for queueing actions. */
async function api(method, p, body) {
  const r = await fetch(localBase + p, {
    method,
    headers: Object.assign({ Authorization: 'Bearer ' + apiSecret }, body ? { 'Content-Type': 'application/json' } : {}),
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await r.text();
  return { status: r.status, body: text ? JSON.parse(text) : {} };
}

async function stats() {
  const r = await api('GET', '/v1/stats');
  assert.equal(r.status, 200);
  return r.body;
}

async function message(id) {
  return api('GET', '/v1/messages/' + id);
}

async function action(id) {
  const r = await api('GET', '/v1/actions/' + id);
  assert.equal(r.status, 200, JSON.stringify(r.body));
  return r.body.action;
}

async function queueAction(body) {
  const r = await api('POST', '/v1/actions', body);
  assert.equal(r.status, 201, JSON.stringify(r.body));
  return r.body.action.id;
}

function journalKeys() {
  return [...rt.props.keys()].filter((k) => k.startsWith('SES_J_'));
}

function uploads() {
  return rt.requests.filter((r) => r.path === '/v1/sync/messages').length;
}

before(async () => {
  if (E2E_URL) {
    localBase = process.env.SES_LOCAL_URL || 'http://127.0.0.1:8766';
    syncSecret = process.env.SES_SYNC_SECRET;
    apiSecret = process.env.SES_API_SECRET;
    assert.ok(syncSecret && apiSecret, 'SES_SYNC_SECRET and SES_API_SECRET are required in E2E mode');
  } else {
    tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'ses-gas-test-'));
    localBase = 'http://127.0.0.1:' + (await freePort());
    syncSecret = 'sync-' + crypto.randomBytes(24).toString('hex');
    apiSecret = 'api-' + crypto.randomBytes(24).toString('hex');
    await startBackend();
  }
  rt = createRuntime({ rewriteUrl: (u) => (E2E_URL ? u : u.replace(FAKE_URL, localBase)) });
  baseline = (await stats()).messages;
});

after(async () => {
  if (!E2E_URL) {
    await stopBackend();
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test('trigger logs configuration problems instead of failing; manual helpers throw', () => {
  rt.call('syncTick');
  assert.match(rt.logText(), /BACKEND_URL is not set/);
  assert.throws(() => rt.call('runFullSyncNow'), /BACKEND_URL/);
  assert.throws(() => rt.call('installTrigger'), /BACKEND_URL/);
  assert.equal(rt.triggers.length, 0);

  rt.props.set('BACKEND_URL', 'http://insecure.example.org');
  rt.props.set('SYNC_SECRET', syncSecret);
  rt.clearLogs();
  rt.call('syncTick');
  assert.match(rt.logText(), /must be https/);
  rt.props.set('BACKEND_URL', (E2E_URL || FAKE_URL) + '/');
});

test('testBackendConnectivity checks health, auth and API', () => {
  rt.clearLogs();
  rt.call('testBackendConnectivity');
  assert.match(rt.logText(), /healthz -> HTTP 200/);
  assert.match(rt.logText(), /Backend connectivity OK/);

  rt.props.set('SYNC_SECRET', 'w'.repeat(40));
  assert.throws(() => rt.call('testBackendConnectivity'), /HTTP 401/);
  rt.props.set('SYNC_SECRET', syncSecret);
});

test('first run imports recent mail with bodies, charsets and attachment metadata', async () => {
  const box = rt.mailbox;
  const now = Date.now();
  ids.plain = box.add({
    subject: 'Field trip permission form', text: 'Please return the form by Friday.',
    html: '<p>Please return the form by <b>Friday</b>.</p>', cc: 'parent@example.com',
    labelIds: ['INBOX', 'UNREAD', 'Label_1'], date: now - 2 * DAY,
  });
  ids.latin1 = box.add({ subject: 'Cafeteria menu', text: 'Café au lait', charset: 'iso-8859-1', form: 'bytes', date: now - 3 * DAY });
  ids.htmlOnly = box.add({
    subject: 'Newsletter', html: '<html><body><h1>Robotics</h1><p>Team wins regional</p></body></html>',
    form: 'bytes', date: now - 4 * DAY,
  });
  ids.att = box.add({
    subject: 'Worksheet', text: 'See attached.', date: now - 5 * DAY,
    attachments: [
      { filename: 'worksheet.pdf', mimeType: 'application/pdf', size: 48213 },
      { filename: 'logo.png', mimeType: 'image/png', size: 2048, inline: true, contentId: 'logo@x' },
    ],
  });
  ids.big = box.add({ subject: 'Long text', text: 'Lorem ipsum '.repeat(30000), outOfLine: true, date: now - 6 * DAY });
  ids.old = box.add({ subject: 'Old news', text: 'old', date: now - 40 * DAY });
  ids.spam = box.add({ subject: 'Win a prize', text: 'spam', labelIds: ['SPAM'], date: now - DAY });
  ids.draft = box.add({ subject: 'Unsent', text: 'draft', labelIds: ['DRAFT'], date: now - DAY });
  ids.fillers = [];
  for (let i = 0; i < 25; i++) {
    const thread = i % 5 === 0 ? undefined : ids.fillers[i - (i % 5)];
    ids.fillers.push(box.add({ subject: 'Thread ' + Math.floor(i / 5), text: 'Message ' + i, threadId: thread, date: now - (30 - i) * 3600000 }));
  }

  rt.clearLogs();
  rt.call('testGmailRead');
  assert.match(rt.logText(), /Gmail API OK: student@school\.example\.org/);
  rt.call('testGmailSend');
  assert.equal(box.sent.length, 1);
  assert.equal(box.sent[0].to, box.me);

  rt.call('runFullSyncNow');
  const s = await stats();
  assert.equal(s.messages - baseline, 31, rt.logText()); // 5 special + 25 fillers + the send test
  assert.ok(uploads() >= 2, 'expected the import to be split into several batches');
  assert.equal(s.labels, 3);

  let m = (await message(ids.plain)).body.message;
  assert.equal(m.cc, 'parent@example.com');
  assert.equal(m.unread, true);
  assert.deepEqual(m.labels.sort(), ['INBOX', 'Robotics', 'UNREAD']);
  assert.equal(m.bodyText, 'Please return the form by Friday.');
  assert.equal(m.hasHtml, true);

  assert.equal((await message(ids.latin1)).body.message.bodyText, 'Café au lait');
  m = (await message(ids.htmlOnly)).body.message;
  assert.equal(m.bodyTextDerived, true);
  assert.match(m.bodyText, /Team wins regional/);

  m = (await message(ids.att)).body.message;
  assert.equal(m.attachments.length, 2);
  const pdf = m.attachments.find((a) => a.filename === 'worksheet.pdf');
  const logo = m.attachments.find((a) => a.filename === 'logo.png');
  assert.equal(pdf.size, 48213);
  assert.equal(pdf.mimeType, 'application/pdf');
  assert.ok(pdf.attachmentId.startsWith('ANGjdJ_'));
  assert.equal(pdf.inline, false);
  assert.equal(logo.inline, true);
  assert.equal(logo.contentId, 'logo@x');

  m = (await message(ids.big)).body.message;
  assert.equal(m.bodyTextTruncated, true);
  assert.equal(m.bodyText.length, 200000);

  for (const id of [ids.old, ids.spam, ids.draft]) {
    assert.equal((await message(id)).status, 404, 'should not be synced: ' + id);
  }
  const st = JSON.parse(rt.props.get('SES_STATE'));
  assert.equal(st.sweep, null);
  assert.ok(st.historyId);
});

test('an idle run uploads nothing and reads no message bodies', async () => {
  const before = (await stats()).messages;
  const gets = rt.mailbox.calls.get;
  rt.requests.length = 0;
  rt.call('syncTick');
  assert.equal(uploads(), 0);
  assert.equal(rt.mailbox.calls.get, gets);
  assert.equal((await stats()).messages, before);
  assert.deepEqual(rt.requests.map((r) => r.path), ['/v1/actions/poll']);
});

test('incremental sync picks up new mail, read state and deletions via history (paged)', async () => {
  const box = rt.mailbox;
  box.historyPageSize = 2;
  const before = (await stats()).messages;
  ids.fresh = box.add({ subject: 'New assignment posted', text: 'Essay due Monday' });
  box.setLabels(ids.plain, ['INBOX', 'Label_1']);
  box.remove(ids.fillers[0]);
  const spam = box.add({ subject: 'spam', labelIds: ['SPAM'] });
  const draft = box.add({ subject: 'draft', labelIds: ['DRAFT'] });

  rt.call('syncTick');
  box.historyPageSize = 500;
  assert.equal((await stats()).messages, before + 1);
  assert.equal((await message(ids.fresh)).status, 200);
  assert.equal((await message(ids.plain)).body.message.unread, false);
  assert.equal((await message(ids.fillers[0])).body.message.deletedInGmail, true);
  assert.equal((await message(spam)).status, 404);
  assert.equal((await message(draft)).status, 404);
  const search = await api('GET', '/v1/messages?q=essay');
  assert.deepEqual(search.body.messages.map((x) => x.id), [ids.fresh]);
});

test('re-scanning after a state reset stores no duplicates', async () => {
  const before = (await stats()).messages;
  rt.call('resetSyncState');
  const runStats = rt.call('runSync_', true);
  assert.equal(runStats.inserted, 0);
  assert.equal(runStats.fetched, 0, 'known messages must not be re-fetched');
  assert.equal((await stats()).messages, before);
});

test('expired Gmail history triggers a recovery scan', async () => {
  const box = rt.mailbox;
  const before = (await stats()).messages;
  box.minHistoryId = box.historyId + 1;
  ids.afterExpiry = box.add({ subject: 'Arrived while history expired', text: 'x' });
  rt.clearLogs();
  rt.call('syncTick');
  box.minHistoryId = 0;
  assert.match(rt.logText(), /history .*expired/);
  assert.equal((await stats()).messages, before + 1);
  assert.equal((await message(ids.afterExpiry)).status, 200);
});

test('backend offline: no state is lost, the script backs off, then catches up', async () => {
  const before = (await stats()).messages;
  const stateBefore = rt.props.get('SES_STATE');
  if (E2E_URL) rt.fetchFailures.push(() => true);
  else await stopBackend();

  ids.offline = rt.mailbox.add({ subject: 'Sent while the server was down', text: 'x' });
  rt.clearLogs();
  rt.call('syncTick'); // must not throw
  assert.match(rt.logText(), /backend unreachable/);
  assert.ok(rt.props.get('SES_BACKOFF'));
  assert.equal(rt.props.get('SES_STATE'), stateBefore, 'history position must not advance');

  rt.requests.length = 0;
  rt.call('syncTick');
  assert.equal(rt.requests.length, 0, 'should skip runs while backing off');

  if (E2E_URL) rt.fetchFailures.length = 0;
  else await startBackend();
  rt.call('runFullSyncNow');
  assert.ok(!rt.props.has('SES_BACKOFF'), 'backoff should be cleared after a successful run');
  assert.equal((await stats()).messages, before + 1);
  assert.equal((await message(ids.offline)).status, 200);
});

test('send and reply actions are performed in Gmail, threaded, and reported', async () => {
  const box = rt.mailbox;
  const plainThread = box.msgs.get(ids.plain).threadId;
  const fillerThread = box.msgs.get(ids.fillers[6]).threadId;
  const sendId = await queueAction({
    type: 'send', to: ['Ms. Rivera <rivera@school.example.org>'], subject: 'Signed form',
    body: 'Attached is the signed form.', htmlBody: '<p>Attached is the signed form.</p>',
  });
  const replyId = await queueAction({
    type: 'reply', messageId: ids.plain, body: 'Thank you!', replyAll: true, cc: ['counselor@school.example.org'],
  });
  const threadReplyId = await queueAction({ type: 'reply', threadId: fillerThread, body: 'Sounds good.' });

  const sentBefore = box.sent.length;
  rt.call('syncTick');
  assert.equal(box.sent.length, sentBefore + 3);
  const [send, reply, threadReply] = box.sent.slice(sentBefore);
  assert.equal(send.to, 'Ms. Rivera <rivera@school.example.org>');
  assert.equal(send.subject, 'Signed form');
  assert.equal(send.opts.htmlBody, '<p>Attached is the signed form.</p>');
  assert.equal(reply.threadId, plainThread);
  assert.equal(reply.replyAll, true);
  assert.equal(reply.opts.cc, 'counselor@school.example.org');
  assert.equal(threadReply.threadId, fillerThread);
  assert.match(threadReply.to, /rivera@/, 'thread replies go to the other party');

  for (const [actionId, sent] of [[sendId, send], [replyId, reply], [threadReplyId, threadReply]]) {
    const a = await action(actionId);
    assert.equal(a.status, 'done');
    assert.equal(a.result.messageId, sent.id);
    assert.equal(a.result.threadId, sent.threadId);
  }
  assert.deepEqual(journalKeys(), []);

  rt.call('syncTick'); // the sent messages arrive through Gmail history
  const thread = await api('GET', '/v1/threads/' + plainThread);
  assert.ok(thread.body.messages.some((m) => m.id === reply.id), 'reply should be stored in the original thread');
});

test('Gmail send failures are reported as permanent or retryable', async () => {
  const box = rt.mailbox;
  const bad = await queueAction({ type: 'send', to: ['nobody@example.org'], subject: 'x', body: 'y' });
  box.sendFailure = 'Invalid email: nobody@example.org';
  rt.call('syncTick');
  let a = await action(bad);
  assert.equal(a.status, 'failed');
  assert.match(a.lastError, /Invalid email/);

  const busy = await queueAction({ type: 'send', to: ['teacher@example.org'], subject: 'x', body: 'y' });
  box.sendFailure = 'Service invoked too many times for one day: email.';
  rt.call('syncTick');
  a = await action(busy);
  assert.equal(a.status, 'pending');
  assert.equal(a.attempts, 1);
  assert.match(a.lastError, /too many times/);
  assert.equal((await api('POST', '/v1/actions/' + busy + '/cancel')).status, 200);
  assert.deepEqual(journalKeys(), []);
});

test('a lost completion report is retried later without sending twice', async () => {
  const box = rt.mailbox;
  const id = await queueAction({ type: 'send', to: ['rivera@school.example.org'], subject: 'Once', body: 'only once' });
  const sentBefore = box.sent.length;
  rt.fetchFailures.push((url) => url.includes('/complete'));
  rt.call('syncTick');
  assert.equal(box.sent.length, sentBefore + 1);
  assert.equal((await action(id)).status, 'claimed');
  assert.equal(journalKeys().length, 1);
  assert.match(rt.props.get(journalKeys()[0]), /"s":"sent"/);

  rt.fetchFailures.length = 0;
  rt.props.delete('SES_BACKOFF');
  rt.call('syncTick');
  const a = await action(id);
  assert.equal(a.status, 'done');
  assert.equal(box.sent.length, sentBefore + 1, 'must not send again');
  assert.deepEqual(journalKeys(), []);
});

test('a run that dies mid-send is reported as uncertain, never re-sent', async () => {
  const box = rt.mailbox;
  const id = await queueAction({ type: 'send', to: ['rivera@school.example.org'], subject: 'Crash', body: 'x' });
  const sentBefore = box.sent.length;
  rt.killOnProperty = (k, v) => k.startsWith('SES_J_') && v.includes('"s":"sent"');
  assert.throws(() => rt.call('syncTick'), /Simulated execution kill/);
  assert.equal(box.sent.length, sentBefore + 1);
  assert.match(rt.props.get(journalKeys()[0]), /"s":"attempting"/);

  rt.call('syncTick');
  const a = await action(id);
  assert.equal(a.status, 'failed');
  assert.match(a.lastError, /outcome unknown/);
  assert.equal(box.sent.length, sentBefore + 1);
  assert.deepEqual(journalKeys(), []);
});

test('installTrigger is idempotent and removeTrigger clears it', () => {
  rt.call('installTrigger');
  rt.call('installTrigger');
  assert.equal(rt.triggers.length, 1);
  assert.equal(rt.triggers[0].fn, 'syncTick');
  assert.equal(rt.triggers[0].minutes, 1);
  rt.call('removeTrigger');
  assert.equal(rt.triggers.length, 0);
});

test('a rejected secret makes the trigger back off for 15 minutes without throwing', () => {
  rt.props.set('SYNC_SECRET', 'z'.repeat(40));
  rt.props.delete('SES_BACKOFF');
  rt.clearLogs();
  rt.call('syncTick');
  assert.match(rt.logText(), /backend refused the request \(HTTP 401/);
  const b = JSON.parse(rt.props.get('SES_BACKOFF'));
  assert.ok(b.until - Date.now() > 14 * 60000);
  rt.props.set('SYNC_SECRET', syncSecret);
  rt.props.delete('SES_BACKOFF');
});

test('showStatus reports state without revealing the secret', () => {
  rt.clearLogs();
  rt.call('showStatus');
  const out = rt.logText();
  assert.match(out, /SYNC_SECRET set/);
  assert.match(out, /historyId: \d+/);
  assert.ok(!out.includes(syncSecret));
  assert.ok(!rt.logText().includes('Please return the form'), 'message bodies must not be logged');
});
