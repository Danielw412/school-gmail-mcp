package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
	"time"
)

// MCP tools for labels, mailbox changes, forwarding and existing drafts.

func (s *Server) toolListLabels(ctx context.Context, _ json.RawMessage) (any, error) {
	labels, err := s.store.ListLabels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		if l.ID == "CHAT" {
			continue
		}
		e := map[string]any{"name": l.Name, "id": l.ID, "type": l.Type, "emails": l.Messages, "unread": l.Unread}
		if labelsNotModifiable[l.ID] {
			e["can_add_or_remove"] = false
		}
		out = append(out, e)
	}
	res := map[string]any{"count": len(out), "labels": out}
	if len(out) == 0 {
		res["hint"] = "No labels are synced yet; Apps Script refreshes the label list every few hours."
	}
	return res, nil
}

func (s *Server) toolUpdateEmails(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		MessageIDs   []string `json:"message_ids"`
		ThreadIDs    []string `json:"thread_ids"`
		MarkRead     *bool    `json:"mark_read"`
		Starred      *bool    `json:"starred"`
		Important    *bool    `json:"important"`
		Archive      *bool    `json:"archive"`
		AddLabels    []string `json:"add_labels"`
		RemoveLabels []string `json:"remove_labels"`
		Trash        *bool    `json:"trash"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if len(a.MessageIDs)+len(a.ThreadIDs) == 0 {
		return nil, userErr("give message_ids and/or thread_ids (from search_emails)")
	}
	if len(dedupe(a.MessageIDs))+len(dedupe(a.ThreadIDs)) > maxModifyTargets {
		return nil, userErr("at most %d emails and conversations per call; split the list", maxModifyTargets)
	}
	for _, id := range dedupe(a.MessageIDs) {
		if _, err := s.loadEmail(ctx, id); err != nil {
			return nil, err
		}
	}
	for _, id := range dedupe(a.ThreadIDs) {
		if !gmailIDPattern.MatchString(id) {
			return nil, userErr("%q is not a Gmail thread id", truncateForError(id))
		}
		n, err := queryStrings(ctx, s.store.db, `SELECT gmail_id FROM messages WHERE thread_id = ? LIMIT 1`, id)
		if err != nil {
			return nil, err
		}
		if len(n) == 0 {
			return nil, userErr("no synced conversation has thread id %s", id)
		}
	}

	var add, remove []string
	flag := func(v *bool, label string, setWhenTrue bool) {
		if v == nil {
			return
		}
		if *v == setWhenTrue {
			add = append(add, label)
		} else {
			remove = append(remove, label)
		}
	}
	flag(a.MarkRead, "UNREAD", false)
	flag(a.Starred, "STARRED", true)
	flag(a.Important, "IMPORTANT", true)
	flag(a.Archive, "INBOX", false)
	resolve := func(names []string, dst *[]string) error {
		for _, n := range names {
			id, err := s.store.ResolveLabel(ctx, n)
			if err != nil {
				return err
			}
			if id == "" {
				return userErr("there is no label named %q; list_labels shows the labels (new labels cannot be created here)", truncateForError(n))
			}
			if labelsNotModifiable[id] {
				return userErr("label %q cannot be added or removed; use trash for the trash", truncateForError(n))
			}
			*dst = append(*dst, id)
		}
		return nil
	}
	if err := resolve(a.AddLabels, &add); err != nil {
		return nil, err
	}
	if err := resolve(a.RemoveLabels, &remove); err != nil {
		return nil, err
	}
	req := ActionRequest{Type: "modify", MessageIDs: a.MessageIDs, ThreadIDs: a.ThreadIDs, AddLabelIDs: add, RemoveLabelIDs: remove}
	if a.Trash != nil {
		req.Trash, req.Untrash = *a.Trash, !*a.Trash
	}
	p, err := req.toPayload()
	if err != nil {
		return nil, userErr("%v", err)
	}
	return s.queueAction(ctx, "modify", p, 0)
}

func (s *Server) toolForwardEmail(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		MessageID   string          `json:"message_id"`
		To          addrList        `json:"to"`
		Cc          addrList        `json:"cc"`
		Bcc         addrList        `json:"bcc"`
		Note        string          `json:"note"`
		Attachments []attachmentArg `json:"attachments"`
		SendAt      string          `json:"send_at"`
		AsDraft     bool            `json:"as_draft"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	m, err := s.loadEmail(ctx, a.MessageID)
	if err != nil {
		return nil, err
	}
	if a.AsDraft && a.SendAt != "" {
		return nil, userErr("send_at does not apply to a draft (as_draft); the user sends it from Gmail")
	}
	sendAt, err := s.parseSendAtArg(a.SendAt)
	if err != nil {
		return nil, err
	}
	files, err := s.resolveAttachments(ctx, a.Attachments)
	if err != nil {
		return nil, err
	}
	typ := "forward"
	if a.AsDraft {
		typ = "draft_forward"
	}
	req := ActionRequest{Type: typ, MessageID: m.ID, To: a.To, Cc: a.Cc, Bcc: a.Bcc, Body: a.Note, Attachments: files}
	p, err := req.toPayload()
	if err != nil {
		return nil, userErr("%v", err)
	}
	out, err := s.queueAction(ctx, typ, p, sendAt)
	if err != nil {
		return nil, err
	}
	fwd := map[string]any{"from": m.From, "subject": m.Subject, "date": localTime(m.InternalDate)}
	if files := attachmentList(m.Attachments, false, false); len(files) > 0 {
		fwd["attachments_included"] = files
	}
	out["forwarding"] = fwd
	return out, nil
}

// ---------------------------------------------------------------------------
// Existing drafts

func (s *Server) draftsNotSyncedHint(ctx context.Context) string {
	if t, err := s.store.LastDraftSync(ctx); err == nil && t == 0 {
		return "The Drafts folder has not been synced yet. It needs Code.gs 1.3.0 or later in Apps Script; after updating, drafts appear within a few minutes."
	}
	return ""
}

func (s *Server) describeDraft(ctx context.Context, d *DraftOut, bodyChars int, pending map[string]*Action) map[string]any {
	out := map[string]any{
		"draft_id": d.DraftID, "thread_id": d.ThreadID, "is_reply": d.InReplyTo != "",
		"gmail_url": s.draftURL(ctx, d.MessageID),
	}
	if !d.HasContent {
		out["note"] = "content not synced yet (new or just edited in Gmail); it arrives within about a minute"
	} else {
		out["to"], out["subject"], out["date"] = d.To, d.Subject, localTime(d.InternalDate)
		if d.Cc != "" {
			out["cc"] = d.Cc
		}
		if d.Bcc != "" {
			out["bcc"] = d.Bcc
		}
		body, clipped := clipRunes(d.Body, bodyChars)
		if bodyChars <= 300 {
			if clipped {
				body += "…"
			}
			out["preview"] = body
		} else {
			out["body"], out["body_truncated"] = body, clipped || d.BodyTruncated
			if d.BodyDerived {
				out["body_note"] = "converted from the HTML version"
			}
		}
		files := make([]map[string]any, 0, len(d.Attachments))
		for _, f := range d.Attachments {
			files = append(files, map[string]any{"filename": f.Filename, "mime_type": f.MimeType, "size_bytes": f.Size})
		}
		out["attachments"] = files
	}
	if a := pending[d.DraftID]; a != nil {
		out["queued_change"] = map[string]any{"action_id": a.ID, "type": a.Type, "status": a.Status}
	}
	return out
}

func (s *Server) toolListDrafts(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		Limit int `json:"limit"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	list, total, err := s.store.ListDrafts(ctx, clamp(a.Limit, 20, 1, 50))
	if err != nil {
		return nil, err
	}
	pending, err := s.store.PendingDraftActions(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(list))
	for _, d := range list {
		items = append(items, s.describeDraft(ctx, d, 200, pending))
	}
	out := map[string]any{"count": len(items), "total": total, "drafts": items}
	if hint := s.draftsNotSyncedHint(ctx); hint != "" {
		out["hint"] = hint
	}
	return out, nil
}

func (s *Server) loadDraft(ctx context.Context, id string) (*DraftOut, error) {
	if !gmailIDPattern.MatchString(id) {
		return nil, userErr("%q is not a Gmail draft id (use draft_id from list_drafts)", truncateForError(id))
	}
	d, err := s.store.GetDraft(ctx, id)
	if errors.Is(err, errNotFound) {
		msg := "no draft has id " + id + "; list_drafts shows the current drafts (a sent or deleted draft is gone)"
		if hint := s.draftsNotSyncedHint(ctx); hint != "" {
			msg += ". " + hint
		}
		return nil, userErr("%s", msg)
	}
	return d, err
}

// loadDraftContent is loadDraft for tools that need the draft's content.
func (s *Server) loadDraftContent(ctx context.Context, id string) (*DraftOut, error) {
	d, err := s.loadDraft(ctx, id)
	if err != nil {
		return nil, err
	}
	if !d.HasContent {
		return nil, userErr("draft %s was just created or edited in Gmail and its content has not synced yet; try again in a minute", id)
	}
	return d, nil
}

func (s *Server) toolGetDraft(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		DraftID  string `json:"draft_id"`
		MaxChars int    `json:"max_chars"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	d, err := s.loadDraft(ctx, a.DraftID)
	if err != nil {
		return nil, err
	}
	pending, err := s.store.PendingDraftActions(ctx)
	if err != nil {
		return nil, err
	}
	return s.describeDraft(ctx, d, clamp(a.MaxChars, 20000, 500, 100000), pending), nil
}

// headerAddresses splits a To/Cc/Bcc header into single addresses.
func headerAddresses(field, h string) ([]string, error) {
	if strings.TrimSpace(h) == "" {
		return nil, nil
	}
	list, err := mail.ParseAddressList(h)
	if err != nil {
		return nil, userErr("could not read the draft's %s addresses (%q); give %s explicitly", field, truncateForError(h), field)
	}
	out := make([]string, len(list))
	for i, a := range list {
		if a.Name == "" {
			out[i] = a.Address
		} else {
			out[i] = a.String()
		}
	}
	return out, nil
}

func (s *Server) toolUpdateDraft(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		DraftID           string          `json:"draft_id"`
		To                *addrList       `json:"to"`
		Cc                *addrList       `json:"cc"`
		Bcc               *addrList       `json:"bcc"`
		Subject           *string         `json:"subject"`
		Body              *string         `json:"body"`
		Attachments       []attachmentArg `json:"attachments"`
		RemoveAttachments bool            `json:"remove_attachments"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if a.To == nil && a.Cc == nil && a.Bcc == nil && a.Subject == nil && a.Body == nil && len(a.Attachments) == 0 && !a.RemoveAttachments {
		return nil, userErr("nothing to change: give to, cc, bcc, subject, body, attachments or remove_attachments")
	}
	d, err := s.loadDraftContent(ctx, a.DraftID)
	if err != nil {
		return nil, err
	}
	pick := func(field string, v *addrList, header string) ([]string, error) {
		if v != nil {
			return *v, nil
		}
		return headerAddresses(field, header)
	}
	req := ActionRequest{Type: "draft_update", DraftID: d.DraftID, DraftMessageID: d.MessageID,
		Subject: d.Subject, Body: d.Body, DropAttachments: a.RemoveAttachments}
	if req.To, err = pick("to", a.To, d.To); err != nil {
		return nil, err
	}
	if req.Cc, err = pick("cc", a.Cc, d.Cc); err != nil {
		return nil, err
	}
	if req.Bcc, err = pick("bcc", a.Bcc, d.Bcc); err != nil {
		return nil, err
	}
	if a.Subject != nil {
		req.Subject = *a.Subject
	}
	if a.Body != nil {
		req.Body = *a.Body
	} else if d.BodyTruncated {
		return nil, userErr("the draft's text is too long to keep unchanged here; give the full new body")
	}
	if req.Attachments, err = s.resolveAttachments(ctx, a.Attachments); err != nil {
		return nil, err
	}
	p, err := req.toPayload()
	if err != nil {
		return nil, userErr("%v", err)
	}
	out, err := s.queueAction(ctx, "draft_update", p, 0)
	if err != nil {
		return nil, err
	}
	if d.BodyDerived {
		out["formatting_note"] = "The draft had formatted (HTML) text; after this change it is plain text."
	}
	return out, nil
}

func (s *Server) toolSendDraft(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		DraftID string `json:"draft_id"`
		SendAt  string `json:"send_at"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	d, err := s.loadDraftContent(ctx, a.DraftID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.To+d.Cc+d.Bcc) == "" {
		return nil, userErr("draft %s has no recipients; add them with update_draft first", d.DraftID)
	}
	sendAt, err := s.parseSendAtArg(a.SendAt)
	if err != nil {
		return nil, err
	}
	req := ActionRequest{Type: "draft_send", DraftID: d.DraftID, DraftMessageID: d.MessageID}
	p, err := req.toPayload()
	if err != nil {
		return nil, userErr("%v", err)
	}
	out, err := s.queueAction(ctx, "draft_send", p, sendAt)
	if err != nil {
		return nil, err
	}
	out["draft"] = map[string]any{"to": d.To, "subject": d.Subject}
	return out, nil
}

func (s *Server) toolDeleteDraft(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		DraftID string `json:"draft_id"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	d, err := s.loadDraft(ctx, a.DraftID)
	if err != nil {
		return nil, err
	}
	req := ActionRequest{Type: "draft_delete", DraftID: d.DraftID}
	p, err := req.toPayload()
	if err != nil {
		return nil, userErr("%v", err)
	}
	out, err := s.queueAction(ctx, "draft_delete", p, 0)
	if err != nil {
		return nil, err
	}
	if d.HasContent {
		out["draft"] = map[string]any{"to": d.To, "subject": d.Subject, "date": localTime(d.InternalDate)}
	}
	return out, nil
}

// draftSyncAge is for mailbox_status.
func (s *Server) draftSyncAge(ctx context.Context) string {
	t, err := s.store.LastDraftSync(ctx)
	if err != nil || t == 0 {
		return "never (needs Code.gs 1.3.0+)"
	}
	return time.UnixMilli(t).In(time.Local).Format(time.RFC3339)
}
