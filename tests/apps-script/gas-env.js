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

/** Bytes given to Apps Script APIs: a signed byte array, a Buffer or a string (UTF-8). */
function toBuffer(data) {
  if (Buffer.isBuffer(data)) return data;
  if (typeof data === 'string') return Buffer.from(data, 'utf8');
  return Buffer.from(data.map((b) => b & 0xff));
}

/** A Blob as Apps Script's Utilities.newBlob / GmailAttachment returns it. */
function makeBlob(data, contentType, name) {
  const buf = toBuffer(data);
  return {
    _buf: buf,
    getBytes: () => toSignedBytes(buf),
    getName: () => name,
    getContentType: () => contentType,
    getDataAsString: (charset) => new TextDecoder(String(charset || 'utf-8').toLowerCase(), { fatal: false }).decode(buf),
  };
}

/** RFC 2047 encoded words (UTF-8, B encoding) back to text. */
function decodeWords(s) {
  return String(s || '').replace(/\?=\s+=\?/g, '?==?')
    .replace(/=\?UTF-8\?B\?([^?]*)\?=/gi, (m, b) => Buffer.from(b, 'base64').toString('utf8'));
}

/** Parses the MIME text Code.gs builds for draft edits into headers, text, html and files. */
function parseMime(raw) {
  const split = raw.indexOf('\r\n\r\n');
  const head = split < 0 ? raw : raw.slice(0, split);
  const body = split < 0 ? '' : raw.slice(split + 4);
  const headers = {};
  head.replace(/\r\n[ \t]/g, ' ').split('\r\n').forEach((line) => {
    const i = line.indexOf(':');
    if (i > 0) headers[line.slice(0, i).toLowerCase()] = line.slice(i + 1).trim();
  });
  const type = headers['content-type'] || 'text/plain';
  const out = { headers, text: '', html: '', files: [] };
  const walk = (h, b) => {
    const ct = h['content-type'] || 'text/plain';
    if (/^multipart\//i.test(ct)) {
      const boundary = /boundary="([^"]+)"/.exec(ct)[1];
      b.split('--' + boundary).slice(1, -1).forEach((p) => {
        const part = p.replace(/^\r\n/, '').replace(/\r\n$/, '');
        const at = part.indexOf('\r\n\r\n');
        const ph = {};
        part.slice(0, at).split('\r\n').forEach((line) => {
          const i = line.indexOf(':');
          if (i > 0) ph[line.slice(0, i).toLowerCase()] = line.slice(i + 1).trim();
        });
        walk(ph, part.slice(at + 4));
      });
      return;
    }
    const data = /base64/i.test(h['content-transfer-encoding'] || '') ? Buffer.from(b.replace(/\s+/g, ''), 'base64') : Buffer.from(b);
    const disp = h['content-disposition'] || '';
    if (/^attachment/i.test(disp)) {
      const star = /filename\*=UTF-8''([^;]+)/i.exec(disp);
      const plain = /filename="([^"]*)"/i.exec(disp);
      out.files.push({ filename: star ? decodeURIComponent(star[1]) : (plain ? plain[1] : ''), mimeType: ct.split(';')[0].trim(), data });
    } else if (/^text\/html/i.test(ct)) {
      out.html = data.toString('utf8');
    } else {
      out.text = data.toString('utf8');
    }
  };
  walk({ 'content-type': type, 'content-transfer-encoding': headers['content-transfer-encoding'] }, body);
  return out;
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
    this.drafts = [];
    this.sendFailure = null;
    this.calls = { get: 0, list: 0, history: 0, attachmentGet: 0, getOpts: [], draftsList: 0, modify: [] };
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
      { name: 'To', value: spec.to !== undefined ? spec.to : this.me },
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

  /** Adds and removes labels the way Gmail does, with a history record. */
  modifyLabels(id, add = [], remove = []) {
    const m = this.msgs.get(id);
    const set = new Set(m.labelIds);
    remove.forEach((l) => set.delete(l));
    add.forEach((l) => set.add(l));
    m.labelIds = [...set];
    this.historyId += 1;
    this.history.push({ id: String(this.historyId), labelsAdded: [{ message: { id, threadId: m.threadId, labelIds: m.labelIds.slice() }, labelIds: add }] });
  }

  /** A draft created in Gmail by the user (not by the script). */
  addDraft(spec) {
    const id = this.add(Object.assign({ from: this.me, labelIds: ['DRAFT'] }, spec));
    const entry = { id, draftId: 'r-' + id, threadId: this.msgs.get(id).threadId, to: spec.to, subject: spec.subject, body: spec.text };
    this.drafts.push(entry);
    return entry;
  }

  /** Drafts currently in the Drafts folder. */
  liveDrafts() {
    return this.drafts.filter((d) => !d.removed && !d.sentAs && this.msgs.has(d.id));
  }

  findDraft(draftId, what) {
    const d = this.liveDrafts().find((x) => x.draftId === draftId);
    if (!d) throw this.notFound(what);
    return d;
  }

  /** The decoded text of the first part with this MIME type. */
  partText(m, mime) {
    let found = '';
    const walk = (p) => {
      if (found || !p) return;
      if (p.mimeType === mime && !p.filename) {
        const raw = p.body.data !== undefined ? p.body.data : this.attData[p.body.attachmentId];
        if (raw !== undefined) found = (typeof raw === 'string' ? Buffer.from(raw, 'base64url') : toBuffer(raw)).toString('utf8');
      }
      (p.parts || []).forEach(walk);
    };
    walk(m.payload);
    return found;
  }

  attachmentBlobs(m) {
    const out = [];
    const walk = (p) => {
      if (!p) return;
      if (p.filename) {
        const raw = p.body.data !== undefined ? p.body.data : this.attData[p.body.attachmentId];
        const buf = raw === undefined ? Buffer.alloc(0) : (typeof raw === 'string' ? Buffer.from(raw, 'base64url') : toBuffer(raw));
        out.push(makeBlob(buf, p.mimeType, p.filename));
      }
      (p.parts || []).forEach(walk);
    };
    walk(m.payload);
    return out;
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
          batchModify: (resource) => {
            box.calls.modify.push({ batch: resource.ids.slice() });
            const unknown = resource.ids.find((id) => !box.msgs.has(id));
            if (unknown) throw new Error('API call to gmail.users.messages.batchModify failed with error: Invalid id value');
            resource.ids.forEach((id) => box.modifyLabels(id, resource.addLabelIds, resource.removeLabelIds));
          },
          modify: (resource, user, id) => {
            box.calls.modify.push({ message: id });
            if (!box.msgs.has(id)) throw box.notFound('messages.modify');
            box.modifyLabels(id, resource.addLabelIds, resource.removeLabelIds);
            return { id, labelIds: box.msgs.get(id).labelIds };
          },
          trash: (user, id) => {
            if (!box.msgs.has(id)) throw box.notFound('messages.trash');
            box.modifyLabels(id, ['TRASH'], []);
          },
          untrash: (user, id) => {
            if (!box.msgs.has(id)) throw box.notFound('messages.untrash');
            box.modifyLabels(id, [], ['TRASH']);
          },
          Attachments: {
            get: (user, messageId, attachmentId) => {
              if (!attachmentId.startsWith('ANGjdJ_text_')) box.calls.attachmentGet += 1;
              if (!(attachmentId in box.attData)) throw box.notFound('messages.attachments.get');
              return { size: 1, data: box.attData[attachmentId] };
            },
          },
        },
        Threads: (() => {
          const inThread = (tid, what) => {
            const ms = [...box.msgs.values()].filter((m) => m.threadId === tid);
            if (!ms.length) throw box.notFound(what);
            return ms;
          };
          return {
            modify: (resource, user, tid) => {
              box.calls.modify.push({ thread: tid });
              inThread(tid, 'threads.modify').forEach((m) => box.modifyLabels(m.id, resource.addLabelIds, resource.removeLabelIds));
            },
            trash: (user, tid) => inThread(tid, 'threads.trash').forEach((m) => box.modifyLabels(m.id, ['TRASH'], [])),
            untrash: (user, tid) => inThread(tid, 'threads.untrash').forEach((m) => box.modifyLabels(m.id, [], ['TRASH'])),
          };
        })(),
        Drafts: {
          list: (user, opts) => {
            box.calls.draftsList += 1;
            const all = box.liveDrafts();
            const offset = opts && opts.pageToken ? Number(opts.pageToken) : 0;
            const size = (opts && opts.maxResults) || 100;
            const res = { drafts: all.slice(offset, offset + size).map((d) => ({ id: d.draftId, message: { id: d.id, threadId: d.threadId } })) };
            if (offset + size < all.length) res.nextPageToken = String(offset + size);
            return res;
          },
          get: (user, draftId, opts) => {
            const d = box.findDraft(draftId, 'drafts.get');
            const m = box.msgs.get(d.id);
            if (opts && opts.format === 'full') return { id: draftId, message: JSON.parse(JSON.stringify(m)) };
            return { id: draftId, message: { id: m.id, threadId: m.threadId, labelIds: m.labelIds.slice() } };
          },
          update: (resource, user, draftId) => {
            const d = box.findDraft(draftId, 'drafts.update');
            const raw = Buffer.from(resource.message.raw.replace(/-/g, '+').replace(/_/g, '/'), 'base64').toString('utf8');
            const parsed = parseMime(raw);
            const h = parsed.headers;
            box.remove(d.id);
            const id = box.add({
              threadId: resource.message.threadId, from: box.me, to: decodeWords(h.to || ''), cc: h.cc ? decodeWords(h.cc) : undefined,
              subject: decodeWords(h.subject || ''), text: parsed.text, labelIds: ['DRAFT'], inReplyTo: h['in-reply-to'],
              attachments: parsed.files.map((f) => ({ filename: f.filename, mimeType: f.mimeType, data: f.data })),
            });
            Object.assign(d, { id, raw, parsed, updates: (d.updates || 0) + 1 });
            return { id: draftId, message: { id, threadId: resource.message.threadId, labelIds: ['DRAFT'] } };
          },
          send: (resource) => {
            const d = box.findDraft(resource.id, 'drafts.send');
            const m = box.msgs.get(d.id);
            box.modifyLabels(m.id, ['SENT'], ['DRAFT']);
            d.sentAs = m.id;
            box.sent.push({ id: m.id, threadId: m.threadId, to: box.header(m, 'To'), subject: box.header(m, 'Subject'), body: box.partText(m, 'text/plain'), fromDraft: d.draftId });
            return { id: m.id, threadId: m.threadId, labelIds: m.labelIds.slice() };
          },
          remove: (user, draftId) => {
            const d = box.findDraft(draftId, 'drafts.delete');
            box.remove(d.id);
            d.removed = true;
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
      getTo: () => box.header(m, 'To'),
      getCc: () => box.header(m, 'Cc'),
      getSubject: () => box.header(m, 'Subject'),
      getHeader: (name) => box.header(m, name),
      getDate: () => new Date(Number(m.internalDate)),
      getPlainBody: () => box.partText(m, 'text/plain'),
      getBody: () => box.partText(m, 'text/html') || box.partText(m, 'text/plain'),
      getAttachments: () => box.attachmentBlobs(m),
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
    let saved = null; // the DRAFT message, created when the script first asks for it
    const draft = {
      getId: () => 'r-' + (saved || draft.getMessage().getId()),
      getMessage() {
        if (!saved) {
          const r = spec.replyTo;
          const to = r ? box.header(r, 'From') : spec.to;
          const subject = r ? 'Re: ' + box.header(r, 'Subject') : spec.subject;
          const opts = spec.opts || {};
          saved = box.add({
            threadId: r ? r.threadId : undefined, from: box.me, to, subject, cc: opts.cc,
            text: spec.body, html: opts.htmlBody, labelIds: ['DRAFT'], inReplyTo: r ? box.header(r, 'Message-ID') : undefined,
          });
          box.drafts.push({ id: saved, draftId: 'r-' + saved, threadId: box.msgs.get(saved).threadId, to, subject, body: spec.body, opts, replyAll: !!spec.all });
        }
        return box.gmailApp()._wrap(box.msgs.get(saved));
      },
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
    return draft;
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
      base64Encode: (data) => toBuffer(data).toString('base64'),
      base64EncodeWebSafe: (data) => toBuffer(data).toString('base64').replace(/\+/g, '-').replace(/\//g, '_'),
      computeDigest: (alg, value) => signed(crypto.createHash(alg).update(String(value), 'utf8').digest()),
      computeHmacSha256Signature: (value, key) => signed(crypto.createHmac('sha256', String(key)).update(String(value), 'utf8').digest()),
      newBlob: (data, contentType, name) => makeBlob(data, contentType, name),
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

module.exports = { createRuntime, SimulatedKill, parseMime, decodeWords };
