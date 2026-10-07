package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits for data accepted from Apps Script. Apps Script truncates bodies well
// below these, so hitting one means a bug or a hostile client, not normal mail.
const (
	maxBatchMessages  = 100
	maxIDsPerRequest  = 1000
	maxHeaderField    = 32 << 10
	maxSnippet        = 4 << 10
	maxBodyText       = 1 << 20
	maxBodyHTML       = 2 << 20
	maxAttachments    = 100
	maxLabelsPerMsg   = 100
	maxLabelsTotal    = 2000
	maxAttachmentName = 1024
)

var (
	gmailIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	labelIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	partIDPattern  = regexp.MustCompile(`^[0-9A-Za-z.]{0,64}$`)
)

// MessageIn is one normalized Gmail message as sent by Apps Script.
type MessageIn struct {
	ID                string         `json:"id"`
	ThreadID          string         `json:"threadId"`
	HistoryID         string         `json:"historyId"`
	InternalDate      int64          `json:"internalDate"`
	LabelIDs          []string       `json:"labelIds"`
	Snippet           string         `json:"snippet"`
	SizeEstimate      int64          `json:"sizeEstimate"`
	From              string         `json:"from"`
	To                string         `json:"to"`
	Cc                string         `json:"cc"`
	Bcc               string         `json:"bcc"`
	ReplyTo           string         `json:"replyTo"`
	Subject           string         `json:"subject"`
	DateHeader        string         `json:"dateHeader"`
	MessageIDHeader   string         `json:"messageIdHeader"`
	InReplyTo         string         `json:"inReplyTo"`
	References        string         `json:"references"`
	BodyText          *string        `json:"bodyText"`
	BodyTextTruncated bool           `json:"bodyTextTruncated"`
	BodyHTML          *string        `json:"bodyHtml"`
	BodyHTMLTruncated bool           `json:"bodyHtmlTruncated"`
	Attachments       []AttachmentIn `json:"attachments"`
}

type AttachmentIn struct {
	PartID       string `json:"partId"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mimeType"`
	Size         int64  `json:"size"`
	AttachmentID string `json:"attachmentId"`
	ContentID    string `json:"contentId"`
	Inline       bool   `json:"inline"`
}

// StateUpdate carries label/read-state changes (or deletion) for a message
// Apps Script believes the backend already has.
type StateUpdate struct {
	ID       string   `json:"id"`
	LabelIDs []string `json:"labelIds"`
	Deleted  bool     `json:"deleted"`
}

type LabelIn struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func (m *MessageIn) validate() error {
	if !gmailIDPattern.MatchString(m.ID) {
		return errors.New("id: missing or invalid Gmail message id")
	}
	if !gmailIDPattern.MatchString(m.ThreadID) {
		return errors.New("threadId: missing or invalid Gmail thread id")
	}
	// Between 1990 and 2200, in milliseconds.
	if m.InternalDate < 631152000000 || m.InternalDate > 7258118400000 {
		return errors.New("internalDate: must be Unix milliseconds")
	}
	if len(m.HistoryID) > 32 {
		return errors.New("historyId: too long")
	}
	if err := validateLabelIDs(m.LabelIDs); err != nil {
		return err
	}
	if m.SizeEstimate < 0 {
		return errors.New("sizeEstimate: must not be negative")
	}
	for name, v := range map[string]string{
		"from": m.From, "to": m.To, "cc": m.Cc, "bcc": m.Bcc, "replyTo": m.ReplyTo,
		"subject": m.Subject, "dateHeader": m.DateHeader, "messageIdHeader": m.MessageIDHeader,
		"inReplyTo": m.InReplyTo, "references": m.References,
	} {
		if len(v) > maxHeaderField {
			return fmt.Errorf("%s: longer than %d bytes", name, maxHeaderField)
		}
	}
	if len(m.Snippet) > maxSnippet {
		return fmt.Errorf("snippet: longer than %d bytes", maxSnippet)
	}
	if m.BodyText != nil && len(*m.BodyText) > maxBodyText {
		return fmt.Errorf("bodyText: longer than %d bytes (truncate before sending)", maxBodyText)
	}
	if m.BodyHTML != nil && len(*m.BodyHTML) > maxBodyHTML {
		return fmt.Errorf("bodyHtml: longer than %d bytes (truncate before sending)", maxBodyHTML)
	}
	if len(m.Attachments) > maxAttachments {
		return fmt.Errorf("attachments: more than %d", maxAttachments)
	}
	seen := make(map[string]bool, len(m.Attachments))
	for i, a := range m.Attachments {
		if !partIDPattern.MatchString(a.PartID) {
			return fmt.Errorf("attachments[%d].partId: invalid", i)
		}
		key := a.PartID
		if key == "" {
			key = "#" + strconv.Itoa(i)
		}
		if seen[key] {
			return fmt.Errorf("attachments[%d].partId: duplicate %q", i, a.PartID)
		}
		seen[key] = true
		if len(a.Filename) > maxAttachmentName || len(a.MimeType) > 255 || len(a.ContentID) > 998 || len(a.AttachmentID) > 4096 {
			return fmt.Errorf("attachments[%d]: field too long", i)
		}
		if a.Size < 0 {
			return fmt.Errorf("attachments[%d].size: must not be negative", i)
		}
	}
	return nil
}

func validateLabelIDs(ids []string) error {
	if len(ids) > maxLabelsPerMsg {
		return fmt.Errorf("labelIds: more than %d", maxLabelsPerMsg)
	}
	for _, l := range ids {
		if !labelIDPattern.MatchString(l) {
			return fmt.Errorf("labelIds: invalid label id %q", truncateForError(l))
		}
	}
	return nil
}

func truncateForError(s string) string {
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return strings.ToValidUTF8(s, "?")
}

// canonicalLabels returns the sorted, de-duplicated label IDs as JSON so that
// label sets can be compared as strings.
func canonicalLabels(ids []string) string {
	set := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := set[id]; !ok {
			set[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Strings(out)
	b, _ := json.Marshal(out)
	return string(b)
}

type labelFlags struct {
	unread, starred, important, inbox, sent, trash, spam, draft int
}

func flagsFor(ids []string) labelFlags {
	var f labelFlags
	for _, id := range ids {
		switch id {
		case "UNREAD":
			f.unread = 1
		case "STARRED":
			f.starred = 1
		case "IMPORTANT":
			f.important = 1
		case "INBOX":
			f.inbox = 1
		case "SENT":
			f.sent = 1
		case "TRASH":
			f.trash = 1
		case "SPAM":
			f.spam = 1
		case "DRAFT":
			f.draft = 1
		}
	}
	return f
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ItemResult reports what happened to one message of a batch.
type ItemResult struct {
	ID     string `json:"id"`
	Status string `json:"status"` // inserted | updated | unchanged | rejected
	Error  string `json:"error,omitempty"`
}

type IngestSummary struct {
	Received  int          `json:"received"`
	Inserted  int          `json:"inserted"`
	Updated   int          `json:"updated"`
	Unchanged int          `json:"unchanged"`
	Rejected  int          `json:"rejected"`
	Results   []ItemResult `json:"results"`
}

// IngestMessages stores a batch in one transaction. The Gmail message ID is the
// primary key, so a repeated delivery never creates a second row: it only
// refreshes labels/read state (Gmail message content is immutable). Items that
// fail validation are reported individually and do not block the rest.
func (s *Store) IngestMessages(ctx context.Context, msgs []MessageIn, now int64) (IngestSummary, error) {
	sum := IngestSummary{Received: len(msgs), Results: make([]ItemResult, 0, len(msgs))}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sum, err
	}
	defer tx.Rollback()

	seenInBatch := make(map[string]bool, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		if err := m.validate(); err != nil {
			sum.Rejected++
			sum.Results = append(sum.Results, ItemResult{ID: truncateForError(m.ID), Status: "rejected", Error: err.Error()})
			continue
		}
		status, err := s.upsertMessage(ctx, tx, m, now)
		if err != nil {
			return sum, fmt.Errorf("message %s: %w", m.ID, err)
		}
		if seenInBatch[m.ID] && status == "unchanged" {
			status = "duplicate"
		}
		seenInBatch[m.ID] = true
		switch status {
		case "inserted":
			sum.Inserted++
		case "updated":
			sum.Updated++
		default:
			sum.Unchanged++
		}
		sum.Results = append(sum.Results, ItemResult{ID: m.ID, Status: status})
	}
	if err := setKVTx(ctx, tx, "last_ingest", strconv.Itoa(sum.Inserted), now); err != nil {
		return sum, err
	}
	return sum, tx.Commit()
}

func (s *Store) upsertMessage(ctx context.Context, tx *sql.Tx, m *MessageIn, now int64) (string, error) {
	labels := canonicalLabels(m.LabelIDs)
	f := flagsFor(m.LabelIDs)

	var existing string
	err := tx.QueryRowContext(ctx, `SELECT label_ids FROM messages WHERE gmail_id = ?`, m.ID).Scan(&existing)
	switch {
	case err == nil:
		if existing == labels {
			_, err = tx.ExecContext(ctx, `UPDATE messages SET last_synced_at = ? WHERE gmail_id = ?`, now, m.ID)
			return "unchanged", err
		}
		_, err = tx.ExecContext(ctx, `UPDATE messages SET
			label_ids = ?, is_unread = ?, is_starred = ?, is_important = ?, in_inbox = ?, in_sent = ?,
			in_trash = ?, in_spam = ?, is_draft = ?, history_id = ?, last_synced_at = ?, state_changed_at = ?
			WHERE gmail_id = ?`,
			labels, f.unread, f.starred, f.important, f.inbox, f.sent, f.trash, f.spam, f.draft,
			m.HistoryID, now, now, m.ID)
		return "updated", err
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}

	bodyText, derived := m.BodyText, false
	if (bodyText == nil || strings.TrimSpace(*bodyText) == "") && m.BodyHTML != nil && *m.BodyHTML != "" {
		t := truncateUTF8(htmlToText(*m.BodyHTML), maxBodyText)
		bodyText, derived = &t, true
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO messages (
		gmail_id, thread_id, history_id, rfc822_message_id, in_reply_to, references_hdr,
		from_addr, to_addrs, cc_addrs, bcc_addrs, reply_to, subject, date_header,
		internal_date, snippet, size_estimate,
		body_text, body_text_truncated, body_text_derived, body_html, body_html_truncated,
		label_ids, is_unread, is_starred, is_important, in_inbox, in_sent, in_trash, in_spam, is_draft,
		first_synced_at, last_synced_at, state_changed_at
	) VALUES (?,?,?,?,?,?, ?,?,?,?,?,?,?, ?,?,?, ?,?,?,?,?, ?,?,?,?,?,?,?,?,?, ?,?,?)`,
		m.ID, m.ThreadID, m.HistoryID, m.MessageIDHeader, m.InReplyTo, m.References,
		m.From, m.To, m.Cc, m.Bcc, m.ReplyTo, m.Subject, m.DateHeader,
		m.InternalDate, html.UnescapeString(m.Snippet), m.SizeEstimate,
		bodyText, boolInt(m.BodyTextTruncated), boolInt(derived), m.BodyHTML, boolInt(m.BodyHTMLTruncated),
		labels, f.unread, f.starred, f.important, f.inbox, f.sent, f.trash, f.spam, f.draft,
		now, now, now)
	if err != nil {
		return "", err
	}
	for i, a := range m.Attachments {
		partID := a.PartID
		if partID == "" {
			partID = "#" + strconv.Itoa(i)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO attachments
			(gmail_id, part_id, filename, mime_type, size, gmail_attachment_id, content_id, is_inline)
			VALUES (?,?,?,?,?,?,?,?)`,
			m.ID, partID, a.Filename, strings.ToLower(a.MimeType), a.Size, a.AttachmentID,
			strings.Trim(a.ContentID, "<> "), boolInt(a.Inline))
		if err != nil {
			return "", err
		}
	}
	return "inserted", nil
}

type StateSummary struct {
	Received  int          `json:"received"`
	Updated   int          `json:"updated"`
	Unchanged int          `json:"unchanged"`
	Rejected  int          `json:"rejected"`
	Unknown   []string     `json:"unknown"`
	Errors    []ItemResult `json:"errors,omitempty"`
}

// ApplyStateUpdates refreshes labels/flags of known messages. IDs the backend has
// never seen are returned in Unknown so Apps Script can fetch and ingest them.
func (s *Store) ApplyStateUpdates(ctx context.Context, ups []StateUpdate, now int64) (StateSummary, error) {
	sum := StateSummary{Received: len(ups), Unknown: []string{}}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sum, err
	}
	defer tx.Rollback()

	for _, u := range ups {
		if !gmailIDPattern.MatchString(u.ID) {
			sum.Rejected++
			sum.Errors = append(sum.Errors, ItemResult{ID: truncateForError(u.ID), Status: "rejected", Error: "id: invalid"})
			continue
		}
		if !u.Deleted && u.LabelIDs == nil {
			sum.Rejected++
			sum.Errors = append(sum.Errors, ItemResult{ID: u.ID, Status: "rejected", Error: "labelIds or deleted is required"})
			continue
		}
		if err := validateLabelIDs(u.LabelIDs); err != nil {
			sum.Rejected++
			sum.Errors = append(sum.Errors, ItemResult{ID: u.ID, Status: "rejected", Error: err.Error()})
			continue
		}
		var existing string
		var deleted int
		err := tx.QueryRowContext(ctx, `SELECT label_ids, gmail_deleted FROM messages WHERE gmail_id = ?`, u.ID).Scan(&existing, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			if !u.Deleted {
				sum.Unknown = append(sum.Unknown, u.ID)
			} else {
				sum.Unchanged++
			}
			continue
		}
		if err != nil {
			return sum, err
		}
		if u.Deleted {
			if deleted == 1 {
				sum.Unchanged++
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET gmail_deleted = 1, last_synced_at = ?, state_changed_at = ? WHERE gmail_id = ?`, now, now, u.ID); err != nil {
				return sum, err
			}
			sum.Updated++
			continue
		}
		labels := canonicalLabels(u.LabelIDs)
		if labels == existing {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET last_synced_at = ? WHERE gmail_id = ?`, now, u.ID); err != nil {
				return sum, err
			}
			sum.Unchanged++
			continue
		}
		f := flagsFor(u.LabelIDs)
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET
			label_ids = ?, is_unread = ?, is_starred = ?, is_important = ?, in_inbox = ?, in_sent = ?,
			in_trash = ?, in_spam = ?, is_draft = ?, last_synced_at = ?, state_changed_at = ?
			WHERE gmail_id = ?`,
			labels, f.unread, f.starred, f.important, f.inbox, f.sent, f.trash, f.spam, f.draft, now, now, u.ID); err != nil {
			return sum, err
		}
		sum.Updated++
	}
	if err := setKVTx(ctx, tx, "last_state_sync", strconv.Itoa(sum.Updated), now); err != nil {
		return sum, err
	}
	return sum, tx.Commit()
}

// UnknownIDs returns the subset of ids not stored yet, preserving input order.
func (s *Store) UnknownIDs(ctx context.Context, ids []string) ([]string, error) {
	known := make(map[string]bool, len(ids))
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		part := ids[start:end]
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		q := `SELECT gmail_id FROM messages WHERE gmail_id IN (?` + strings.Repeat(",?", len(part)-1) + `)`
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			known[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out := []string{}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !known[id] && !seen[id] {
			out = append(out, id)
			seen[id] = true
		}
	}
	return out, nil
}

// ReplaceLabels stores the mailbox's label list (ID → display name).
func (s *Store) ReplaceLabels(ctx context.Context, labels []LabelIn, now int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM labels`); err != nil {
		return 0, err
	}
	for _, l := range labels {
		t := strings.ToLower(l.Type)
		if t == "" {
			t = "user"
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO labels(label_id, name, type, updated_at) VALUES (?,?,?,?)`,
			l.ID, l.Name, t, now); err != nil {
			return 0, err
		}
	}
	return len(labels), tx.Commit()
}

func (s *Store) labelNames(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT label_id, name FROM labels`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		m[id] = name
	}
	return m, rows.Err()
}

// ---------------------------------------------------------------------------
// Reading

type SearchParams struct {
	Text      string // plain words, safely converted to an FTS5 query
	FTS       string // raw FTS5 query syntax
	From      string
	To        string // matches To or Cc
	Subject   string
	Label     string // label ID or display name
	ThreadID  string
	Unread    *bool
	Starred   *bool
	InInbox   *bool
	After     int64 // ms, inclusive
	Before    int64 // ms, exclusive
	Limit     int
	CursorTS  int64
	CursorID  string
	WithTrash bool
}

type MessageSummary struct {
	ID              string   `json:"id"`
	ThreadID        string   `json:"threadId"`
	From            string   `json:"from"`
	To              string   `json:"to"`
	Cc              string   `json:"cc,omitempty"`
	Subject         string   `json:"subject"`
	Date            string   `json:"date"`
	InternalDate    int64    `json:"internalDate"`
	Snippet         string   `json:"snippet"`
	LabelIDs        []string `json:"labelIds"`
	Unread          bool     `json:"unread"`
	Starred         bool     `json:"starred"`
	InInbox         bool     `json:"inInbox"`
	AttachmentCount int      `json:"attachmentCount"`
}

var errBadQuery = errors.New("bad query")

// ftsFromText turns free text into an FTS5 query of quoted terms (implicit AND),
// so user input can never be parsed as FTS5 operators. A trailing * on a word
// keeps prefix matching.
func ftsFromText(q string) string {
	var terms []string
	for _, w := range strings.Fields(q) {
		prefix := strings.HasSuffix(w, "*")
		w = strings.Trim(w, "*")
		w = strings.ReplaceAll(w, `"`, "")
		if w == "" {
			continue
		}
		if utf8.RuneCountInString(w) > 64 {
			w = string([]rune(w)[:64])
		}
		t := `"` + w + `"`
		if prefix {
			t += "*"
		}
		terms = append(terms, t)
		if len(terms) == 16 {
			break
		}
	}
	return strings.Join(terms, " ")
}

func likeArg(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

func (s *Store) Search(ctx context.Context, p SearchParams) ([]MessageSummary, string, error) {
	var where []string
	var args []any

	match := strings.TrimSpace(p.FTS)
	if match == "" && strings.TrimSpace(p.Text) != "" {
		match = ftsFromText(p.Text)
	}
	if match != "" {
		where = append(where, `m.rowid IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)`)
		args = append(args, match)
	}
	if p.From != "" {
		where = append(where, `m.from_addr LIKE ? ESCAPE '\'`)
		args = append(args, likeArg(p.From))
	}
	if p.To != "" {
		where = append(where, `(m.to_addrs LIKE ? ESCAPE '\' OR m.cc_addrs LIKE ? ESCAPE '\')`)
		args = append(args, likeArg(p.To), likeArg(p.To))
	}
	if p.Subject != "" {
		where = append(where, `m.subject LIKE ? ESCAPE '\'`)
		args = append(args, likeArg(p.Subject))
	}
	if p.Label != "" {
		where = append(where, `EXISTS (SELECT 1 FROM json_each(m.label_ids) j WHERE j.value = ?
			OR j.value IN (SELECT label_id FROM labels WHERE name = ? COLLATE NOCASE))`)
		args = append(args, p.Label, p.Label)
	}
	if p.ThreadID != "" {
		where = append(where, `m.thread_id = ?`)
		args = append(args, p.ThreadID)
	}
	for col, v := range map[string]*bool{"m.is_unread": p.Unread, "m.is_starred": p.Starred, "m.in_inbox": p.InInbox} {
		if v != nil {
			where = append(where, col+` = ?`)
			args = append(args, boolInt(*v))
		}
	}
	if !p.WithTrash {
		where = append(where, `m.in_trash = 0 AND m.in_spam = 0 AND m.gmail_deleted = 0`)
	}
	if p.After > 0 {
		where = append(where, `m.internal_date >= ?`)
		args = append(args, p.After)
	}
	if p.Before > 0 {
		where = append(where, `m.internal_date < ?`)
		args = append(args, p.Before)
	}
	if p.CursorID != "" {
		where = append(where, `(m.internal_date < ? OR (m.internal_date = ? AND m.gmail_id < ?))`)
		args = append(args, p.CursorTS, p.CursorTS, p.CursorID)
	}

	q := `SELECT m.gmail_id, m.thread_id, m.from_addr, m.to_addrs, m.cc_addrs, m.subject, m.internal_date,
		m.snippet, m.label_ids, m.is_unread, m.is_starred, m.in_inbox,
		(SELECT count(*) FROM attachments a WHERE a.gmail_id = m.gmail_id AND a.is_inline = 0)
		FROM messages m`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += ` ORDER BY m.internal_date DESC, m.gmail_id DESC LIMIT ?`
	args = append(args, p.Limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		if isFTSSyntaxError(err) {
			return nil, "", fmt.Errorf("%w: %v", errBadQuery, err)
		}
		return nil, "", err
	}
	defer rows.Close()
	out := []MessageSummary{}
	for rows.Next() {
		var m MessageSummary
		var labels string
		var unread, starred, inbox int
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.From, &m.To, &m.Cc, &m.Subject, &m.InternalDate,
			&m.Snippet, &labels, &unread, &starred, &inbox, &m.AttachmentCount); err != nil {
			return nil, "", err
		}
		m.Date = msToRFC3339(m.InternalDate)
		m.LabelIDs = decodeLabels(labels)
		m.Unread, m.Starred, m.InInbox = unread == 1, starred == 1, inbox == 1
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		if isFTSSyntaxError(err) {
			return nil, "", fmt.Errorf("%w: %v", errBadQuery, err)
		}
		return nil, "", err
	}
	next := ""
	if len(out) > p.Limit {
		out = out[:p.Limit]
		last := out[len(out)-1]
		next = strconv.FormatInt(last.InternalDate, 10) + ":" + last.ID
	}
	return out, next, nil
}

func isFTSSyntaxError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "fts5") || strings.Contains(s, "syntax error") ||
		strings.Contains(s, "no such column") || strings.Contains(s, "unterminated string")
}

func decodeLabels(s string) []string {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

func msToRFC3339(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

type AttachmentOut struct {
	PartID       string `json:"partId"`
	Filename     string `json:"filename"`
	MimeType     string `json:"mimeType"`
	Size         int64  `json:"size"`
	AttachmentID string `json:"attachmentId,omitempty"`
	ContentID    string `json:"contentId,omitempty"`
	Inline       bool   `json:"inline"`
	FetchState   string `json:"fetchState"`
}

type MessageOut struct {
	ID                string          `json:"id"`
	ThreadID          string          `json:"threadId"`
	HistoryID         string          `json:"historyId,omitempty"`
	From              string          `json:"from"`
	To                string          `json:"to"`
	Cc                string          `json:"cc"`
	Bcc               string          `json:"bcc"`
	ReplyTo           string          `json:"replyTo"`
	Subject           string          `json:"subject"`
	Date              string          `json:"date"`
	InternalDate      int64           `json:"internalDate"`
	DateHeader        string          `json:"dateHeader"`
	MessageIDHeader   string          `json:"messageIdHeader"`
	InReplyTo         string          `json:"inReplyTo"`
	References        string          `json:"references"`
	Snippet           string          `json:"snippet"`
	SizeEstimate      int64           `json:"sizeEstimate"`
	LabelIDs          []string        `json:"labelIds"`
	Labels            []string        `json:"labels"`
	Unread            bool            `json:"unread"`
	Starred           bool            `json:"starred"`
	Important         bool            `json:"important"`
	InInbox           bool            `json:"inInbox"`
	InSent            bool            `json:"inSent"`
	InTrash           bool            `json:"inTrash"`
	InSpam            bool            `json:"inSpam"`
	Draft             bool            `json:"draft"`
	DeletedInGmail    bool            `json:"deletedInGmail"`
	BodyText          *string         `json:"bodyText"`
	BodyTextTruncated bool            `json:"bodyTextTruncated"`
	BodyTextDerived   bool            `json:"bodyTextDerived"`
	BodyHTML          *string         `json:"bodyHtml,omitempty"`
	BodyHTMLTruncated bool            `json:"bodyHtmlTruncated"`
	HasHTML           bool            `json:"hasHtml"`
	Attachments       []AttachmentOut `json:"attachments"`
	FirstSyncedAt     string          `json:"firstSyncedAt"`
	LastSyncedAt      string          `json:"lastSyncedAt"`
}

var errNotFound = errors.New("not found")

const messageColumns = `gmail_id, thread_id, history_id, from_addr, to_addrs, cc_addrs, bcc_addrs, reply_to,
	subject, internal_date, date_header, rfc822_message_id, in_reply_to, references_hdr, snippet, size_estimate,
	label_ids, is_unread, is_starred, is_important, in_inbox, in_sent, in_trash, in_spam, is_draft, gmail_deleted,
	body_text, body_text_truncated, body_text_derived, body_html IS NOT NULL AND body_html != '', body_html_truncated,
	first_synced_at, last_synced_at`

func scanMessage(sc interface{ Scan(...any) error }) (*MessageOut, error) {
	var m MessageOut
	var labels string
	var unread, starred, important, inbox, sent, trash, spam, draft, deleted, textTrunc, textDerived, hasHTML, htmlTrunc int
	var first, last int64
	var body sql.NullString
	err := sc.Scan(&m.ID, &m.ThreadID, &m.HistoryID, &m.From, &m.To, &m.Cc, &m.Bcc, &m.ReplyTo,
		&m.Subject, &m.InternalDate, &m.DateHeader, &m.MessageIDHeader, &m.InReplyTo, &m.References, &m.Snippet, &m.SizeEstimate,
		&labels, &unread, &starred, &important, &inbox, &sent, &trash, &spam, &draft, &deleted,
		&body, &textTrunc, &textDerived, &hasHTML, &htmlTrunc, &first, &last)
	if err != nil {
		return nil, err
	}
	if body.Valid {
		m.BodyText = &body.String
	}
	m.Date = msToRFC3339(m.InternalDate)
	m.LabelIDs = decodeLabels(labels)
	m.Unread, m.Starred, m.Important, m.InInbox = unread == 1, starred == 1, important == 1, inbox == 1
	m.InSent, m.InTrash, m.InSpam, m.Draft, m.DeletedInGmail = sent == 1, trash == 1, spam == 1, draft == 1, deleted == 1
	m.BodyTextTruncated, m.BodyTextDerived, m.HasHTML, m.BodyHTMLTruncated = textTrunc == 1, textDerived == 1, hasHTML == 1, htmlTrunc == 1
	m.FirstSyncedAt, m.LastSyncedAt = msToRFC3339(first), msToRFC3339(last)
	m.Attachments = []AttachmentOut{}
	return &m, nil
}

func (s *Store) attachmentsFor(ctx context.Context, ids []string) (map[string][]AttachmentOut, error) {
	out := map[string][]AttachmentOut{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT gmail_id, part_id, filename, mime_type, size, gmail_attachment_id,
		content_id, is_inline, fetch_state FROM attachments WHERE gmail_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)
		ORDER BY gmail_id, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var gid string
		var a AttachmentOut
		var inline int
		if err := rows.Scan(&gid, &a.PartID, &a.Filename, &a.MimeType, &a.Size, &a.AttachmentID, &a.ContentID, &inline, &a.FetchState); err != nil {
			return nil, err
		}
		a.Inline = inline == 1
		out[gid] = append(out[gid], a)
	}
	return out, rows.Err()
}

// GetMessage returns one stored message. The HTML body is only loaded on request.
func (s *Store) GetMessage(ctx context.Context, id string, withHTML bool) (*MessageOut, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE gmail_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if withHTML && m.HasHTML {
		var h string
		if err := s.db.QueryRowContext(ctx, `SELECT body_html FROM messages WHERE gmail_id = ?`, id).Scan(&h); err != nil {
			return nil, err
		}
		m.BodyHTML = &h
	}
	atts, err := s.attachmentsFor(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	if a := atts[id]; a != nil {
		m.Attachments = a
	}
	if err := s.fillLabelNames(ctx, []*MessageOut{m}); err != nil {
		return nil, err
	}
	return m, nil
}

// GetThread returns up to limit messages of a thread, oldest first, with text
// bodies cut to bodyChars characters so responses stay bounded.
func (s *Store) GetThread(ctx context.Context, threadID string, limit, bodyChars int) ([]*MessageOut, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE thread_id = ?
		ORDER BY internal_date ASC, gmail_id ASC LIMIT ?`, threadID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []*MessageOut
	var ids []string
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		if m.BodyText != nil && utf8.RuneCountInString(*m.BodyText) > bodyChars {
			t := string([]rune(*m.BodyText)[:bodyChars])
			m.BodyText = &t
			m.BodyTextTruncated = true
		}
		msgs = append(msgs, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, errNotFound
	}
	atts, err := s.attachmentsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if a := atts[m.ID]; a != nil {
			m.Attachments = a
		}
	}
	return msgs, s.fillLabelNames(ctx, msgs)
}

func (s *Store) fillLabelNames(ctx context.Context, msgs []*MessageOut) error {
	names, err := s.labelNames(ctx)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		m.Labels = make([]string, 0, len(m.LabelIDs))
		for _, id := range m.LabelIDs {
			if n, ok := names[id]; ok {
				m.Labels = append(m.Labels, n)
			} else {
				m.Labels = append(m.Labels, id)
			}
		}
	}
	return nil
}

type Stats struct {
	Messages        int64            `json:"messages"`
	Threads         int64            `json:"threads"`
	Unread          int64            `json:"unread"`
	Attachments     int64            `json:"attachments"`
	Labels          int64            `json:"labels"`
	OldestMessage   string           `json:"oldestMessage,omitempty"`
	NewestMessage   string           `json:"newestMessage,omitempty"`
	Actions         map[string]int64 `json:"actions"`
	LastIngestAt    string           `json:"lastIngestAt,omitempty"`
	LastStateSyncAt string           `json:"lastStateSyncAt,omitempty"`
	LastPollAt      string           `json:"lastPollAt,omitempty"`
	DBBytes         int64            `json:"dbBytes"`
}

func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	st := &Stats{Actions: map[string]int64{}}
	var oldest, newest sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT count(*), count(DISTINCT thread_id), coalesce(sum(is_unread), 0),
		min(internal_date), max(internal_date) FROM messages`).Scan(&st.Messages, &st.Threads, &st.Unread, &oldest, &newest)
	if err != nil {
		return nil, err
	}
	if oldest.Valid {
		st.OldestMessage, st.NewestMessage = msToRFC3339(oldest.Int64), msToRFC3339(newest.Int64)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM attachments`).Scan(&st.Attachments); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM labels`).Scan(&st.Labels); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT status, count(*) FROM actions GROUP BY status`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return nil, err
		}
		st.Actions[k] = n
	}
	rows.Close()
	for key, dst := range map[string]*string{"last_ingest": &st.LastIngestAt, "last_state_sync": &st.LastStateSyncAt, "last_poll": &st.LastPollAt} {
		t, err := s.getKVTime(ctx, key)
		if err != nil {
			return nil, err
		}
		if t > 0 {
			*dst = msToRFC3339(t)
		}
	}
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(s.path + suffix); err == nil {
			st.DBBytes += fi.Size()
		}
	}
	return st, nil
}

func setKVTx(ctx context.Context, tx *sql.Tx, key, value string, now int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO kv(key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, now)
	return err
}
