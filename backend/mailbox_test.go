package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (e *mcpEnv) labelsOf(id string) string {
	e.t.Helper()
	var labels string
	if err := e.store.db.QueryRow(`SELECT label_ids FROM messages WHERE gmail_id = ?`, id).Scan(&labels); err != nil {
		e.t.Fatal(err)
	}
	return labels
}

func (e *mcpEnv) searchIDs(query string) []string {
	e.t.Helper()
	out, isErr, text := e.call("search_emails", map[string]any{"query": query, "limit": 50})
	if isErr {
		e.t.Fatalf("search %q: %s", query, text)
	}
	var ids []string
	for _, m := range list(out["emails"]) {
		ids = append(ids, str(obj(m)["id"]))
	}
	return ids
}

func (e *mcpEnv) mustCall(tool string, args any) map[string]any {
	e.t.Helper()
	out, isErr, text := e.call(tool, args)
	if isErr {
		e.t.Fatalf("%s: %s", tool, text)
	}
	return out
}

func (e *mcpEnv) mustFail(tool string, args any, want string) {
	e.t.Helper()
	_, isErr, text := e.call(tool, args)
	if !isErr || !strings.Contains(text, want) {
		e.t.Fatalf("%s %v: want an error containing %q, got %s", tool, args, want, text)
	}
}

func (e *mcpEnv) claimAll() []ClaimedAction {
	e.t.Helper()
	got, err := e.store.ClaimActions(context.Background(), 20, e.clock.Now().UnixMilli(), time.Minute)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func payloadOf(t *testing.T, c ClaimedAction) ActionPayload {
	t.Helper()
	var p ActionPayload
	if err := json.Unmarshal(c.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMCPUpdateEmails(t *testing.T) {
	e := newMCPEnv(t, true)
	e.seed()
	ctx := context.Background()

	// Mark read and star one email: search reflects it at once.
	out := e.mustCall("update_emails", map[string]any{"message_ids": []string{"s2"}, "mark_read": true, "starred": true})
	readID := str(out["action_id"])
	if str(out["type"]) != "modify" || str(out["status"]) != "pending" || out["send_after"] != nil ||
		!strings.Contains(strings.Join(toStrings(out["changes"]), ","), "mark as read") {
		t.Fatalf("update_emails: %v", out)
	}
	if ids := e.searchIDs("is:unread"); len(ids) != 0 {
		t.Fatalf("still unread locally: %v", ids)
	}
	if ids := e.searchIDs("is:starred"); len(ids) != 1 || ids[0] != "s2" {
		t.Fatalf("starred: %v", ids)
	}

	// Whole conversation: archive and mark important, by label name.
	out = e.mustCall("update_emails", map[string]any{"thread_ids": []string{"thr1"}, "archive": true, "add_labels": []string{"important"}})
	threadID := str(out["action_id"])
	if e.labelsOf("s1") != `["IMPORTANT","Label_7"]` || e.labelsOf("s3") != `["IMPORTANT","SENT"]` {
		t.Fatalf("thread change: %s %s", e.labelsOf("s1"), e.labelsOf("s3"))
	}
	// Repeating a change is not deduplicated (it may have been undone meanwhile).
	out = e.mustCall("update_emails", map[string]any{"thread_ids": []string{"thr1"}, "archive": true, "add_labels": []string{"important"}})
	if str(out["action_id"]) == threadID {
		t.Fatal("modify should not be treated as a duplicate")
	}
	e.mustCall("cancel_email", map[string]any{"action_id": str(out["action_id"])})

	// Cancelling puts the local copy back.
	out = e.mustCall("cancel_email", map[string]any{"action_id": threadID})
	if !strings.Contains(str(out["note"]), "put back") {
		t.Fatalf("cancel note: %v", out)
	}
	if e.labelsOf("s1") != `["INBOX","Label_7"]` || e.labelsOf("s3") != `["SENT"]` {
		t.Fatalf("after cancel: %s %s", e.labelsOf("s1"), e.labelsOf("s3"))
	}

	// Mailbox changes are not held back: Apps Script can claim them at once.
	got := e.claimAll()
	if len(got) != 1 || got[0].ID != readID {
		t.Fatalf("claim: %+v", got)
	}
	p := payloadOf(t, got[0])
	if strings.Join(p.MessageIDs, ",") != "s2" || strings.Join(p.RemoveLabelIDs, ",") != "UNREAD" || strings.Join(p.AddLabelIDs, ",") != "STARRED" {
		t.Fatalf("payload: %+v", p)
	}
	// A permanent failure restores the previous state.
	if _, err := e.store.FailAction(ctx, readID, got[0].ClaimID, "Gmail said no", false, e.clock.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if e.labelsOf("s2") != `["INBOX","UNREAD"]` {
		t.Fatalf("after failure: %s", e.labelsOf("s2"))
	}

	// A newer Gmail state is not overwritten by an undo.
	out = e.mustCall("update_emails", map[string]any{"message_ids": []string{"s2"}, "mark_read": true})
	if _, err := e.store.ApplyStateUpdates(ctx, []StateUpdate{{ID: "s2", LabelIDs: []string{"INBOX", "STARRED"}}}, e.clock.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	e.mustCall("cancel_email", map[string]any{"action_id": str(out["action_id"])})
	if e.labelsOf("s2") != `["INBOX","STARRED"]` {
		t.Fatalf("undo overwrote newer state: %s", e.labelsOf("s2"))
	}

	// Trash hides the email from search; restore brings it back. Completion is reported.
	out = e.mustCall("update_emails", map[string]any{"message_ids": []string{"s1"}, "trash": true})
	trashID := str(out["action_id"])
	if ids := e.searchIDs("robotics"); len(ids) != 1 || ids[0] != "s3" {
		t.Fatalf("trashed email still listed: %v", ids)
	}
	got = e.claimAll()
	if len(got) != 1 || !payloadOf(t, got[0]).Trash {
		t.Fatalf("trash claim: %+v", got)
	}
	if _, _, err := e.store.CompleteAction(ctx, trashID, ActionResult{Modified: 1}, e.clock.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	out = e.mustCall("list_outgoing", map[string]any{"limit": 1})
	if first := obj(list(out["outgoing"])[0]); num(first["modified"]) != 1 || str(first["status_meaning"]) != "applied in Gmail" {
		t.Fatalf("done modify: %v", first)
	}
	e.mustCall("update_emails", map[string]any{"message_ids": []string{"s1"}, "trash": false})
	if ids := e.searchIDs("robotics"); len(ids) != 2 {
		t.Fatalf("restored email: %v", ids)
	}

	// A lease that expires on the last attempt fails the action and undoes it.
	out = e.mustCall("update_emails", map[string]any{"message_ids": []string{"s1"}, "add_labels": []string{"Robotics Club"}, "starred": true})
	leaseID := str(out["action_id"])
	for i := 0; i < 3; i++ {
		if !claimed(e.claimAll(), leaseID) {
			t.Fatalf("attempt %d: not offered", i+1)
		}
		e.clock.Advance(2 * time.Minute)
	}
	e.claimAll()
	a, _ := e.store.GetAction(ctx, leaseID)
	if a.Status != "failed" || e.labelsOf("s1") != `["INBOX","Label_7"]` {
		t.Fatalf("after lease expiry: %s %s", a.Status, e.labelsOf("s1"))
	}

	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"mark_read": true}, "message_ids"},
		{map[string]any{"message_ids": []string{"nope"}, "mark_read": true}, "no synced email"},
		{map[string]any{"thread_ids": []string{"nope"}, "mark_read": true}, "no synced conversation"},
		{map[string]any{"message_ids": []string{"s1"}}, "needs a change"},
		{map[string]any{"message_ids": []string{"s1"}, "add_labels": []string{"Homework"}}, "list_labels"},
		{map[string]any{"message_ids": []string{"s1"}, "add_labels": []string{"sent"}}, "cannot be added"},
		{map[string]any{"message_ids": []string{"s1"}, "add_labels": []string{"robotics-club"}, "remove_labels": []string{"Robotics Club"}}, "both added and removed"},
	} {
		e.mustFail("update_emails", c.args, c.want)
	}
}

func claimed(got []ClaimedAction, id string) bool {
	for _, c := range got {
		if c.ID == id {
			return true
		}
	}
	return false
}

func toStrings(v any) []string {
	var out []string
	for _, x := range list(v) {
		out = append(out, str(x))
	}
	return out
}

func TestMCPListLabels(t *testing.T) {
	e := newMCPEnv(t, true)
	e.seed()
	ctx := context.Background()
	if _, err := e.store.ReplaceLabels(ctx, []LabelIn{
		{ID: "INBOX", Name: "INBOX", Type: "system"}, {ID: "SENT", Name: "SENT", Type: "system"},
		{ID: "CHAT", Name: "CHAT", Type: "system"}, {ID: "Label_7", Name: "Robotics Club", Type: "user"},
	}, baseDate); err != nil {
		t.Fatal(err)
	}
	out := e.mustCall("list_labels", map[string]any{})
	byName := map[string]map[string]any{}
	for _, l := range list(out["labels"]) {
		byName[str(obj(l)["name"])] = obj(l)
	}
	if num(out["count"]) != 3 || byName["CHAT"] != nil || str(byName["Robotics Club"]["id"]) != "Label_7" ||
		num(byName["Robotics Club"]["emails"]) != 1 || num(byName["INBOX"]["emails"]) != 2 || num(byName["INBOX"]["unread"]) != 1 ||
		byName["SENT"]["can_add_or_remove"] != false {
		t.Fatalf("list_labels: %v", out)
	}
	if str(obj(list(out["labels"])[0])["name"]) != "Robotics Club" {
		t.Fatalf("user labels should come first: %v", out)
	}
}

func TestMCPForwardAttachmentsAndSchedule(t *testing.T) {
	e := newMCPEnv(t, true)
	e.seed()
	if _, err := e.store.IngestMessages(context.Background(), []MessageIn{sampleMessage("f1")}, baseDate); err != nil {
		t.Fatal(err)
	}

	out := e.mustCall("forward_email", map[string]any{"message_id": "f1", "to": "friend@example.com", "note": "FYI, see below."})
	if str(out["type"]) != "forward" || str(out["forward_message_id"]) != "f1" || str(out["send_after"]) == "" ||
		str(obj(list(obj(out["forwarding"])["attachments_included"])[0])["filename"]) != "permission.pdf" {
		t.Fatalf("forward_email: %v", out)
	}
	out = e.mustCall("forward_email", map[string]any{"message_id": "f1", "as_draft": true})
	if str(out["type"]) != "draft_forward" || out["send_after"] != nil {
		t.Fatalf("forward as draft: %v", out)
	}
	e.mustFail("forward_email", map[string]any{"message_id": "f1"}, "at least one recipient")
	e.mustFail("forward_email", map[string]any{"message_id": "f1", "as_draft": true, "send_at": "2026-10-07 08:00"}, "send_at does not apply")

	// Attach a file from another email.
	out = e.mustCall("send_email", map[string]any{
		"to": "rivera@school.example.org", "subject": "Signed form", "body": "Here it is.",
		"attachments": []map[string]any{{"message_id": "f1", "attachment_id": "2"}},
	})
	if strings.Join(toStrings(out["attachments"]), ",") != "permission.pdf" {
		t.Fatalf("attachments: %v", out)
	}
	e.mustCall("reply_to_email", map[string]any{"message_id": "s1", "body": "Attached.",
		"attachments": []map[string]any{{"message_id": "f1", "attachment_id": "2"}}})
	e.mustCall("create_draft", map[string]any{"body": "Draft with file",
		"attachments": []map[string]any{{"message_id": "f1", "attachment_id": "2"}}})
	e.mustFail("send_email", map[string]any{"to": "a@x.org", "subject": "s", "body": "b",
		"attachments": []map[string]any{{"message_id": "f1", "attachment_id": "9"}}}, "no attachment")
	e.mustFail("send_email", map[string]any{"to": "a@x.org", "subject": "s", "body": "b",
		"attachments": []map[string]any{{"message_id": "missing", "attachment_id": "2"}}}, "no synced email")

	// Scheduled send: held until send_at, in RFC 3339 or local time.
	at := e.clock.Now().Add(2 * time.Hour)
	out = e.mustCall("send_email", map[string]any{"to": "a@x.org", "subject": "Later", "body": "Scheduled", "send_at": at.Format(time.RFC3339)})
	schedID := str(out["action_id"])
	if str(out["send_after"]) != at.In(time.Local).Format(time.RFC3339) || !strings.Contains(str(out["note"]), "Scheduled") {
		t.Fatalf("scheduled: %v", out)
	}
	local := e.clock.Now().In(time.Local).Add(26 * time.Hour).Format("2006-01-02 15:04")
	out = e.mustCall("reply_to_email", map[string]any{"message_id": "s1", "body": "Tomorrow", "send_at": local})
	want, _ := time.ParseInLocation("2006-01-02 15:04", local, time.Local)
	if str(out["send_after"]) != want.Format(time.RFC3339) {
		t.Fatalf("local send_at %s: %v", local, out)
	}
	e.mustFail("send_email", map[string]any{"to": "a@x.org", "subject": "s", "body": "b", "send_at": "2020-01-01T00:00:00Z"}, "in the past")
	e.mustFail("send_email", map[string]any{"to": "a@x.org", "subject": "s", "body": "b", "send_at": "2030-01-01T00:00:00Z"}, "one year")
	e.mustFail("send_email", map[string]any{"to": "a@x.org", "subject": "s", "body": "b", "send_at": "next tuesday"}, "send_at: use")

	e.clock.Advance(61 * time.Second)
	for _, c := range e.claimAll() {
		if c.ID == schedID {
			t.Fatal("scheduled email released early")
		}
		if c.Type == "send" && len(payloadOf(t, c).Attachments) == 1 {
			if a := payloadOf(t, c).Attachments[0]; a.MessageID != "f1" || a.AttachmentID != "2" || a.Size != 48213 {
				t.Fatalf("attachment ref: %+v", a)
			}
		}
	}
	e.clock.Advance(2 * time.Hour)
	if !claimed(e.claimAll(), schedID) {
		t.Fatal("scheduled email not released after send_at")
	}
}

func draftMessage(id, threadID string, mods ...func(*MessageIn)) MessageIn {
	return sampleMessage(id, append([]func(*MessageIn){func(m *MessageIn) {
		m.ThreadID, m.LabelIDs, m.From, m.Cc = threadID, []string{"DRAFT"}, "student@school.example.org", ""
	}}, mods...)...)
}

func TestMCPDrafts(t *testing.T) {
	e := newMCPEnv(t, true)
	e.seed()
	ctx := context.Background()
	now := e.clock.Now().UnixMilli()

	out := e.mustCall("list_drafts", map[string]any{})
	if num(out["count"]) != 0 || !strings.Contains(str(out["hint"]), "1.3.0") {
		t.Fatalf("before sync: %v", out)
	}

	refs := []DraftRef{{ID: "r-1", MessageID: "dm1", ThreadID: "dt1"}, {ID: "r-2", MessageID: "dm2", ThreadID: "thr1"}}
	sum, err := e.store.SyncDrafts(ctx, DraftSyncRequest{Drafts: refs}, now)
	if err != nil || strings.Join(sum.Need, ",") != "r-1,r-2" {
		t.Fatalf("first sync: %+v %v", sum, err)
	}
	sum, err = e.store.SyncDrafts(ctx, DraftSyncRequest{Drafts: refs, Messages: []MessageIn{
		draftMessage("dm1", "dt1", func(m *MessageIn) {
			m.To, m.Subject, m.BodyText = "rivera@school.example.org, Parent <parent@example.com>", "Essay draft", ptr("Dear Ms. Rivera, here is my essay.")
		}),
		draftMessage("dm2", "thr1", func(m *MessageIn) {
			m.To, m.Subject, m.InReplyTo, m.BodyText, m.Attachments = "", "Re: Robotics meeting moved", "<x@y>", nil, nil
			m.BodyHTML = ptr("<p>See you <b>Thursday</b></p>")
		}),
	}}, now)
	if err != nil || sum.Stored != 2 || len(sum.Need) != 0 {
		t.Fatalf("content sync: %+v %v", sum, err)
	}

	out = e.mustCall("list_drafts", map[string]any{})
	if num(out["count"]) != 2 || out["hint"] != nil {
		t.Fatalf("list_drafts: %v", out)
	}
	out = e.mustCall("get_draft", map[string]any{"draft_id": "r-2"})
	if out["is_reply"] != true || !strings.Contains(str(out["body"]), "See you Thursday") || str(out["body_note"]) == "" {
		t.Fatalf("get_draft: %v", out)
	}
	out = e.mustCall("get_draft", map[string]any{"draft_id": "r-1"})
	if len(list(out["attachments"])) != 1 || str(out["subject"]) != "Essay draft" {
		t.Fatalf("get_draft r-1: %v", out)
	}

	// Edit: only the subject changes; recipients and body are kept.
	out = e.mustCall("update_draft", map[string]any{"draft_id": "r-1", "subject": "Essay (final)"})
	if str(out["type"]) != "draft_update" || str(out["draft_id"]) != "r-1" {
		t.Fatalf("update_draft: %v", out)
	}
	got := e.claimAll()
	if len(got) != 1 {
		t.Fatalf("draft updates are not delayed: %+v", got)
	}
	p := payloadOf(t, got[0])
	if p.DraftMessageID != "dm1" || p.Subject != "Essay (final)" || len(p.To) != 2 || p.To[1] != `"Parent" <parent@example.com>` ||
		p.Body != "Dear Ms. Rivera, here is my essay." || p.DropAttachments {
		t.Fatalf("draft_update payload: %+v", p)
	}
	out = e.mustCall("update_draft", map[string]any{"draft_id": "r-2", "to": "lee@school.example.org", "body": "See you then!", "remove_attachments": true})
	if str(out["formatting_note"]) == "" {
		t.Fatalf("HTML draft edit should warn: %v", out)
	}
	e.mustFail("update_draft", map[string]any{"draft_id": "r-1"}, "nothing to change")

	// Send and delete.
	e.mustFail("send_draft", map[string]any{"draft_id": "r-2"}, "no recipients")
	out = e.mustCall("send_draft", map[string]any{"draft_id": "r-1"})
	if str(out["send_after"]) == "" || str(obj(out["draft"])["subject"]) != "Essay draft" {
		t.Fatalf("send_draft: %v", out)
	}
	out = e.mustCall("delete_draft", map[string]any{"draft_id": "r-2"})
	if str(out["delete_after"]) == "" || str(out["status_meaning"]) == "" {
		t.Fatalf("delete_draft: %v", out)
	}
	out = e.mustCall("list_drafts", map[string]any{})
	for _, d := range list(out["drafts"]) {
		if obj(d)["queued_change"] == nil {
			t.Fatalf("queued changes should be shown: %v", d)
		}
	}
	e.mustFail("get_draft", map[string]any{"draft_id": "r-9"}, "no draft has id")

	// Edited in Gmail: the content is dropped until it is re-sent; r-2 is gone.
	sum, _ = e.store.SyncDrafts(ctx, DraftSyncRequest{Drafts: []DraftRef{{ID: "r-1", MessageID: "dm1b", ThreadID: "dt1"}}}, now)
	if sum.Removed != 1 || strings.Join(sum.Need, ",") != "r-1" {
		t.Fatalf("edited draft: %+v", sum)
	}
	out = e.mustCall("get_draft", map[string]any{"draft_id": "r-1"})
	if !strings.Contains(str(out["note"]), "not synced") {
		t.Fatalf("draft without content: %v", out)
	}
	e.mustFail("update_draft", map[string]any{"draft_id": "r-1", "body": "x"}, "not synced yet")
	e.mustFail("send_draft", map[string]any{"draft_id": "r-1"}, "not synced yet")

	status := e.mustCall("mailbox_status", map[string]any{})
	if strings.Contains(str(status["drafts_last_synced"]), "never") {
		t.Fatalf("mailbox_status: %v", status)
	}
}

func TestMCPReadOnlyHidesNewWriteTools(t *testing.T) {
	e := newMCPEnv(t, false)
	names := map[string]bool{}
	for _, tool := range list(obj(e.rpc("tools/list", map[string]any{})["result"])["tools"]) {
		names[str(obj(tool)["name"])] = true
	}
	for _, n := range []string{"list_labels", "list_drafts", "get_draft"} {
		if !names[n] {
			t.Errorf("read tool %s missing", n)
		}
	}
	for _, n := range []string{"update_emails", "forward_email", "update_draft", "send_draft", "delete_draft"} {
		if names[n] {
			t.Errorf("write tool %s listed in read-only mode", n)
		}
	}
}

func TestActionValidationNewTypes(t *testing.T) {
	e := newTestEnv(t)
	e.expect(e.do("POST", "/v1/sync/messages", testSyncSecret, batch(sampleMessage("m1"))), 200)

	for _, c := range []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"type": "modify", "removeLabelIds": []string{"UNREAD"}}, "messageIds and/or threadIds"},
		{map[string]any{"type": "modify", "messageIds": []string{"m1"}}, "needs a change"},
		{map[string]any{"type": "modify", "messageIds": []string{"m1"}, "addLabelIds": []string{"SENT"}}, "cannot be added"},
		{map[string]any{"type": "modify", "messageIds": []string{"m1"}, "trash": true, "untrash": true}, "cannot both"},
		{map[string]any{"type": "modify", "messageIds": []string{"m1"}, "trash": true, "body": "x"}, "body is not valid"},
		{map[string]any{"type": "modify", "messageIds": []string{"bad id"}, "trash": true}, "invalid id"},
		{map[string]any{"type": "forward", "to": []string{"a@x.org"}}, "forward needs messageId"},
		{map[string]any{"type": "forward", "messageId": "m1", "to": []string{"a@x.org"}, "subject": "x"}, "subject is not valid"},
		{map[string]any{"type": "draft_update", "draftId": "r-1", "body": "x"}, "draftMessageId is required"},
		{map[string]any{"type": "draft_delete"}, "draftId"},
		{map[string]any{"type": "draft", "body": "x", "sendAt": "2026-10-07T00:00:00Z"}, "sendAt is only valid"},
		{map[string]any{"type": "send", "to": []string{"a@x.org"}, "subject": "s", "body": "b", "sendAt": "tomorrow"}, "RFC 3339"},
		{map[string]any{"type": "send", "to": []string{"a@x.org"}, "subject": "s", "body": "b",
			"attachments": []map[string]any{{"messageId": "m1", "attachmentId": "../x"}}}, "attachmentId: invalid"},
		{map[string]any{"type": "delete"}, "type must be"},
	} {
		r := e.expect(e.do("POST", "/v1/actions", testAPISecret, c.body), 400)
		if !strings.Contains(r.raw, c.want) {
			t.Errorf("%v: want %q in %s", c.body, c.want, r.raw)
		}
	}

	// A modify through the HTTP API changes the local copy too.
	a := createAction(t, e, map[string]any{"type": "modify", "messageIds": []string{"m1"}, "removeLabelIds": []string{"UNREAD", "INBOX"}})
	m := e.expect(e.do("GET", "/v1/messages/m1", testAPISecret, nil), 200)
	if obj(m.body["message"])["unread"] != false || obj(m.body["message"])["inInbox"] != false {
		t.Fatalf("local modify: %v", m.raw)
	}
	claimed := poll(t, e, 5)
	if len(claimed) != 1 || str(claimed[0]["id"]) != str(a["id"]) {
		t.Fatalf("poll: %v", claimed)
	}
	r := e.do("POST", "/v1/actions/"+str(a["id"])+"/complete", testSyncSecret,
		map[string]any{"claimId": claimed[0]["claimId"], "result": map[string]any{"modified": 1, "missing": []string{"bad id"}}})
	e.expect(r, 400)
	e.expect(e.do("POST", "/v1/actions/"+str(a["id"])+"/complete", testSyncSecret,
		map[string]any{"claimId": claimed[0]["claimId"], "result": map[string]any{"modified": 1, "missing": []string{"gone1"}}}), 200)

	// sendAt holds the action.
	at := e.clock.Now().Add(3 * time.Hour).UTC()
	s := createAction(t, e, map[string]any{"type": "send", "to": []string{"a@x.org"}, "subject": "s", "body": "b", "sendAt": at.Format(time.RFC3339)})
	if str(s["notBefore"]) != at.Format(time.RFC3339) {
		t.Fatalf("sendAt: %v", s)
	}

	// Drafts sync endpoint.
	e.expect(e.do("POST", "/v1/sync/drafts", testSyncSecret, map[string]any{"drafts": []map[string]any{{"id": "bad id", "messageId": "x"}}}), 400)
	d := e.expect(e.do("POST", "/v1/sync/drafts", testSyncSecret, map[string]any{"drafts": []map[string]any{{"id": "r-5", "messageId": "dm5", "threadId": "dt5"}}}), 200)
	if strings.Join(toStrings(d.body["need"]), ",") != "r-5" {
		t.Fatalf("drafts sync: %s", d.raw)
	}
	e.expect(e.do("POST", "/v1/sync/drafts", testAPISecret, map[string]any{"drafts": []any{}}), 401)
}

func TestMigrationV4FromV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV1 + schemaV2 + schemaV3 + `PRAGMA user_version = 3;
		INSERT INTO actions (id, type, status, payload, idempotency_key, attempts, max_attempts, not_before, created_at, updated_at)
		VALUES ('act_v3', 'draft_reply', 'pending', '{"messageId":"m1","body":"b"}', 'k3', 0, 5, 1, 1, 1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if a, err := s.GetAction(ctx, "act_v3"); err != nil || a.Type != "draft_reply" || a.IdempotencyKey != "k3" {
		t.Fatalf("action after migration: %+v %v", a, err)
	}
	for _, typ := range []string{"forward", "draft_forward", "modify", "draft_update", "draft_delete", "draft_send"} {
		if _, _, err := s.CreateAction(ctx, typ, ActionPayload{DraftID: "r-1", MessageIDs: []string{"m9"}, Trash: true}, "", 5, 10, 0); err != nil {
			t.Fatalf("create %s: %v", typ, err)
		}
	}
	if _, err := s.SyncDrafts(ctx, DraftSyncRequest{Drafts: []DraftRef{{ID: "r-1", MessageID: "x1"}}}, 10); err != nil {
		t.Fatalf("drafts table: %v", err)
	}
}
