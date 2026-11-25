package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Gmail's Drafts folder is mirrored in the drafts table. Apps Script lists the
// drafts (ID and current message ID) and posts the whole list; the backend drops
// drafts that are gone and asks for the content of new or edited ones (their
// message ID changes on every edit), which Apps Script sends in a second call.

const (
	maxDrafts            = 500
	maxDraftMessagesPost = 20
)

type DraftRef struct {
	ID        string `json:"id"`
	MessageID string `json:"messageId"`
	ThreadID  string `json:"threadId"`
}

type DraftSyncRequest struct {
	Drafts   []DraftRef  `json:"drafts"`
	Messages []MessageIn `json:"messages"`
}

type DraftSyncSummary struct {
	Drafts  int      `json:"drafts"`
	Stored  int      `json:"stored"`
	Removed int      `json:"removed"`
	Need    []string `json:"need"` // draft IDs whose content the backend does not have
}

type DraftAttachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}

type DraftOut struct {
	DraftID       string            `json:"draftId"`
	MessageID     string            `json:"messageId"`
	ThreadID      string            `json:"threadId"`
	HasContent    bool              `json:"hasContent"`
	To            string            `json:"to"`
	Cc            string            `json:"cc"`
	Bcc           string            `json:"bcc"`
	Subject       string            `json:"subject"`
	InReplyTo     string            `json:"inReplyTo"`
	InternalDate  int64             `json:"internalDate"`
	Body          string            `json:"body"`
	BodyTruncated bool              `json:"bodyTruncated"`
	BodyDerived   bool              `json:"bodyDerived"`
	Attachments   []DraftAttachment `json:"attachments"`
}

func (r *DraftSyncRequest) validate() error {
	if len(r.Drafts) > maxDrafts {
		return fmt.Errorf("drafts: more than %d", maxDrafts)
	}
	if len(r.Messages) > maxDraftMessagesPost {
		return fmt.Errorf("messages: more than %d per request", maxDraftMessagesPost)
	}
	seen := map[string]bool{}
	for i, d := range r.Drafts {
		if !gmailIDPattern.MatchString(d.ID) || !gmailIDPattern.MatchString(d.MessageID) ||
			(d.ThreadID != "" && !gmailIDPattern.MatchString(d.ThreadID)) {
			return fmt.Errorf("drafts[%d]: invalid id", i)
		}
		if seen[d.ID] {
			return fmt.Errorf("drafts[%d]: duplicate draft id", i)
		}
		seen[d.ID] = true
	}
	for i := range r.Messages {
		if err := r.Messages[i].validate(); err != nil {
			return fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	return nil
}

// draftAttachments keeps the files a person attached, leaving out inline images.
func draftAttachments(atts []AttachmentIn) string {
	out := []DraftAttachment{}
	for _, a := range atts {
		mime := strings.ToLower(a.MimeType)
		if strings.HasPrefix(mime, "image/") && a.ContentID != "" && (a.Inline || a.Filename == "") {
			continue
		}
		out = append(out, DraftAttachment{Filename: a.Filename, MimeType: mime, Size: a.Size})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// SyncDrafts replaces the drafts table with Apps Script's list. Content arrives
// in msgs (keyed by message ID) for drafts the backend asked for.
func (s *Store) SyncDrafts(ctx context.Context, req DraftSyncRequest, now int64) (DraftSyncSummary, error) {
	sum := DraftSyncSummary{Drafts: len(req.Drafts), Need: []string{}}
	content := make(map[string]*MessageIn, len(req.Messages))
	for i := range req.Messages {
		content[req.Messages[i].ID] = &req.Messages[i]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sum, err
	}
	defer tx.Rollback()

	keep := make(map[string]bool, len(req.Drafts))
	for _, d := range req.Drafts {
		keep[d.ID] = true
	}
	existing, err := queryStrings(ctx, tx, `SELECT draft_id FROM drafts`)
	if err != nil {
		return sum, err
	}
	for _, id := range existing {
		if !keep[id] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM drafts WHERE draft_id = ?`, id); err != nil {
				return sum, err
			}
			sum.Removed++
		}
	}

	for _, d := range req.Drafts {
		if m := content[d.MessageID]; m != nil {
			body, derived := m.BodyText, false
			if (body == nil || strings.TrimSpace(*body) == "") && m.BodyHTML != nil && *m.BodyHTML != "" {
				t := truncateUTF8(htmlToText(*m.BodyHTML), maxBodyText)
				body, derived = &t, true
			}
			threadID := d.ThreadID
			if threadID == "" {
				threadID = m.ThreadID
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO drafts (draft_id, message_id, thread_id, has_content,
				to_addrs, cc_addrs, bcc_addrs, subject, in_reply_to, internal_date,
				body_text, body_text_truncated, body_text_derived, attachments, synced_at)
				VALUES (?,?,?,1, ?,?,?,?,?,?, ?,?,?,?,?)
				ON CONFLICT(draft_id) DO UPDATE SET message_id = excluded.message_id, thread_id = excluded.thread_id,
				has_content = 1, to_addrs = excluded.to_addrs, cc_addrs = excluded.cc_addrs, bcc_addrs = excluded.bcc_addrs,
				subject = excluded.subject, in_reply_to = excluded.in_reply_to, internal_date = excluded.internal_date,
				body_text = excluded.body_text, body_text_truncated = excluded.body_text_truncated,
				body_text_derived = excluded.body_text_derived, attachments = excluded.attachments, synced_at = excluded.synced_at`,
				d.ID, d.MessageID, threadID, m.To, m.Cc, m.Bcc, m.Subject, m.InReplyTo, m.InternalDate,
				body, boolInt(m.BodyTextTruncated), boolInt(derived), draftAttachments(m.Attachments), now); err != nil {
				return sum, err
			}
			sum.Stored++
			continue
		}
		var msgID string
		var has int
		err := tx.QueryRowContext(ctx, `SELECT message_id, has_content FROM drafts WHERE draft_id = ?`, d.ID).Scan(&msgID, &has)
		switch {
		case err == nil && msgID == d.MessageID:
			if _, err := tx.ExecContext(ctx, `UPDATE drafts SET synced_at = ? WHERE draft_id = ?`, now, d.ID); err != nil {
				return sum, err
			}
			if has == 0 {
				sum.Need = append(sum.Need, d.ID)
			}
			continue
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return sum, err
		}
		// New, or edited in Gmail since: forget the old content until the new arrives.
		if _, err := tx.ExecContext(ctx, `INSERT INTO drafts (draft_id, message_id, thread_id, has_content, synced_at)
			VALUES (?,?,?,0,?)
			ON CONFLICT(draft_id) DO UPDATE SET message_id = excluded.message_id, thread_id = excluded.thread_id,
			has_content = 0, to_addrs = '', cc_addrs = '', bcc_addrs = '', subject = '', in_reply_to = '',
			internal_date = 0, body_text = NULL, body_text_truncated = 0, body_text_derived = 0, attachments = '[]',
			synced_at = excluded.synced_at`, d.ID, d.MessageID, d.ThreadID, now); err != nil {
			return sum, err
		}
		sum.Need = append(sum.Need, d.ID)
	}
	if err := setKVTx(ctx, tx, "last_drafts_sync", fmt.Sprint(len(req.Drafts)), now); err != nil {
		return sum, err
	}
	return sum, tx.Commit()
}

const draftColumns = `draft_id, message_id, thread_id, has_content, to_addrs, cc_addrs, bcc_addrs, subject,
	in_reply_to, internal_date, coalesce(body_text, ''), body_text_truncated, body_text_derived, attachments`

func scanDraft(sc interface{ Scan(...any) error }) (*DraftOut, error) {
	var d DraftOut
	var has, trunc, derived int
	var atts string
	if err := sc.Scan(&d.DraftID, &d.MessageID, &d.ThreadID, &has, &d.To, &d.Cc, &d.Bcc, &d.Subject,
		&d.InReplyTo, &d.InternalDate, &d.Body, &trunc, &derived, &atts); err != nil {
		return nil, err
	}
	d.HasContent, d.BodyTruncated, d.BodyDerived = has == 1, trunc == 1, derived == 1
	if json.Unmarshal([]byte(atts), &d.Attachments) != nil || d.Attachments == nil {
		d.Attachments = []DraftAttachment{}
	}
	return &d, nil
}

// ListDrafts returns drafts newest first (drafts whose content has not arrived yet last).
func (s *Store) ListDrafts(ctx context.Context, limit int) ([]*DraftOut, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM drafts`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+draftColumns+` FROM drafts
		ORDER BY internal_date DESC, draft_id LIMIT ?`, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*DraftOut{}
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

func (s *Store) GetDraft(ctx context.Context, id string) (*DraftOut, error) {
	d, err := scanDraft(s.db.QueryRowContext(ctx, `SELECT `+draftColumns+` FROM drafts WHERE draft_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return d, err
}

// LastDraftSync is when Apps Script last reported the Drafts folder (0 = never).
func (s *Store) LastDraftSync(ctx context.Context) (int64, error) {
	return s.getKVTime(ctx, "last_drafts_sync")
}

// PendingDraftActions maps draft IDs to queued (pending or claimed) actions on them.
func (s *Store) PendingDraftActions(ctx context.Context) (map[string]*Action, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+actionColumns+` FROM actions
		WHERE status IN ('pending', 'claimed') AND type IN ('draft_update', 'draft_delete', 'draft_send')
		ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*Action{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		var p ActionPayload
		if json.Unmarshal(a.Payload, &p) == nil && p.DraftID != "" {
			out[p.DraftID] = a
		}
	}
	return out, rows.Err()
}
