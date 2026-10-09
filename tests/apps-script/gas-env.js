'use strict';
// Minimal Google Apps Script runtime for testing apps-script/Code.gs under Node.
// Code.gs is loaded unmodified into a VM context with fakes for the services it
// uses. UrlFetchApp performs real (synchronous) HTTP requests so the script talks
// to a real backend; the Gmail fakes keep an in-memory mailbox with history.

const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const vm = require('vm');
const { spawnSync } = require('child_process');

const CODE_PATH = path.join(__dirname, '..', '..', 'apps-script', 'Code.gs');

// --- UrlFetchApp ------------------------------------------------------------
// Synchronous HTTP via a child Node process; secrets travel over stdin, never argv.
// SES_DNS_SERVER=1.1.1.1 resolves hostnames with that server instead of the system
// resolver (useful while a local resolver still caches NXDOMAIN for a new record).
const CHILD_FETCH = `
const fs = require('fs'), dns = require('dns'), http = require('http'), https = require('https');
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const done = (o) => process.stdout.write(JSON.stringify(o));
const u = new URL(input.url);
const opts = { method: input.method, headers: input.headers, timeout: 30000 };
if (process.env.SES_DNS_SERVER) {
  const r = new dns.Resolver();
  r.setServers([process.env.SES_DNS_SERVER]);
  opts.lookup = (host, o, cb) => r.resolve4(host, (e, a) => (o && o.all)
    ? cb(e, (a || []).map((x) => ({ address: x, family: 4 })))
    : cb(e, a && a[0], 4));
}
const req = (u.protocol === 'https:' ? https : http).request(u, opts, (res) => {
  let body = '';
  res.setEncoding('utf8');
  res.on('data', (c) => { body += c; });
  res.on('end', () => done({ status: res.statusCode, body }));
});
req.on('timeout', () => req.destroy(new Error('timeout')));
req.on('error', (e) => done({ error: String(e.code || e.message) }));
if (input.body !== undefined) req.write(input.body);
req.end();
`;

function syncFetch(url, params) {
  const headers = Object.assign({}, params.headers || {});
  if (params.contentType) headers['Content-Type'] = params.contentType;
  const input = JSON.stringify({
    url, method: String(params.method || 'get').toUpperCase(), headers,
    body: params.payload === undefined ? undefined : String(params.payload),
  });
  const r = spawnSync(process.execPath, ['-e', CHILD_FETCH], { input, maxBuffer: 64 << 20, encoding: 'utf8' });
  const out = JSON.parse(r.stdout || '{"error":"no output"}');
  if (out.error) throw new Error('Exception: Address unavailable: ' + url + ' (' + out.error + ')');
  return { getResponseCode: () => out.status, getContentText: () => out.body };
}

// --- Fake Gmail mailbox -----------------------------------------------------

function toSignedBytes(buf) {
  return Array.from(buf, (b) => (b > 127 ? b - 256 : b));
}

class FakeMailbox {
  constructor(me) {
    this.me = me;
    this.msgs = new Map();
    this.history = [];
    this.historyId = 5000;
    this.minHistoryId = 0;
    this.historyPageSize = 500;
    this.seq = 0;
    this.attData = {};
    this.sent = [];
    this.sendFailure = null;
    this.calls = { get: 0, list: 0, history: 0, attachmentGet: 0, getOpts: [] };
  }

  newId() {
    this.seq += 1;
    return (0x19a000000000 + this.seq * 7919).toString(16);
  }

  /** Encodes body text the way the Gmail API does; alternates string/byte-array forms. */
  encode(text, charset, form) {
    const buf = Buffer.from(text, charset === 'iso-8859-1' ? 'latin1' : 'utf8');
    return form === 'bytes' ? toSignedBytes(buf) : buf.toString('base64url');
  }

  textPart(partId, mime, text, opts = {}) {
    const charset = opts.charset || 'utf-8';
    const part = {
      partId, mimeType: mime, filename: '',
      headers: [{ name: 'Content-Type', value: `${mime}; charset="${charset}"` }],
      body: { size: Buffer.byteLength(text) },
    };
    if (opts.outOfLine) {
      const aid = 'ANGjdJ_text_' + this.newId();
      this.attData[aid] = this.encode(text, charset, opts.form);
      part.body.attachmentId = aid;
    } else {
      part.body.data = this.encode(text, charset, opts.form);
    }
    return part;
  }

  add(spec = {}) {
    const id = spec.id || this.newId();
    const threadId = spec.threadId || id;
    const labelIds = spec.labelIds || ['INBOX', 'UNREAD'];
    const date = spec.date || Date.now();
    const headers = [
      { name: 'From', value: spec.from || 'Ms. Rivera <rivera@school.example.org>' },
      { name: 'To', value: spec.to || this.me },
      { name: 'Subject', value: spec.subject || 'Hello ' + id },
      { name: 'Date', value: new Date(date).toUTCString() },
      { name: 'Message-ID', value: `<${id}@mail.example.org>` },
    ];
    if (spec.cc) headers.push({ name: 'Cc', value: spec.cc });
    if (spec.inReplyTo) headers.push({ name: 'In-Reply-To', value: spec.inReplyTo });

    const alt = [];
    if (spec.text !== undefined) alt.push(this.textPart('0.0', 'text/plain', spec.text, spec));
    if (spec.html !== undefined) alt.push(this.textPart('0.1', 'text/html', spec.html, { form: spec.form }));
    const parts = spec.parts || [{ partId: '0', mimeType: 'multipart/alternative', filename: '', headers: [], body: { size: 0 }, parts: alt }];
    (spec.attachments || []).forEach((a, i) => parts.push(this.attachmentPart(String(i + 1), a)));
    const payload = { partId: '', mimeType: 'multipart/mixed', filename: '', headers, body: { size: 0 }, parts };

    this.historyId += 1;
    const msg = {
      id, threadId, labelIds: labelIds.slice(), snippet: (spec.text || spec.subject || '').slice(0, 100),
      historyId: String(this.historyId), internalDate: String(date), sizeEstimate: 1234, payload,
    };
    this.msgs.set(id, msg);
    this.history.push({ id: String(this.historyId), messagesAdded: [{ message: { id, threadId, labelIds: labelIds.slice() } }] });
    return id;
  }

  /**
   * An attachment part. With a.data (Buffer) the content can be downloaded; it is
   * kept out of line (attachmentId) unless a.inData, and encoded as base64url or as
   * a byte array (a.form), as the Gmail advanced service may return either.
   */
  attachmentPart(partId, a) {
    const h = [{ name: 'Content-Type', value: a.mimeType }];
    h.push({ name: 'Content-Disposition', value: `${a.inline ? 'inline' : 'attachment'}; filename="${a.filename}"` });
    if (a.contentId) h.push({ name: 'Content-ID', value: `<${a.contentId}>` });
    const size = a.data ? a.data.length : a.size;
    const part = { partId, mimeType: a.mimeType, filename: a.filename, headers: h, body: { size } };
    const encoded = a.data && (a.form === 'bytes' ? toSignedBytes(a.data) : a.data.toString('base64url'));
    if (a.inData) {
      part.body.data = encoded;
    } else {
      part.body.attachmentId = 'ANGjdJ_' + this.newId();
      if (a.data) this.attData[part.body.attachmentId] = encoded;
    }
    return part;
  }

  hasAttachment(m) {
    const walk = (p) => !!p && (!!p.filename || (p.parts || []).some(walk));
    return walk(m.payload);
  }

  setLabels(id, labelIds) {
    const m = this.msgs.get(id);
    m.labelIds = labelIds.slice();
    this.historyId += 1;
    this.history.push({
      id: String(this.historyId),
      labelsRemoved: [{ message: { id, threadId: m.threadId, labelIds: labelIds.slice() }, labelIds: ['UNREAD'] }],
    });
  }

  remove(id) {
    const m = this.msgs.get(id);
    this.msgs.delete(id);
    this.historyId += 1;
    this.history.push({ id: String(this.historyId), messagesDeleted: [{ message: { id, threadId: m.threadId } }] });
  }

  header(m, name) {
    const h = m.payload.headers.find((x) => x.name.toLowerCase() === name.toLowerCase());
    return h ? h.value : '';
  }

  notFound(what) {
    return new Error(`API call to gmail.users.${what} failed with error: Requested entity was not found.`);
  }

  advancedService() {
    const box = this;
    return {
      Users: {
        getProfile: () => ({
          emailAddress: box.me, messagesTotal: box.msgs.size, threadsTotal: box.msgs.size, historyId: String(box.historyId),
        }),
        History: {
          list: (user, opts) => {
            box.calls.history += 1;
            const start = Number(opts.startHistoryId);
            if (start < box.minHistoryId) throw box.notFound('history.list');
            const recs = box.history.filter((h) => Number(h.id) > start);
            const size = Math.min(opts.maxResults || 100, box.historyPageSize);
            const offset = opts.pageToken ? Number(opts.pageToken) : 0;
            const res = { historyId: String(box.historyId) };
            const page = recs.slice(offset, offset + size);
            if (page.length) res.history = JSON.parse(JSON.stringify(page));
            if (offset + size < recs.length) res.nextPageToken = String(offset + size);
            return res;
          },
        },
        Messages: {
          get: (user, id, opts) => {
            box.calls.get += 1;
            box.calls.getOpts.push(Object.assign({}, opts || {}));
            const m = box.msgs.get(id);
            if (!m) throw box.notFound('messages.get');
            const copy = JSON.parse(JSON.stringify(m));
            if (opts && opts.fields) {
              // Partial response: the MIME tree without body data.
              const strip = (p) => { if (p.body) delete p.body.data; (p.parts || []).forEach(strip); };
              strip(copy.payload);
            }
            return copy;
          },
          list: (user, opts) => {
            box.calls.list += 1;
            const after = Number((/after:(\d+)/.exec(opts.q || '') || [0, 0])[1]) * 1000;
            const noDrafts = /-in:drafts/.test(opts.q || '');
            const withAtt = /has:attachment/.test(opts.q || '');
            const all = [...box.msgs.values()]
              .filter((m) => Number(m.internalDate) >= after)
              .filter((m) => !withAtt || box.hasAttachment(m))
              .filter((m) => opts.includeSpamTrash || !m.labelIds.some((l) => l === 'SPAM' || l === 'TRASH'))
              .filter((m) => !noDrafts || !m.labelIds.includes('DRAFT'))
              .sort((a, b) => Number(b.internalDate) - Number(a.internalDate));
            const offset = opts.pageToken ? Number(opts.pageToken) : 0;
            const size = opts.maxResults || 100;
            const res = { resultSizeEstimate: all.length };
            const page = all.slice(offset, offset + size);
            if (page.length) res.messages = page.map((m) => ({ id: m.id, threadId: m.threadId }));
            if (offset + size < all.length) res.nextPageToken = String(offset + size);
            return res;
          },
          Attachments: {
            get: (user, messageId, attachmentId) => {
              if (!attachmentId.startsWith('ANGjdJ_text_')) box.calls.attachmentGet += 1;
              if (!(attachmentId in box.attData)) throw box.notFound('messages.attachments.get');
              return { size: 1, data: box.attData[attachmentId] };
            },
          },
        },
        Labels: {
          list: () => ({
            labels: [
              { id: 'INBOX', name: 'INBOX', type: 'system' },
              { id: 'UNREAD', name: 'UNREAD', type: 'system' },
              { id: 'Label_1', name: 'Robotics', type: 'user' },
            ],
          }),
        },
      },
    };
  }

  gmailApp() {
    const box = this;
    const wrap = (m) => ({
      getId: () => m.id,
      getThread: () => ({ getId: () => m.threadId }),
      isDraft: () => m.labelIds.includes('DRAFT'),
      getFrom: () => box.header(m, 'From'),
      createDraftReply: (body, opts) => box.draft({ replyTo: m, all: false, body, opts }),
      createDraftReplyAll: (body, opts) => box.draft({ replyTo: m, all: true, body, opts }),
    });
    return {
      createDraft: (to, subject, body, opts) => box.draft({ to, subject, body, opts }),
      getMessageById: (id) => (box.msgs.has(id) ? wrap(box.msgs.get(id)) : null),
      getThreadById: (tid) => {
        const ms = [...box.msgs.values()].filter((m) => m.threadId === tid)
          .sort((a, b) => Number(a.internalDate) - Number(b.internalDate));
        return ms.length ? { getMessages: () => ms.map(wrap) } : null;
      },
      getInboxUnreadCount: () => [...box.msgs.values()].filter((m) => m.labelIds.includes('UNREAD')).length,
      _wrap: wrap,
    };
  }

  draft(spec) {
    const box = this;
    return {
      send() {
        if (box.sendFailure) {
          const f = box.sendFailure;
          box.sendFailure = null;
          throw new Error(f);
        }
        const r = spec.replyTo;
        const to = r ? box.header(r, 'From') : spec.to;
        const subject = r ? 'Re: ' + box.header(r, 'Subject') : spec.subject;
        const opts = spec.opts || {};
        const id = box.add({
          threadId: r ? r.threadId : undefined, from: box.me, to, subject, cc: opts.cc,
          text: spec.body, html: opts.htmlBody, labelIds: ['SENT'], inReplyTo: r ? box.header(r, 'Message-ID') : undefined,
        });
        box.sent.push({ id, threadId: box.msgs.get(id).threadId, to, subject, body: spec.body, opts, replyAll: !!spec.all });
        return box.gmailApp()._wrap(box.msgs.get(id));
      },
    };
  }
}

// --- Script runtime ---------------------------------------------------------

class SimulatedKill extends Error {}

function createRuntime({ me = 'student@school.example.org', rewriteUrl = (u) => u } = {}) {
  const mailbox = new FakeMailbox(me);
  const props = new Map();
  const triggers = [];
  const logs = [];
  const rt = {
    mailbox, props, triggers, logs,
    requests: [], // {method, path} of every UrlFetchApp call
    fetchFailures: [], // predicates (url, params) => bool that simulate network errors
    killOnProperty: null, // predicate (key, value) => bool that simulates the script dying
    verbose: !!process.env.VERBOSE,
  };

  const checkValue = (k, v) => {
    if (Buffer.byteLength(String(v)) > 9 * 1024) throw new Error(`Property ${k} exceeds the 9 KB value limit`);
    if (rt.killOnProperty && rt.killOnProperty(k, String(v))) {
      rt.killOnProperty = null;
      throw new SimulatedKill('Simulated execution kill');
    }
  };
  const store = {
    getProperty: (k) => (props.has(k) ? props.get(k) : null),
    getProperties: () => Object.fromEntries(props),
    setProperty: (k, v) => { checkValue(k, v); props.set(k, String(v)); return store; },
    setProperties: (obj) => {
      Object.entries(obj).forEach(([k, v]) => checkValue(k, v));
      Object.entries(obj).forEach(([k, v]) => props.set(k, String(v)));
      return store;
    },
    deleteProperty: (k) => { props.delete(k); return store; },
  };

  const log = (level) => (...args) => {
    const line = args.map(String).join(' ');
    logs.push({ level, line });
    if (rt.verbose) process.stderr.write(`    [gas ${level}] ${line}\n`);
  };

  const cache = new Map();
  const signed = (buf) => toSignedBytes(buf);
  const sandbox = {
    console: { log: log('log'), info: log('info'), warn: log('warn'), error: log('error') },
    PropertiesService: { getScriptProperties: () => store },
    CacheService: {
      getScriptCache: () => ({
        get: (k) => (cache.has(k) ? cache.get(k) : null),
        put: (k, v) => { cache.set(k, String(v)); },
      }),
    },
    ContentService: {
      MimeType: { JSON: 'JSON' },
      createTextOutput: (text) => {
        const out = { text, mimeType: 'TEXT', getContent: () => text, setMimeType: (m) => { out.mimeType = m; return out; } };
        return out;
      },
    },
    LockService: { getScriptLock: () => ({ tryLock: () => true, releaseLock: () => {} }) },
    UrlFetchApp: {
      fetch: (url, params) => {
        rt.requests.push({ method: String(params.method || 'get').toUpperCase(), path: new URL(url).pathname });
        if (rt.fetchFailures.some((f) => f(url, params))) throw new Error('Exception: Address unavailable: ' + url);
        return syncFetch(rewriteUrl(url), params);
      },
    },
    Utilities: {
      sleep: () => {},
      Charset: { UTF_8: 'UTF_8', US_ASCII: 'US_ASCII' },
      DigestAlgorithm: { SHA_256: 'sha256' },
      base64Decode: (s) => toSignedBytes(Buffer.from(s, 'base64')),
      base64Encode: (bytes) => Buffer.from(bytes.map((b) => b & 0xff)).toString('base64'),
      computeDigest: (alg, value) => signed(crypto.createHash(alg).update(String(value), 'utf8').digest()),
      computeHmacSha256Signature: (value, key) => signed(crypto.createHmac('sha256', String(key)).update(String(value), 'utf8').digest()),
      newBlob: (bytes) => ({
        getDataAsString: (charset) => new TextDecoder(String(charset || 'utf-8').toLowerCase(), { fatal: false })
          .decode(Buffer.from(bytes.map((b) => b & 0xff))),
      }),
    },
    ScriptApp: {
      newTrigger: (fn) => ({
        timeBased: () => ({
          everyMinutes: (n) => ({
            create: () => {
              const t = { fn, minutes: n, getHandlerFunction: () => fn };
              triggers.push(t);
              return t;
            },
          }),
        }),
      }),
      getProjectTriggers: () => triggers.slice(),
      deleteTrigger: (t) => { const i = triggers.indexOf(t); if (i >= 0) triggers.splice(i, 1); },
    },
    Gmail: mailbox.advancedService(),
    GmailApp: mailbox.gmailApp(),
  };
  const ctx = vm.createContext(sandbox);
  vm.runInContext(fs.readFileSync(CODE_PATH, 'utf8'), ctx, { filename: 'Code.gs' });
  rt.ctx = ctx;
  rt.call = (fn, ...args) => ctx[fn](...args);
  rt.logText = () => logs.map((l) => l.line).join('\n');
  rt.cache = cache;
  rt.clearLogs = () => { logs.length = 0; };
  return rt;
}

module.exports = { createRuntime, SimulatedKill };
