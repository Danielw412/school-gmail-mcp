package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testSyncSecret = "sync-secret-0123456789abcdef0123456789abcdef"
	testAPISecret  = "api-secret-0123456789abcdef0123456789abcdef"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type testEnv struct {
	t     *testing.T
	ts    *httptest.Server
	store *Store
	clock *fakeClock
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	store, err := openStore(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ListenAddr: "127.0.0.1:0", SyncSecret: testSyncSecret, APISecret: testAPISecret,
		ActionLease: 10 * time.Minute, ActionMaxAttempts: 3,
	}
	clock := &fakeClock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	srv := newServer(cfg, store, newLoggerTo(io.Discard, slog.LevelError), clock.Now)
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(func() {
		ts.Close()
		store.Close()
	})
	return &testEnv{t: t, ts: ts, store: store, clock: clock}
}

type resp struct {
	status int
	body   map[string]any
	raw    string
}

// do sends a request. body may be nil, a string (sent raw) or a value (JSON-encoded).
func (e *testEnv) do(method, path, secret string, body any, headers ...string) resp {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, raw: string(raw)}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (e *testEnv) expect(r resp, status int) resp {
	e.t.Helper()
	if r.status != status {
		e.t.Fatalf("status = %d, want %d; body: %s", r.status, status, r.raw)
	}
	return r
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func ptr(s string) *string { return &s }

var baseDate = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).UnixMilli()

func sampleMessage(id string, mods ...func(*MessageIn)) MessageIn {
	m := MessageIn{
		ID: id, ThreadID: "t" + id, HistoryID: "1234", InternalDate: baseDate,
		LabelIDs: []string{"INBOX", "UNREAD", "CATEGORY_UPDATES"},
		Snippet:  "Reminder: the field trip form is due Friday &amp; needs a signature",
		From:     "Ms. Rivera <rivera@school.example.org>", To: "student@school.example.org",
		Cc: "parent@example.com", Subject: "Field trip permission form",
		DateHeader: "Wed, 1 Oct 2026 09:00:00 +0000", MessageIDHeader: "<abc@school.example.org>",
		BodyText: ptr("Please return the field trip permission form by Friday."),
		BodyHTML: ptr("<p>Please return the <b>field trip</b> permission form by Friday.</p>"),
		Attachments: []AttachmentIn{{
			PartID: "2", Filename: "permission.pdf", MimeType: "application/pdf", Size: 48213,
			AttachmentID: "ANGjdJ8-example",
		}},
	}
	for _, f := range mods {
		f(&m)
	}
	return m
}

func batch(msgs ...MessageIn) map[string]any {
	return map[string]any{"messages": msgs}
}

// ---------------------------------------------------------------------------

func TestHealth(t *testing.T) {
	e := newTestEnv(t)
	r := e.expect(e.do("GET", "/healthz", "", nil), 200)
	if r.body["ok"] != true {
		t.Fatalf("healthz body = %s", r.raw)
	}
	e.expect(e.do("POST", "/healthz", "", nil), 404) // wrong method: refused by the JSON catch-all
	e.expect(e.do("GET", "/nope", "", nil), 404)
}

func TestUnauthenticatedRequestsRejected(t *testing.T) {
	e := newTestEnv(t)
	syncRoutes := [][2]string{
		{"GET", "/v1/sync/ping"}, {"POST", "/v1/sync/messages"}, {"POST", "/v1/sync/message"},
		{"POST", "/v1/sync/known"}, {"POST", "/v1/sync/state"}, {"POST", "/v1/sync/labels"},
		{"POST", "/v1/actions/poll"}, {"POST", "/v1/actions/act_x/complete"}, {"POST", "/v1/actions/act_x/fail"},
	}
	clientRoutes := [][2]string{
		{"POST", "/v1/actions"}, {"GET", "/v1/actions"}, {"GET", "/v1/actions/act_x"},
		{"POST", "/v1/actions/act_x/cancel"}, {"GET", "/v1/messages"}, {"GET", "/v1/messages/m1"},
		{"GET", "/v1/threads/t1"}, {"GET", "/v1/stats"},
	}
	for _, rt := range append(append([][2]string{}, syncRoutes...), clientRoutes...) {
		for _, secret := range []string{"", "wrong-secret-wrong-secret-wrong-secret!!"} {
			r := e.do(rt[0], rt[1], secret, "{}")
			if r.status != 401 {
				t.Errorf("%s %s with secret %q: status %d, want 401", rt[0], rt[1], secret, r.status)
			}
		}
	}
	// Each secret only opens its own role.
	for _, rt := range syncRoutes {
		if r := e.do(rt[0], rt[1], testAPISecret, "{}"); r.status != 401 {
			t.Errorf("API secret on %s %s: status %d, want 401", rt[0], rt[1], r.status)
		}
	}
	for _, rt := range clientRoutes {
		if r := e.do(rt[0], rt[1], testSyncSecret, "{}"); r.status != 401 {
			t.Errorf("sync secret on %s %s: status %d, want 401", rt[0], rt[1], r.status)
		}
	}
	// Non-Bearer schemes are rejected too.
	r := e.do("GET", "/v1/sync/ping", "", nil, "Authorization", "Basic "+testSyncSecret)
	e.expect(r, 401)
	e.expect(e.do("GET", "/v1/sync/ping", testSyncSecret, nil), 200)
}

func TestClientAPIIsLocalOnly(t *testing.T) {
	e := newTestEnv(t)
	e.expect(e.do("GET", "/v1/stats", testAPISecret, nil), 200)
	for _, h := range []string{"Cf-Connecting-Ip", "Cf-Ray", "X-Forwarded-For"} {
		r := e.do("GET", "/v1/stats", testAPISecret, nil, h, "203.0.113.9")
		e.expect(r, 403)
		r = e.do("POST", "/v1/actions", testAPISecret, map[string]any{
			"type": "send", "to": []string{"a@example.org"}, "subject": "x", "body": "y",
		}, h, "203.0.113.9")
		e.expect(r, 403)
	}
	// The Apps Script role works through the tunnel.
	e.expect(e.do("GET", "/v1/sync/ping", testSyncSecret, nil, "Cf-Connecting-Ip", "203.0.113.9"), 200)
}

func TestIngestSingleMessage(t *testing.T) {
	e := newTestEnv(t)
	r := e.expect(e.do("POST", "/v1/sync/message", testSyncSecret, sampleMessage("m1")), 200)
	if num(r.body["inserted"]) != 1 {
		t.Fatalf("inserted = %v; %s", r.body["inserted"], r.raw)
	}
	r = e.expect(e.do("GET", "/v1/messages/m1", testAPISecret, nil), 200)
	m := obj(r.body["message"])
	if str(m["subject"]) != "Field trip permission form" || str(m["threadId"]) != "tm1" {
		t.Fatalf("stored message mismatch: %s", r.raw)
	}
	if m["unread"] != true || m["inInbox"] != true || m["starred"] != false {
		t.Fatalf("flags not derived from labels: %s", r.raw)
	}
	if str(m["cc"]) != "parent@example.com" || str(m["messageIdHeader"]) != "<abc@school.example.org>" {
		t.Fatalf("headers not stored: %s", r.raw)
	}
	if !strings.Contains(str(m["snippet"]), "Friday & needs") {
		t.Fatalf("snippet entities not decoded: %q", m["snippet"])
	}
	if m["bodyHtml"] != nil || m["hasHtml"] != true {
		t.Fatalf("HTML should only be returned with ?html=1: %s", r.raw)
	}
	atts := list(m["attachments"])
	if len(atts) != 1 || str(obj(atts[0])["filename"]) != "permission.pdf" ||
		num(obj(atts[0])["size"]) != 48213 || str(obj(atts[0])["fetchState"]) != "none" ||
		str(obj(atts[0])["attachmentId"]) != "ANGjdJ8-example" {
		t.Fatalf("attachment metadata wrong: %v", atts)
	}
	r = e.expect(e.do("GET", "/v1/messages/m1?html=1", testAPISecret, nil), 200)
	if !strings.Contains(str(obj(r.body["message"])["bodyHtml"]), "<b>field trip</b>") {
		t.Fatalf("html body missing: %s", r.raw)
	}
	e.expect(e.do("GET", "/v1/messages/nope", testAPISecret, nil), 404)
}

func TestBatchIngestion(t *testing.T) {
	e := newTestEnv(t)
	var msgs []MessageIn
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("b%03d", i)
		msgs = append(msgs, sampleMessage(id, func(m *MessageIn) {
			m.InternalDate = baseDate + int64(i)*60_000
			m.Attachments = nil
		}))
	}
	r := e.expect(e.do("POST", "/v1/sync/messages", testSyncSecret, batch(msgs...)), 200)
	if num(r.body["received"]) != 25 || num(r.body["inserted"]) != 25 || len(list(r.body["results"])) != 25 {
		t.Fatalf("batch result: %s", r.raw)
	}
	r = e.expect(e.do("GET", "/v1/stats", testAPISecret, nil), 200)
	if num(r.body["messages"]) != 25 || str(r.body["lastIngestAt"]) == "" {
		t.Fatalf("stats: %s", r.raw)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestDuplicateMessagesAreSafe(t *testing.T) {
	e := newTestEnv(t)
	b := batch(sampleMessage("d1"), sampleMessage("d2"))
	e.expect(e.do("POST", "/v1/sync/messages", testSyncSecret, b), 200)

	// Same batch again: nothing new, nothing changed.
	r := e.expect(e.do("POST", "/v1/sync/messages", testSyncSecret, b), 200)
	if num(r.body["inserted"]) != 0 || num(r.body["unchanged"]) != 2 {
		t.Fatalf("redelivery: %s", r.raw)
	}
	// Same ID twice within one batch.
	r = e.expect(e.do("POST", "/v1/sync/messages", testSyncSecret, batch(sampleMessage("d3"), sampleMessage("d3"))), 200)
	if num(r.body["inserted"]) != 1 || num(r.body["unchanged"]) != 1 {
		t.Fatalf("in-batch duplicate: %s", r.raw)
	}
	// Redelivery with new labels updates state but never content.
	changed := sampleMessage("d1", func(m *MessageIn) {
		m.LabelIDs = []string{"INBOX", "STARRED"}
		m.Subject = "tampered subject"
	})
	r = e.expect(e.do("POST", "/v1/sync/message", testSyncSecret, changed), 200)
	if num(r.body["updated"]) != 1 {
		t.Fatalf("label change: %s", r.raw)
	}
	m := obj(e.expect(e.do("GET", "/v1/messages/d1", testAPISecret, nil), 200).body["message"])
	if m["unread"] != false || m["starred"] != true || str(m["subject"]) != "Field trip permission form" {
		t.Fatalf("after redelivery: %v", m)
	}
	// Exactly one row per ID, one FTS entry per row, attachments not duplicated.
	var msgs, fts, atts int
	e.store.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&msgs)
	e.store.db.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH '"permission"'`).Scan(&fts)
	e.store.db.QueryRow(`SELECT count(*) FROM attachments`).Scan(&atts)
	if msgs != 3 || fts != 3 || atts != 3 {
		t.Fatalf("rows: messages=%d fts=%d attachments=%d, want 3/3/3", msgs, fts, atts)
	}
}

func TestMalformedPayloadsRejected(t *testing.T) {
	e := newTestEnv(t)
	cases := []struct {
		name, path, body string
		status           int
	}{
		{"not json", "/v1/sync/messages", `{"messages": [`, 400},
		{"garbage", "/v1/sync/messages", `hello`, 400},
		{"empty body", "/v1/sync/message", ``, 400},
		{"wrong top-level shape", "/v1/sync/messages", `{"msgs": []}`, 400},
		{"extra top-level field", "/v1/sync/messages", `{"messages": [{"id":"a1","threadId":"t","internalDate":1759309200000}], "x": 1}`, 400},
		{"empty batch", "/v1/sync/messages", `{"messages": []}`, 400},
		{"unknown message field", "/v1/sync/messages", `{"messages": [{"id":"a1","threadId":"t","internalDate":1759309200000,"bogus":1}]}`, 400},
		{"wrong field type", "/v1/sync/messages", `{"messages": [{"id":123,"threadId":"t","internalDate":1759309200000}]}`, 400},
		{"trailing data", "/v1/sync/message", `{"id":"a1","threadId":"t","internalDate":1759309200000} {}`, 400},
		{"known: bad id", "/v1/sync/known", `{"ids": ["ok1", "bad id!"]}`, 400},
		{"state: empty", "/v1/sync/state", `{"updates": []}`, 400},
		{"labels: bad id", "/v1/sync/labels", `{"labels": [{"id": "has space", "name": "x"}]}`, 400},
		{"poll: bad limit", "/v1/actions/poll", `{"limit": 500}`, 400},
		{"fail: no claim", "/v1/actions/act_x/fail", `{"error": "x"}`, 400},
	}
	for _, c := range cases {
		r := e.do("POST", c.path, testSyncSecret, c.body)
		if r.status != c.status {
			t.Errorf("%s: status %d, want %d (%s)", c.name, r.status, c.status, r.raw)
		}
		if r.status == 400 && obj(r.body["error"])["code"] == nil {
			t.Errorf("%s: error body missing code: %s", c.name, r.raw)
		}
	}

	// Too many messages in one batch.
	var many []MessageIn
	for i := 0; i <= maxBatchMessages; i++ {
		many = append(many, MessageIn{ID: "x" + itoa(i), ThreadID: "t", InternalDate: baseDate})
	}
	e.expect(e.do("POST", "/v1/sync/messages", testSyncSecret, batch(many...)), 400)

	// Wrong content type.
	req, _ := http.NewRequest("POST", e.ts.URL+"/v1/sync/message", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testSyncSecret)
	req.Header.Set("Content-Type", "text/plain")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 415 {
		t.Errorf("text/plain: status %d, want 415", res.StatusCode)
	}

	// Oversized body.
	huge := `{"messages":[{"id":"h1","threadId":"t","internalDate":1759309200000,"bodyText":"` +
		strings.Repeat("a", maxIngestBody) + `"}]}`
	e.expect(e.do("POST", "/v1/sync/messages", testSyncSecret, huge), 413)

	// Item-level validation: bad items are rejected individually, good ones stored.
	bad := []MessageIn{
		sampleMessage("good1"),
		{ID: "", ThreadID: "t", InternalDate: baseDate},
		{ID: "baddate", ThreadID: "t", InternalDate: 12345},
		{ID: "bad label", ThreadID: "t", InternalDate: baseDate},
		sampleMessage("biglabel", func(m *MessageIn) { m.LabelIDs = []string{"not ok"} }),
		sampleMessage("bigbody", func(m *MessageIn) { m.BodyText = ptr(strings.Repeat("x", maxBodyText+1)) }),
	}
	r := e.expect(e.do("POST", "/v1/sync/messages", testSyncSecret, batch(bad...)), 200)
	if num(r.body["inserted"]) != 1 || num(r.body["rejected"]) != 5 {
		t.Fatalf("item validation: %s", r.raw)
	}
	var n int
	e.store.db.QueryRow(`SELECT count(*) FROM messages`).Scan(&n)
	if n != 1 {
		t.Fatalf("stored %d messages, want 1", n)
	}
}

func TestKnownAndStateUpdates(t *testing.T) {
	e := newTestEnv(t)
	e.expect(e.do("POST", "/v1/sync/messages", testSyncSecret, batch(sampleMessage("k1"), sampleMessage("k2"))), 200)

	r := e.expect(e.do("POST", "/v1/sync/known", testSyncSecret, map[string]any{"ids": []string{"k1", "zz", "k2", "yy", "zz"}}), 200)
	if got := list(r.body["unknown"]); len(got) != 2 || got[0] != "zz" || got[1] != "yy" {
		t.Fatalf("unknown = %v", got)
	}

	r = e.expect(e.do("POST", "/v1/sync/state", testSyncSecret, map[string]any{"updates": []map[string]any{
		{"id": "k1", "labelIds": []string{"INBOX"}},                               // read now
		{"id": "k2", "labelIds": []string{"CATEGORY_UPDATES", "UNREAD", "INBOX"}}, // same set, other order
		{"id": "new1", "labelIds": []string{"INBOX", "UNREAD"}},                   // never seen
		{"id": "k2", "deleted": true},
		{"id": "k1"}, // neither labels nor deleted
	}}), 200)
	if num(r.body["updated"]) != 2 || num(r.body["unchanged"]) != 1 || num(r.body["rejected"]) != 1 {
		t.Fatalf("state summary: %s", r.raw)
	}
	if got := list(r.body["unknown"]); len(got) != 1 || got[0] != "new1" {
		t.Fatalf("unknown = %v", got)
	}
	m := obj(e.expect(e.do("GET", "/v1/messages/k1", testAPISecret, nil), 200).body["message"])
	if m["unread"] != false {
		t.Fatalf("k1 should be read: %v", m)
	}
	m = obj(e.expect(e.do("GET", "/v1/messages/k2", testAPISecret, nil), 200).body["message"])
	if m["deletedInGmail"] != true {
		t.Fatalf("k2 should be marked deleted: %v", m)
	}

	e.expect(e.do("POST", "/v1/sync/labels", testSyncSecret, map[string]any{"labels": []map[string]string{
		{"id": "INBOX", "name": "INBOX", "type": "system"}, {"id": "Label_7", "name": "Robotics Club", "type": "user"},
	}}), 200)
	e.expect(e.do("POST", "/v1/sync/state", testSyncSecret, map[string]any{"updates": []map[string]any{
		{"id": "k1", "labelIds": []string{"INBOX", "Label_7"}},
	}}), 200)
	m = obj(e.expect(e.do("GET", "/v1/messages/k1", testAPISecret, nil), 200).body["message"])
	if names := list(m["labels"]); len(names) != 2 || names[1] != "Robotics Club" {
		t.Fatalf("label names: %v", m["labels"])
	}
}

func TestSearch(t *testing.T) {
	e := newTestEnv(t)
	msgs := []MessageIn{
		sampleMessage("s1", func(m *MessageIn) {
			m.Subject, m.BodyText, m.InternalDate = "Robotics meeting moved", ptr("We meet in room 204 on Thursday."), baseDate
			m.From, m.LabelIDs = "Coach Lee <lee@school.example.org>", []string{"INBOX", "Label_7"}
		}),
		sampleMessage("s2", func(m *MessageIn) {
			m.Subject, m.BodyText, m.InternalDate = "Calculus homework", ptr("Problem set 4 is due Monday."), baseDate+1000
			m.From, m.LabelIDs = "Dr. Shah <shah@school.example.org>", []string{"INBOX", "UNREAD"}
		}),
		sampleMessage("s3", func(m *MessageIn) {
			m.Subject, m.BodyText, m.BodyHTML, m.InternalDate = "Newsletter", nil, ptr("<html><style>.x{}</style><p>Robotics team wins regional!</p></html>"), baseDate+2000
			m.LabelIDs = []string{"INBOX"}
		}),
		sampleMessage("s4", func(m *MessageIn) {
			m.Subject, m.InternalDate, m.LabelIDs = "Robotics spam", baseDate+3000, []string{"SPAM"}
		}),
	}
	e.expect(e.do("POST", "/v1/sync/messages", testSyncSecret, batch(msgs...)), 200)
	e.expect(e.do("POST", "/v1/sync/labels", testSyncSecret, map[string]any{"labels": []map[string]string{
		{"id": "Label_7", "name": "Robotics Club"}}}), 200)

	ids := func(path string) []string {
		t.Helper()
		r := e.expect(e.do("GET", path, testAPISecret, nil), 200)
		var out []string
		for _, m := range list(r.body["messages"]) {
			out = append(out, str(obj(m)["id"]))
		}
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	eq(ids("/v1/messages?q=robotics"), "s3", "s1") // newest first; spam excluded; HTML-only body searchable
	eq(ids("/v1/messages?q=robotics&includeTrash=true"), "s4", "s3", "s1")
	eq(ids("/v1/messages?q=homew*"), "s2")
	eq(ids("/v1/messages?q=%22unbalanced%20AND%20(OR"), []string{}...) // hostile text is quoted, not parsed
	eq(ids("/v1/messages?unread=true"), "s2")
	eq(ids("/v1/messages?from=shah"), "s2")
	eq(ids("/v1/messages?label=Robotics%20Club"), "s1")
	eq(ids("/v1/messages?label=Label_7"), "s1")
	eq(ids("/v1/messages?fts=subject:calculus"), "s2")
	eq(ids("/v1/messages?after=2026-10-01T09:00:01Z"), "s3", "s2")

	// Pagination with a bounded page size.
	r := e.expect(e.do("GET", "/v1/messages?limit=2", testAPISecret, nil), 200)
	if len(list(r.body["messages"])) != 2 || str(r.body["nextCursor"]) == "" {
		t.Fatalf("page 1: %s", r.raw)
	}
	eq(ids("/v1/messages?limit=2&cursor="+str(r.body["nextCursor"])), "s1")
	e.expect(e.do("GET", "/v1/messages?limit=1000", testAPISecret, nil), 400)
	e.expect(e.do("GET", "/v1/messages?fts=%22unterminated", testAPISecret, nil), 400)
	e.expect(e.do("GET", "/v1/messages?after=yesterday", testAPISecret, nil), 400)

	m := obj(e.expect(e.do("GET", "/v1/messages/s3", testAPISecret, nil), 200).body["message"])
	if m["bodyTextDerived"] != true || str(m["bodyText"]) != "Robotics team wins regional!" {
		t.Fatalf("derived text: %q %v", m["bodyText"], m["bodyTextDerived"])
	}
}

func TestThread(t *testing.T) {
	e := newTestEnv(t)
	e.expect(e.do("POST", "/v1/sync/messages", testSyncSecret, batch(
		sampleMessage("r2", func(m *MessageIn) { m.ThreadID, m.InternalDate = "thr", baseDate+5000 }),
		sampleMessage("r1", func(m *MessageIn) {
			m.ThreadID, m.BodyText = "thr", ptr(strings.Repeat("long ", 100))
		}),
	)), 200)
	r := e.expect(e.do("GET", "/v1/threads/thr?bodyChars=10", testAPISecret, nil), 200)
	msgs := list(r.body["messages"])
	if len(msgs) != 2 || str(obj(msgs[0])["id"]) != "r1" || len(str(obj(msgs[0])["bodyText"])) != 10 ||
		obj(msgs[0])["bodyTextTruncated"] != true {
		t.Fatalf("thread: %s", r.raw)
	}
	e.expect(e.do("GET", "/v1/threads/none", testAPISecret, nil), 404)
}

func createAction(t *testing.T, e *testEnv, body map[string]any) map[string]any {
	t.Helper()
	r := e.expect(e.do("POST", "/v1/actions", testAPISecret, body), 201)
	return obj(r.body["action"])
}

func poll(t *testing.T, e *testEnv, limit int) []map[string]any {
	t.Helper()
	r := e.expect(e.do("POST", "/v1/actions/poll", testSyncSecret, map[string]any{"limit": limit}), 200)
	var out []map[string]any
	for _, a := range list(r.body["actions"]) {
		out = append(out, obj(a))
	}
	return out
}

func TestActionCreateAndPoll(t *testing.T) {
	e := newTestEnv(t)
	send := createAction(t, e, map[string]any{
		"type": "send", "to": []string{"Ms. Rivera <rivera@school.example.org>"}, "cc": []string{"parent@example.com"},
		"subject": "Permission form", "body": "Attached is the signed form.",
	})
	if str(send["status"]) != "pending" || !strings.HasPrefix(str(send["id"]), "act_") {
		t.Fatalf("created: %v", send)
	}
	reply := createAction(t, e, map[string]any{"type": "reply", "messageId": "m1", "body": "Thanks!", "replyAll": true})

	got := poll(t, e, 5)
	if len(got) != 2 || str(got[0]["id"]) != str(send["id"]) || str(got[1]["id"]) != str(reply["id"]) {
		t.Fatalf("poll: %v", got)
	}
	p := obj(got[0]["payload"])
	if str(got[0]["type"]) != "send" || str(got[0]["claimId"]) == "" || num(got[0]["attempt"]) != 1 ||
		str(p["subject"]) != "Permission form" || len(list(p["to"])) != 1 || len(list(p["cc"])) != 1 {
		t.Fatalf("claimed send: %v", got[0])
	}
	rp := obj(got[1]["payload"])
	if str(rp["messageId"]) != "m1" || rp["replyAll"] != true || str(rp["body"]) != "Thanks!" {
		t.Fatalf("claimed reply: %v", got[1])
	}
	// Claimed actions are not handed out again while the lease is valid.
	if again := poll(t, e, 5); len(again) != 0 {
		t.Fatalf("second poll returned %d actions", len(again))
	}
	a := obj(e.expect(e.do("GET", "/v1/actions/"+str(send["id"]), testAPISecret, nil), 200).body["action"])
	if str(a["status"]) != "claimed" || num(a["attempts"]) != 1 {
		t.Fatalf("after claim: %v", a)
	}
	r := e.expect(e.do("GET", "/v1/actions?status=claimed", testAPISecret, nil), 200)
	if len(list(r.body["actions"])) != 2 {
		t.Fatalf("list: %s", r.raw)
	}
}

func TestActionCompletion(t *testing.T) {
	e := newTestEnv(t)
	a := createAction(t, e, map[string]any{"type": "send", "to": []string{"x@example.org"}, "subject": "s", "body": "b"})
	c := poll(t, e, 1)[0]
	path := "/v1/actions/" + str(a["id"]) + "/complete"
	body := map[string]any{"claimId": c["claimId"], "result": map[string]string{"messageId": "18f00aa", "threadId": "18f00aa"}}
	r := e.expect(e.do("POST", path, testSyncSecret, body), 200)
	done := obj(r.body["action"])
	if str(done["status"]) != "done" || r.body["alreadyDone"] != false || str(obj(done["result"])["messageId"]) != "18f00aa" ||
		str(done["finishedAt"]) == "" {
		t.Fatalf("complete: %s", r.raw)
	}
	// Reporting again (e.g. Apps Script retrying after a timeout) is harmless.
	r = e.expect(e.do("POST", path, testSyncSecret, body), 200)
	if r.body["alreadyDone"] != true {
		t.Fatalf("second complete: %s", r.raw)
	}
	if len(poll(t, e, 5)) != 0 {
		t.Fatal("done action was handed out again")
	}
	e.expect(e.do("POST", "/v1/actions/act_missing/complete", testSyncSecret, body), 404)
	e.expect(e.do("POST", path, testSyncSecret, map[string]any{"result": map[string]string{"messageId": "bad id"}}), 400)
}

func TestActionFailureReporting(t *testing.T) {
	e := newTestEnv(t) // ActionMaxAttempts = 3
	a := createAction(t, e, map[string]any{"type": "send", "to": []string{"x@example.org"}, "subject": "s", "body": "b"})
	id := str(a["id"])
	fail := func(claim string, retryable bool) resp {
		return e.do("POST", "/v1/actions/"+id+"/fail", testSyncSecret,
			map[string]any{"claimId": claim, "error": "Service invoked too many times", "retryable": retryable})
	}

	// Attempt 1: temporary failure → pending with backoff.
	c1 := poll(t, e, 1)[0]
	r := e.expect(fail(str(c1["claimId"]), true), 200)
	got := obj(r.body["action"])
	if str(got["status"]) != "pending" || str(got["lastError"]) == "" {
		t.Fatalf("retryable failure: %s", r.raw)
	}
	if len(poll(t, e, 1)) != 0 {
		t.Fatal("action retried before its backoff elapsed")
	}
	// A report from an old claim is refused.
	e.expect(fail(str(c1["claimId"]), true), 409)

	// Attempt 2 after backoff.
	e.clock.Advance(31 * time.Second)
	c2 := poll(t, e, 1)
	if len(c2) != 1 || num(c2[0]["attempt"]) != 2 {
		t.Fatalf("retry poll: %v", c2)
	}
	// Permanent failure → failed, never handed out again.
	r = e.expect(fail(str(c2[0]["claimId"]), false), 200)
	if str(obj(r.body["action"])["status"]) != "failed" {
		t.Fatalf("permanent failure: %s", r.raw)
	}
	e.clock.Advance(time.Hour)
	if len(poll(t, e, 5)) != 0 {
		t.Fatal("failed action was handed out again")
	}

	// Retryable failures stop after max attempts.
	b := createAction(t, e, map[string]any{"type": "send", "to": []string{"y@example.org"}, "subject": "s", "body": "b2"})
	for i := 1; i <= 3; i++ {
		c := poll(t, e, 1)
		if len(c) != 1 {
			t.Fatalf("attempt %d not offered", i)
		}
		r := e.expect(e.do("POST", "/v1/actions/"+str(b["id"])+"/fail", testSyncSecret,
			map[string]any{"claimId": c[0]["claimId"], "error": "timeout", "retryable": true}), 200)
		status := str(obj(r.body["action"])["status"])
		if (i < 3 && status != "pending") || (i == 3 && status != "failed") {
			t.Fatalf("attempt %d status %s", i, status)
		}
		e.clock.Advance(time.Hour)
	}
	// A completion still wins if the mail turns out to have been sent.
	r = e.expect(e.do("POST", "/v1/actions/"+str(b["id"])+"/complete", testSyncSecret, map[string]any{}), 200)
	if str(obj(r.body["action"])["status"]) != "done" {
		t.Fatalf("late completion: %s", r.raw)
	}
}

func TestActionLeaseExpiry(t *testing.T) {
	e := newTestEnv(t)
	a := createAction(t, e, map[string]any{"type": "reply", "threadId": "thr1", "body": "ok"})
	first := poll(t, e, 1)
	e.clock.Advance(11 * time.Minute)
	second := poll(t, e, 1)
	if len(second) != 1 || str(second[0]["id"]) != str(a["id"]) || str(second[0]["claimId"]) == str(first[0]["claimId"]) ||
		num(second[0]["attempt"]) != 2 {
		t.Fatalf("lease expiry: %v", second)
	}
}

func TestActionValidationIdempotencyAndCancel(t *testing.T) {
	e := newTestEnv(t)
	bad := []map[string]any{
		{"type": "delete", "messageId": "m1"},
		{"type": "send", "subject": "s", "body": "b"},
		{"type": "send", "to": []string{"not-an-address"}, "subject": "s", "body": "b"},
		{"type": "send", "to": []string{"a@x.org, b@x.org"}, "subject": "s", "body": "b"},
		{"type": "send", "to": []string{"a@x.org"}, "subject": "s\r\nBcc: evil@x.org", "body": "b"},
		{"type": "send", "to": []string{"a@x.org"}, "subject": "s", "body": "  "},
		{"type": "send", "to": []string{"a@x.org"}, "subject": "", "body": "b"},
		{"type": "reply", "body": "b"},
		{"type": "reply", "messageId": "m1", "to": []string{"a@x.org"}, "body": "b"},
		{"type": "send", "to": []string{"a@x.org"}, "subject": "s", "body": "b", "unknownField": 1},
	}
	for i, b := range bad {
		if r := e.do("POST", "/v1/actions", testAPISecret, b); r.status != 400 {
			t.Errorf("case %d: status %d, want 400 (%s)", i, r.status, r.raw)
		}
	}
	tooMany := make([]string, maxRecipients+1)
	for i := range tooMany {
		tooMany[i] = "p" + itoa(i) + "@example.org"
	}
	e.expect(e.do("POST", "/v1/actions", testAPISecret, map[string]any{"type": "send", "to": tooMany, "subject": "s", "body": "b"}), 400)

	// HTML-only send gets a plain-text alternative.
	h := createAction(t, e, map[string]any{"type": "send", "to": []string{"a@x.org"}, "subject": "s", "htmlBody": "<p>Hi <b>there</b></p>"})
	if str(obj(h["payload"])["body"]) != "Hi there" {
		t.Fatalf("plain alternative: %v", h["payload"])
	}

	req := map[string]any{"type": "send", "to": []string{"a@x.org"}, "subject": "s", "body": "b", "idempotencyKey": "k-1"}
	first := createAction(t, e, req)
	r := e.expect(e.do("POST", "/v1/actions", testAPISecret, req), 200)
	if str(obj(r.body["action"])["id"]) != str(first["id"]) || r.body["created"] != false {
		t.Fatalf("idempotent retry: %s", r.raw)
	}
	req["body"] = "different"
	e.expect(e.do("POST", "/v1/actions", testAPISecret, req), 409)

	r = e.expect(e.do("POST", "/v1/actions/"+str(first["id"])+"/cancel", testAPISecret, nil), 200)
	if str(obj(r.body["action"])["status"]) != "cancelled" {
		t.Fatalf("cancel: %s", r.raw)
	}
	e.expect(e.do("POST", "/v1/actions/"+str(first["id"])+"/cancel", testAPISecret, nil), 409)
	for _, c := range poll(t, e, 20) {
		if str(c["id"]) == str(first["id"]) {
			t.Fatal("cancelled action was handed out")
		}
	}
}

func TestConfig(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	ok := []string{"SYNC_SECRET", testSyncSecret, "API_SECRET", testAPISecret}
	c, err := loadConfig(env(ok...))
	if err != nil || c.ListenAddr != "127.0.0.1:8766" || c.AllowRemoteAPI {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for _, addr := range []string{"0.0.0.0:8766", ":8766", "192.168.1.5:8766", "localhost:8766", "[::]:8766"} {
		if _, err := loadConfig(env(append(ok, "LISTEN_ADDR", addr)...)); err == nil {
			t.Errorf("LISTEN_ADDR %s accepted", addr)
		}
	}
	if _, err := loadConfig(env(append(ok, "LISTEN_ADDR", "[::1]:9000")...)); err != nil {
		t.Errorf("[::1] rejected: %v", err)
	}
	if _, err := loadConfig(env("SYNC_SECRET", "short", "API_SECRET", testAPISecret)); err == nil {
		t.Error("short secret accepted")
	}
	if _, err := loadConfig(env("SYNC_SECRET", testSyncSecret, "API_SECRET", testSyncSecret)); err == nil {
		t.Error("identical secrets accepted")
	}
}

func TestHTMLToText(t *testing.T) {
	in := `<html><head><title>x</title><style>p{color:red}</style></head><body><!-- c -->
		<p>Hello&nbsp;<b>world</b> &amp; friends</p><div>Line two<br>Line three</div>
		<script>alert(1)</script><table><tr><td>a</td><td>b</td></tr></table></body></html>`
	got := htmlToText(in)
	want := "Hello world & friends\nLine two\nLine three\n\na b"
	if got != want {
		t.Fatalf("htmlToText:\n got %q\nwant %q", got, want)
	}
	if s := truncateUTF8("héllo", 2); s != "h" {
		t.Fatalf("truncateUTF8 split a rune: %q", s)
	}
}

func TestBackup(t *testing.T) {
	e := newTestEnv(t)
	e.expect(e.do("POST", "/v1/sync/message", testSyncSecret, sampleMessage("bk1")), 200)
	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := e.store.Backup(context.Background(), dest); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Backup(context.Background(), dest); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	b, err := openStore(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var n int
	b.db.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'permission'`).Scan(&n)
	if n != 1 {
		t.Fatalf("backup FTS rows = %d", n)
	}
}
