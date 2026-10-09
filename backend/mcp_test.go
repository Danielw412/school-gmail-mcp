package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type mcpEnv struct {
	t     *testing.T
	ts    *httptest.Server
	store *Store
	clock *fakeClock
	srv   *Server
}

func newMCPEnv(t *testing.T, allowSend bool) *mcpEnv {
	t.Helper()
	store, err := openStore(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		ListenAddr: "127.0.0.1:0", SyncSecret: testSyncSecret, APISecret: testAPISecret,
		ActionLease: 10 * time.Minute, ActionMaxAttempts: 3,
		MCPListenAddr: "127.0.0.1:0", MCPSendDelay: 60 * time.Second, MCPAllowSend: allowSend,
		AttachmentMaxBytes: 10 << 20, AttachmentFetchTimeout: 5 * time.Second, AttachmentConvertTimeout: 15 * time.Second,
	}
	clock := &fakeClock{t: time.Date(2026, 10, 6, 16, 0, 0, 0, time.UTC)}
	srv := newServer(cfg, store, newLoggerTo(io.Discard, slog.LevelError), clock.Now)
	ts := httptest.NewServer(srv.mcpRoutes())
	t.Cleanup(func() {
		ts.Close()
		store.Close()
	})
	return &mcpEnv{t: t, ts: ts, store: store, clock: clock, srv: srv}
}

func (e *mcpEnv) post(body string, headers ...string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// rpc sends one request and returns the decoded response.
func (e *mcpEnv) rpc(method string, params any) map[string]any {
	e.t.Helper()
	p, _ := json.Marshal(params)
	status, body := e.post(`{"jsonrpc":"2.0","id":7,"method":"` + method + `","params":` + string(p) + `}`)
	if status != 200 {
		e.t.Fatalf("%s: HTTP %d %s", method, status, body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		e.t.Fatalf("%s: bad JSON %s", method, body)
	}
	if out["jsonrpc"] != "2.0" || num(out["id"]) != 7 {
		e.t.Fatalf("%s: bad envelope %s", method, body)
	}
	return out
}

// call invokes a tool and returns its decoded JSON output (or the error text).
func (e *mcpEnv) call(tool string, args any) (map[string]any, bool, string) {
	e.t.Helper()
	out := e.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	if out["error"] != nil {
		e.t.Fatalf("%s: rpc error %v", tool, out["error"])
	}
	res := obj(out["result"])
	content := list(res["content"])
	if len(content) != 1 || str(obj(content[0])["type"]) != "text" {
		e.t.Fatalf("%s: unexpected content %v", tool, res)
	}
	text := str(obj(content[0])["text"])
	isErr := res["isError"] == true
	var data map[string]any
	if !isErr {
		if err := json.Unmarshal([]byte(text), &data); err != nil {
			e.t.Fatalf("%s: tool text is not JSON: %s", tool, text)
		}
	}
	return data, isErr, text
}

func (e *mcpEnv) seed() {
	e.t.Helper()
	msgs := []MessageIn{
		sampleMessage("s1", func(m *MessageIn) {
			m.Subject, m.BodyText, m.InternalDate = "Robotics meeting moved", ptr("We meet in room 204 on Thursday."), baseDate
			m.From, m.LabelIDs, m.ThreadID = "Coach Lee <lee@school.example.org>", []string{"INBOX", "Label_7"}, "thr1"
		}),
		sampleMessage("s2", func(m *MessageIn) {
			m.Subject, m.BodyText, m.InternalDate = "Calculus homework", ptr(strings.Repeat("Problem set 4 is due Monday. ", 100)), baseDate+1000
			m.From, m.LabelIDs, m.Attachments = "Dr. Shah <shah@school.example.org>", []string{"INBOX", "UNREAD"}, nil
		}),
		sampleMessage("s3", func(m *MessageIn) {
			m.Subject, m.InternalDate, m.ThreadID = "Re: Robotics meeting moved", baseDate+2000, "thr1"
			m.From, m.To, m.LabelIDs = "Student <student@school.example.org>", "lee@school.example.org", []string{"SENT"}
			m.BodyText = ptr("Thanks, see you there.")
		}),
	}
	if _, err := e.store.IngestMessages(context.Background(), msgs, baseDate); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.store.ReplaceLabels(context.Background(), []LabelIn{{ID: "Label_7", Name: "Robotics Club"}}, baseDate); err != nil {
		e.t.Fatal(err)
	}
}

func TestMCPHandshakeAndToolList(t *testing.T) {
	e := newMCPEnv(t, true)
	init := obj(e.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "openai-mcp", "version": "1"},
	})["result"])
	if str(init["protocolVersion"]) != "2025-06-18" || obj(obj(init["capabilities"])["tools"]) == nil ||
		str(obj(init["serverInfo"])["name"]) != "school-gmail" || !strings.Contains(str(init["instructions"]), "untrusted") {
		t.Fatalf("initialize: %v", init)
	}
	if v := str(obj(e.rpc("initialize", map[string]any{"protocolVersion": "1999-01-01"})["result"])["protocolVersion"]); v != mcpLatestVersion {
		t.Fatalf("unknown version negotiated to %s", v)
	}
	if status, body := e.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); status != 202 || body != "" {
		t.Fatalf("notification: %d %q", status, body)
	}
	if obj(e.rpc("ping", map[string]any{})["result"]) == nil {
		t.Fatal("ping")
	}

	tools := list(obj(e.rpc("tools/list", map[string]any{})["result"])["tools"])
	byName := map[string]map[string]any{}
	for _, raw := range tools {
		tool := obj(raw)
		byName[str(tool["name"])] = tool
		if str(obj(tool["inputSchema"])["type"]) != "object" || str(tool["description"]) == "" {
			t.Errorf("tool %s: bad schema/description", tool["name"])
		}
	}
	for _, name := range []string{"search", "fetch", "search_emails", "get_email", "get_thread", "mailbox_status",
		"list_outgoing", "send_email", "reply_to_email", "cancel_email"} {
		if byName[name] == nil {
			t.Errorf("missing tool %s", name)
		}
	}
	if obj(byName["search_emails"]["annotations"])["readOnlyHint"] != true {
		t.Error("search_emails should be read-only")
	}
	if a := obj(byName["send_email"]["annotations"]); a["readOnlyHint"] != false || a["openWorldHint"] != true {
		t.Errorf("send_email annotations: %v", a)
	}
}

func TestMCPTransportAndGuards(t *testing.T) {
	e := newMCPEnv(t, true)
	if status, _ := e.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Host", "evil.example.com"); status != 403 {
		t.Errorf("foreign Host: %d", status)
	}
	if status, _ := e.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Origin", "https://evil.example.com"); status != 403 {
		t.Errorf("foreign Origin: %d", status)
	}
	if status, _ := e.post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "Host", "localhost:8767", "Origin", "http://127.0.0.1:8767"); status != 200 {
		t.Errorf("loopback Host/Origin: %d", status)
	}
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server", "/nope"} {
		res, _ := http.Get(e.ts.URL + path)
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Errorf("GET %s: %d (tunnel-client expects 404 for absent OAuth metadata)", path, res.StatusCode)
		}
	}
	res, _ := http.Get(e.ts.URL + "/mcp")
	res.Body.Close()
	if res.StatusCode != 405 {
		t.Errorf("GET /mcp: %d", res.StatusCode)
	}
	if status, body := e.post(`{not json`); status != 400 || !strings.Contains(body, "-32700") {
		t.Errorf("parse error: %d %s", status, body)
	}
	if out := e.rpc("does/not/exist", map[string]any{}); num(obj(out["error"])["code"]) != rpcMethodNotFound {
		t.Errorf("unknown method: %v", out)
	}
	if out := e.rpc("tools/call", map[string]any{"name": "rm_rf"}); num(obj(out["error"])["code"]) != rpcInvalidParams {
		t.Errorf("unknown tool: %v", out)
	}
	status, body := e.post(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`)
	var batch []map[string]any
	if status != 200 || json.Unmarshal([]byte(body), &batch) != nil || len(batch) != 2 {
		t.Errorf("batch: %d %s", status, body)
	}
	if status, _ := e.post(`{"jsonrpc":"2.0","id":1,"method":"ping","params":"` + strings.Repeat("x", mcpMaxBody) + `"}`); status != 413 {
		t.Errorf("oversized request: %d", status)
	}
}

func TestMCPReadTools(t *testing.T) {
	e := newMCPEnv(t, true)
	e.seed()

	out, isErr, text := e.call("search_emails", map[string]any{"query": "robotics"})
	if isErr || num(out["count"]) != 2 {
		t.Fatalf("search_emails robotics: %s", text)
	}
	first := obj(list(out["emails"])[0])
	if str(first["id"]) != "s3" || str(first["thread_id"]) != "thr1" || first["attachments"] == nil {
		t.Fatalf("newest first / fields: %v", first)
	}
	out, _, text = e.call("search_emails", map[string]any{"query": "from:shah is:unread has:attachment"})
	if num(out["count"]) != 0 {
		t.Fatalf("s2 has no attachments: %s", text)
	}
	out, _, _ = e.call("search_emails", map[string]any{"query": "from:shah is:unread"})
	if num(out["count"]) != 1 {
		t.Fatalf("from:shah is:unread: %v", out)
	}
	out, _, _ = e.call("search_emails", map[string]any{"query": "label:robotics-club"})
	if num(out["count"]) != 1 || len(list(obj(list(out["emails"])[0])["labels"])) != 2 {
		t.Fatalf("label by hyphenated name: %v", out)
	}
	out, _, _ = e.call("search_emails", map[string]any{"query": "in:sent"})
	if num(out["count"]) != 1 {
		t.Fatalf("in:sent: %v", out)
	}
	out, _, _ = e.call("search_emails", map[string]any{"query": "robotics -spam"})
	if len(list(out["ignored_terms"])) != 1 {
		t.Fatalf("negation should be reported as ignored: %v", out)
	}
	out, _, _ = e.call("search_emails", map[string]any{"limit": 2})
	if num(out["count"]) != 2 || str(out["next_cursor"]) == "" {
		t.Fatalf("paging: %v", out)
	}
	out, _, _ = e.call("search_emails", map[string]any{"limit": 2, "cursor": out["next_cursor"]})
	if num(out["count"]) != 1 {
		t.Fatalf("page 2: %v", out)
	}
	if _, isErr, text = e.call("search_emails", map[string]any{"query": "after:yesterday"}); !isErr || !strings.Contains(text, "YYYY/MM/DD") {
		t.Fatalf("bad date should be a tool error: %s", text)
	}

	// Standard search/fetch shapes used by ChatGPT deep research.
	out, _, _ = e.call("search", map[string]any{"query": "calculus"})
	results := list(out["results"])
	if len(results) != 1 || str(obj(results[0])["id"]) != "s2" || !strings.Contains(str(obj(results[0])["title"]), "Dr. Shah") ||
		!strings.HasPrefix(str(obj(results[0])["url"]), "https://mail.google.com/mail/u/student@school.example.org/#all/") {
		t.Fatalf("search: %v", out)
	}
	out, _, _ = e.call("fetch", map[string]any{"id": "s1"})
	if str(out["title"]) != "Robotics meeting moved" || !strings.Contains(str(out["text"]), "room 204") ||
		!strings.Contains(str(out["text"]), "From: Coach Lee") {
		t.Fatalf("fetch: %v", out)
	}

	out, _, _ = e.call("get_email", map[string]any{"message_id": "s2", "max_chars": 500})
	if len([]rune(str(out["body"]))) != 500 || out["body_truncated"] != true || str(out["thread_id"]) != "ts2" {
		t.Fatalf("get_email truncation: %v", out)
	}
	if _, isErr, text = e.call("get_email", map[string]any{"message_id": "nope"}); !isErr || !strings.Contains(text, "search_emails") {
		t.Fatalf("unknown id: %s", text)
	}
	out, _, _ = e.call("get_thread", map[string]any{"thread_id": "thr1"})
	msgs := list(out["messages"])
	if num(out["message_count"]) != 2 || str(obj(msgs[0])["id"]) != "s1" || str(out["subject"]) != "Robotics meeting moved" {
		t.Fatalf("get_thread: %v", out)
	}
	out, _, _ = e.call("get_thread", map[string]any{"thread_id": "thr1", "limit": 1})
	if num(out["omitted_older_messages"]) != 1 || str(obj(list(out["messages"])[0])["id"]) != "s3" {
		t.Fatalf("get_thread keeps newest: %v", out)
	}
	out, _, _ = e.call("mailbox_status", map[string]any{})
	if num(out["messages"]) != 3 || str(out["account"]) != "student@school.example.org" ||
		!strings.Contains(str(out["sync"]), "has not checked in") || out["sending_enabled"] != true {
		t.Fatalf("mailbox_status: %v", out)
	}
}

func TestMCPSendReplyCancel(t *testing.T) {
	e := newMCPEnv(t, true)
	e.seed()
	ctx := context.Background()

	out, isErr, text := e.call("send_email", map[string]any{
		"to": "rivera@school.example.org, parent@example.com", "subject": "Field trip form", "body": "Hi Ms. Rivera,\nThe form is signed.\nThanks",
	})
	if isErr || str(out["status"]) != "pending" || len(list(out["to"])) != 2 || !strings.Contains(str(out["note"]), "cancel_email") ||
		str(out["send_after"]) == "" {
		t.Fatalf("send_email: %s", text)
	}
	sendID := str(out["action_id"])

	// Held back for the cancel window, then released to Apps Script.
	if got, _ := e.store.ClaimActions(ctx, 5, e.clock.Now().UnixMilli(), time.Minute); len(got) != 0 {
		t.Fatal("action released before the send delay")
	}
	// Repeating the identical request does not queue a second email.
	out, _, _ = e.call("send_email", map[string]any{
		"to": []string{"rivera@school.example.org", "parent@example.com"}, "subject": "Field trip form", "body": "Hi Ms. Rivera,\nThe form is signed.\nThanks",
	})
	if str(out["action_id"]) != sendID || !strings.Contains(str(out["note"]), "not queued again") {
		t.Fatalf("duplicate send: %v", out)
	}

	out, isErr, text = e.call("reply_to_email", map[string]any{"message_id": "s1", "body": "I'll be there.", "reply_all": true, "cc": []string{"mom@example.com"}})
	if isErr || str(out["reply_to_message_id"]) != "s1" || out["reply_all"] != true ||
		str(obj(out["in_reply_to"])["subject"]) != "Robotics meeting moved" {
		t.Fatalf("reply_to_email: %s", text)
	}
	replyID := str(out["action_id"])
	if _, isErr, _ = e.call("reply_to_email", map[string]any{"message_id": "missing", "body": "x"}); !isErr {
		t.Fatal("reply to unknown message should fail")
	}
	if _, isErr, text = e.call("send_email", map[string]any{"to": []string{"not an address"}, "subject": "x", "body": "y"}); !isErr ||
		!strings.Contains(text, "invalid email address") {
		t.Fatalf("validation error should reach the model: %s", text)
	}

	out, _, _ = e.call("cancel_email", map[string]any{"action_id": replyID})
	if str(out["status"]) != "cancelled" {
		t.Fatalf("cancel: %v", out)
	}
	e.clock.Advance(61 * time.Second)
	got, _ := e.store.ClaimActions(ctx, 5, e.clock.Now().UnixMilli(), time.Minute)
	if len(got) != 1 || got[0].ID != sendID {
		t.Fatalf("after the delay only the send should be released: %v", got)
	}
	var p ActionPayload
	_ = json.Unmarshal(got[0].Payload, &p)
	if len(p.To) != 2 || p.Subject != "Field trip form" {
		t.Fatalf("payload: %+v", p)
	}
	if _, isErr, text = e.call("cancel_email", map[string]any{"action_id": sendID}); !isErr || !strings.Contains(text, "sending it now") {
		t.Fatalf("cancelling a claimed action: %s", text)
	}

	out, _, _ = e.call("list_outgoing", map[string]any{})
	if num(out["count"]) != 2 {
		t.Fatalf("list_outgoing: %v", out)
	}
	out, _, _ = e.call("list_outgoing", map[string]any{"status": "cancelled"})
	if num(out["count"]) != 1 || str(obj(list(out["outgoing"])[0])["action_id"]) != replyID {
		t.Fatalf("list_outgoing cancelled: %v", out)
	}
}

func TestMCPReadOnlyMode(t *testing.T) {
	e := newMCPEnv(t, false)
	for _, raw := range list(obj(e.rpc("tools/list", map[string]any{})["result"])["tools"]) {
		if name := str(obj(raw)["name"]); name == "send_email" || name == "reply_to_email" || name == "cancel_email" {
			t.Errorf("%s listed in read-only mode", name)
		}
	}
	if out := e.rpc("tools/call", map[string]any{"name": "send_email", "arguments": map[string]any{}}); out["error"] == nil {
		t.Error("send_email callable in read-only mode")
	}
}

func TestParseMailQuery(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.Local)
	p, ignored, err := parseMailQuery(`from:rivera subject:"field trip" is:unread has:attachment newer_than:7d "permission form" form* -spam label:robotics-club`, now, "me@x.org")
	if err != nil {
		t.Fatal(err)
	}
	if p.From != "rivera" || p.Subject != "field trip" || p.Unread == nil || !*p.Unread || p.HasAttach == nil || p.Label != "robotics-club" {
		t.Fatalf("operators: %+v", p)
	}
	if p.After != now.Add(-7*24*time.Hour).UnixMilli() {
		t.Fatalf("newer_than: %d", p.After)
	}
	if p.FTS != `"permission form" AND "form"*` {
		t.Fatalf("fts: %q", p.FTS)
	}
	if len(ignored) != 1 || !strings.HasPrefix(ignored[0], "-spam") {
		t.Fatalf("ignored: %v", ignored)
	}

	p, _, _ = parseMailQuery(`essay OR paper deadline from:me in:anywhere after:2026/10/01 before:2026-10-05`, now, "me@x.org")
	if p.FTS != `("essay" OR "paper") AND "deadline"` || p.From != "me@x.org" || !p.WithTrash {
		t.Fatalf("OR/from:me/in:anywhere: %+v", p)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local).UnixMilli(); p.After != want {
		t.Fatalf("after: %d want %d", p.After, want)
	}
	p, _, _ = parseMailQuery(`from:me category:updates "re: lunch" AND NEAR(`, now, "")
	if p.Label != "CATEGORY_UPDATES" || p.From != "" || p.FTS != `"re: lunch" AND "AND" AND "NEAR("` {
		t.Fatalf("operator-looking text stays quoted: %+v", p)
	}
	if _, _, err := parseMailQuery("older_than:soon", now, ""); err == nil {
		t.Fatal("bad relative date accepted")
	}
}

func TestMCPConfig(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{"SYNC_SECRET": testSyncSecret, "API_SECRET": testAPISecret}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	c, err := loadConfig(env())
	if err != nil || c.MCPListenAddr != "127.0.0.1:8767" || !c.MCPAllowSend || c.MCPSendDelay != time.Minute {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	if c, _ := loadConfig(env("MCP_LISTEN_ADDR", "off")); c.MCPListenAddr != "" {
		t.Fatal("off should disable MCP")
	}
	for _, bad := range []string{"0.0.0.0:8767", "127.0.0.1:8766"} {
		if _, err := loadConfig(env("MCP_LISTEN_ADDR", bad)); err == nil {
			t.Errorf("MCP_LISTEN_ADDR %s accepted", bad)
		}
	}
	if _, err := loadConfig(env("MCP_SEND_DELAY_SECONDS", "5000")); err == nil {
		t.Error("huge send delay accepted")
	}
}
