package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Outbound actions are queued here and executed by Apps Script, which is the only
// component holding Gmail credentials. Lifecycle:
//
//	pending → claimed (poll) → done (complete)
//	                         → pending again with backoff (retryable failure)
//	                         → failed (permanent failure or attempts exhausted)
//	pending → cancelled (local client)
//
// A claim is a lease. If Apps Script never reports, the lease expires and the
// action becomes pending again; Apps Script keeps its own journal so it never
// performs the same action twice (see apps-script/Code.gs).

const (
	maxRecipients     = 50
	maxActionSubject  = 500
	maxActionBody     = 1 << 20
	maxActionHTMLBody = 2 << 20
	maxIdemKey        = 200
	maxErrorText      = 2000
)

var (
	errConflict   = errors.New("conflict")
	errStaleClaim = errors.New("stale claim")
)

type ActionRequest struct {
	Type           string   `json:"type"`
	To             []string `json:"to"`
	Cc             []string `json:"cc"`
	Bcc            []string `json:"bcc"`
	Subject        string   `json:"subject"`
	Body           string   `json:"body"`
	HTMLBody       string   `json:"htmlBody"`
	MessageID      string   `json:"messageId"`
	ThreadID       string   `json:"threadId"`
	ReplyAll       bool     `json:"replyAll"`
	IdempotencyKey string   `json:"idempotencyKey"`
	// SendAt (RFC 3339) holds a send, reply, forward or draft_send until then.
	SendAt      string          `json:"sendAt"`
	Attachments []AttachmentRef `json:"attachments"`
	// modify
	MessageIDs     []string `json:"messageIds"`
	ThreadIDs      []string `json:"threadIds"`
	AddLabelIDs    []string `json:"addLabelIds"`
	RemoveLabelIDs []string `json:"removeLabelIds"`
	Trash          bool     `json:"trash"`
	Untrash        bool     `json:"untrash"`
	// draft_update, draft_delete, draft_send
	DraftID         string `json:"draftId"`
	DraftMessageID  string `json:"draftMessageId"`
	DropAttachments bool   `json:"dropAttachments"`
}

// AttachmentRef names a file attached to a synced email (attachmentId is the
// attachment_id the tools show: Gmail's MIME part ID or "#<position>"). Apps
// Script downloads it from Gmail and attaches it to the outgoing email.
type AttachmentRef struct {
	MessageID    string `json:"messageId"`
	AttachmentID string `json:"attachmentId"`
	Filename     string `json:"filename,omitempty"` // for display only
	Size         int64  `json:"size,omitempty"`     // for display only
}

// ActionPayload is what Apps Script receives. Field names match Code.gs.
type ActionPayload struct {
	To          []string        `json:"to,omitempty"`
	Cc          []string        `json:"cc,omitempty"`
	Bcc         []string        `json:"bcc,omitempty"`
	Subject     string          `json:"subject,omitempty"`
	Body        string          `json:"body"`
	HTMLBody    string          `json:"htmlBody,omitempty"`
	MessageID   string          `json:"messageId,omitempty"`
	ThreadID    string          `json:"threadId,omitempty"`
	ReplyAll    bool            `json:"replyAll,omitempty"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`

	MessageIDs     []string `json:"messageIds,omitempty"`
	ThreadIDs      []string `json:"threadIds,omitempty"`
	AddLabelIDs    []string `json:"addLabelIds,omitempty"`
	RemoveLabelIDs []string `json:"removeLabelIds,omitempty"`
	Trash          bool     `json:"trash,omitempty"`
	Untrash        bool     `json:"untrash,omitempty"`

	DraftID         string `json:"draftId,omitempty"`
	DraftMessageID  string `json:"draftMessageId,omitempty"`
	DropAttachments bool   `json:"dropAttachments,omitempty"`
}

// ActionResult is what Apps Script did: the Gmail message it sent or saved
// (DraftID for draft actions), or for "modify" how many emails or conversations
// it changed and which IDs Gmail no longer has.
type ActionResult struct {
	MessageID string   `json:"messageId,omitempty"`
	ThreadID  string   `json:"threadId,omitempty"`
	DraftID   string   `json:"draftId,omitempty"`
	Modified  int      `json:"modified,omitempty"`
	Missing   []string `json:"missing,omitempty"`
}

// isDraftAction reports whether an action type saves a draft instead of sending.
func isDraftAction(typ string) bool {
	return typ == "draft" || typ == "draft_reply" || typ == "draft_forward" || typ == "draft_update"
}

// sendsMail reports whether performing an action type sends an email.
func sendsMail(typ string) bool {
	return typ == "send" || typ == "reply" || typ == "forward" || typ == "draft_send"
}

const (
	maxModifyTargets     = 100      // emails plus conversations in one modify action
	maxOutgoingFiles     = 10       // attachments on one outgoing email
	maxOutgoingFileBytes = 20 << 20 // their total size (Gmail's limit is 25 MB per email, encoded)
)

// actionFields lists the request fields each action type accepts.
var actionFields = map[string]string{
	"send":          "to cc bcc subject body htmlBody attachments",
	"draft":         "to cc bcc subject body htmlBody attachments",
	"reply":         "messageId threadId replyAll cc bcc body htmlBody attachments",
	"draft_reply":   "messageId threadId replyAll cc bcc body htmlBody attachments",
	"forward":       "messageId to cc bcc body attachments",
	"draft_forward": "messageId to cc bcc body attachments",
	"modify":        "messageIds threadIds addLabelIds removeLabelIds trash untrash",
	"draft_update":  "draftId draftMessageId to cc bcc subject body htmlBody attachments dropAttachments",
	"draft_delete":  "draftId draftMessageId",
	"draft_send":    "draftId draftMessageId",
}

// actionTypeNames is the list of valid types for error messages.
const actionTypeNames = `"send", "reply", "forward", "draft", "draft_reply", "draft_forward", "modify", "draft_update", "draft_delete" or "draft_send"`

// labelsNotModifiable are system labels a modify action may not add or remove:
// Gmail sets them itself (SENT, DRAFT, CHAT), or trash/untrash handle them.
var labelsNotModifiable = map[string]bool{"SENT": true, "DRAFT": true, "CHAT": true, "TRASH": true, "SPAM": true}

type Action struct {
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	Status         string          `json:"status"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"maxAttempts"`
	NotBefore      string          `json:"notBefore"`
	LeaseUntil     string          `json:"leaseUntil,omitempty"`
	LastError      string          `json:"lastError,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	CreatedAt      string          `json:"createdAt"`
	UpdatedAt      string          `json:"updatedAt"`
	FinishedAt     string          `json:"finishedAt,omitempty"`
}

type ClaimedAction struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	ClaimID    string          `json:"claimId"`
	Attempt    int             `json:"attempt"`
	LeaseUntil string          `json:"leaseUntil"`
	Payload    json.RawMessage `json:"payload"`
}

func validateAddressList(field string, list []string) error {
	for i, a := range list {
		if a == "" || len(a) > 320 || strings.ContainsAny(a, "\r\n\x00,;") {
			return fmt.Errorf("%s[%d]: must be a single email address (no commas, semicolons or line breaks)", field, i)
		}
		addr, err := mail.ParseAddress(a)
		if err != nil || !strings.Contains(addr.Address, "@") {
			return fmt.Errorf("%s[%d]: invalid email address", field, i)
		}
	}
	return nil
}

func hasControl(s string) bool {
	return strings.ContainsAny(s, "\r\n\x00")
}

// setFields names the request fields that were given.
func (r *ActionRequest) setFields() []string {
	var out []string
	add := func(name string, set bool) {
		if set {
			out = append(out, name)
		}
	}
	add("to", len(r.To) > 0)
	add("cc", len(r.Cc) > 0)
	add("bcc", len(r.Bcc) > 0)
	add("subject", r.Subject != "")
	add("body", r.Body != "")
	add("htmlBody", r.HTMLBody != "")
	add("messageId", r.MessageID != "")
	add("threadId", r.ThreadID != "")
	add("replyAll", r.ReplyAll)
	add("attachments", len(r.Attachments) > 0)
	add("messageIds", len(r.MessageIDs) > 0)
	add("threadIds", len(r.ThreadIDs) > 0)
	add("addLabelIds", len(r.AddLabelIDs) > 0)
	add("removeLabelIds", len(r.RemoveLabelIDs) > 0)
	add("trash", r.Trash)
	add("untrash", r.Untrash)
	add("draftId", r.DraftID != "")
	add("draftMessageId", r.DraftMessageID != "")
	add("dropAttachments", r.DropAttachments)
	return out
}

func validateIDs(field string, ids []string, pattern *regexp.Regexp) error {
	for i, id := range ids {
		if !pattern.MatchString(id) {
			return fmt.Errorf("%s[%d]: invalid id %q", field, i, truncateForError(id))
		}
	}
	return nil
}

// dedupe returns ids without repeats, in order.
func dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func validateAttachmentRefs(refs []AttachmentRef) error {
	if len(refs) > maxOutgoingFiles {
		return fmt.Errorf("attachments: at most %d files", maxOutgoingFiles)
	}
	seen := map[string]bool{}
	for i, a := range refs {
		if !gmailIDPattern.MatchString(a.MessageID) {
			return fmt.Errorf("attachments[%d].messageId: invalid Gmail message id", i)
		}
		if a.AttachmentID == "" || len(a.AttachmentID) > 65 || !attachmentRefPattern.MatchString(a.AttachmentID) {
			return fmt.Errorf("attachments[%d].attachmentId: invalid", i)
		}
		key := a.MessageID + "/" + a.AttachmentID
		if seen[key] {
			return fmt.Errorf("attachments[%d]: listed twice", i)
		}
		seen[key] = true
		if len(a.Filename) > maxAttachmentName || hasControl(a.Filename) || a.Size < 0 {
			return fmt.Errorf("attachments[%d]: invalid filename or size", i)
		}
	}
	return nil
}

// attachmentRefPattern matches an attachment_id: a MIME part ID or "#<position>".
var attachmentRefPattern = regexp.MustCompile(`^(#[0-9]{1,3}|[0-9A-Za-z.]{1,64})$`)

// parseSendAt reads an RFC 3339 time for a scheduled send.
func parseSendAt(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, errors.New("sendAt: use RFC 3339, e.g. 2026-11-24T07:30:00-05:00")
	}
	return t.UnixMilli(), nil
}

// toPayload validates a request and returns the normalized payload.
func (r *ActionRequest) toPayload() (ActionPayload, error) {
	p := ActionPayload{
		To: r.To, Cc: r.Cc, Bcc: r.Bcc, Subject: r.Subject, Body: r.Body, HTMLBody: r.HTMLBody,
		MessageID: r.MessageID, ThreadID: r.ThreadID, ReplyAll: r.ReplyAll, Attachments: r.Attachments,
		MessageIDs: dedupe(r.MessageIDs), ThreadIDs: dedupe(r.ThreadIDs),
		AddLabelIDs: dedupe(r.AddLabelIDs), RemoveLabelIDs: dedupe(r.RemoveLabelIDs), Trash: r.Trash, Untrash: r.Untrash,
		DraftID: r.DraftID, DraftMessageID: r.DraftMessageID, DropAttachments: r.DropAttachments,
	}
	if len(r.IdempotencyKey) > maxIdemKey || hasControl(r.IdempotencyKey) {
		return p, fmt.Errorf("idempotencyKey: at most %d characters, no line breaks", maxIdemKey)
	}
	allowed, ok := actionFields[r.Type]
	if !ok {
		return p, errors.New("type must be " + actionTypeNames)
	}
	allowedSet := strings.Fields(allowed)
	for _, f := range r.setFields() {
		if !slices.Contains(allowedSet, f) {
			if (r.Type == "reply" || r.Type == "draft_reply") && (f == "to" || f == "subject") {
				return p, errors.New("reply does not take to/subject: Gmail addresses the reply and keeps the thread subject (use cc/bcc to add people)")
			}
			return p, fmt.Errorf("%s is not valid for type %q", f, r.Type)
		}
	}
	if r.SendAt != "" && !sendsMail(r.Type) {
		return p, fmt.Errorf("sendAt is only valid for types that send mail (send, reply, forward, draft_send)")
	}

	switch r.Type {
	case "modify":
		return p, validateModify(&p)
	case "draft_delete", "draft_send":
		return p, validateDraftRef(&p)
	}

	// Types that compose an email.
	if !utf8.ValidString(r.Subject) || !utf8.ValidString(r.Body) || !utf8.ValidString(r.HTMLBody) {
		return p, errors.New("subject and bodies must be valid UTF-8")
	}
	if len(r.Body) > maxActionBody {
		return p, fmt.Errorf("body: longer than %d bytes", maxActionBody)
	}
	if len(r.HTMLBody) > maxActionHTMLBody {
		return p, fmt.Errorf("htmlBody: longer than %d bytes", maxActionHTMLBody)
	}
	forward := r.Type == "forward" || r.Type == "draft_forward"
	if !forward && strings.TrimSpace(r.Body) == "" && strings.TrimSpace(r.HTMLBody) == "" {
		return p, errors.New("body (or htmlBody) is required")
	}
	if strings.TrimSpace(p.Body) == "" && p.HTMLBody != "" {
		p.Body = htmlToText(p.HTMLBody)
	}
	if err := validateAddressList("to", r.To); err != nil {
		return p, err
	}
	if err := validateAddressList("cc", r.Cc); err != nil {
		return p, err
	}
	if err := validateAddressList("bcc", r.Bcc); err != nil {
		return p, err
	}
	if err := validateAttachmentRefs(r.Attachments); err != nil {
		return p, err
	}

	// "draft" and "draft_reply" are "send" and "reply" that Apps Script saves in
	// Gmail's Drafts folder instead of sending. A new draft may leave out the
	// recipients and subject; the user fills them in before sending it from Gmail.
	switch r.Type {
	case "send":
		if len(r.To) == 0 {
			return p, errors.New("to: at least one recipient is required")
		}
		if strings.TrimSpace(r.Subject) == "" {
			return p, errors.New("subject is required")
		}
	case "reply", "draft_reply":
		if r.MessageID == "" && r.ThreadID == "" {
			return p, errors.New("reply needs messageId (preferred) or threadId")
		}
		if r.MessageID != "" && !gmailIDPattern.MatchString(r.MessageID) {
			return p, errors.New("messageId: invalid Gmail message id")
		}
		if r.ThreadID != "" && !gmailIDPattern.MatchString(r.ThreadID) {
			return p, errors.New("threadId: invalid Gmail thread id")
		}
	case "forward", "draft_forward":
		if !gmailIDPattern.MatchString(r.MessageID) {
			return p, errors.New("forward needs messageId: the Gmail message id of the email to forward")
		}
		if len(r.To) == 0 && r.Type == "forward" {
			return p, errors.New("to: at least one recipient is required")
		}
	case "draft_update":
		if err := validateDraftRef(&p); err != nil {
			return p, err
		}
		if r.DraftMessageID == "" {
			return p, errors.New("draftMessageId is required: the draft's current message id, so a draft edited in Gmail meanwhile is not overwritten")
		}
	}
	if hasControl(r.Subject) || len(r.Subject) > maxActionSubject {
		return p, fmt.Errorf("subject: at most %d bytes, no line breaks", maxActionSubject)
	}
	if len(r.To)+len(r.Cc)+len(r.Bcc) > maxRecipients {
		return p, fmt.Errorf("at most %d recipients in total", maxRecipients)
	}
	return p, nil
}

func validateDraftRef(p *ActionPayload) error {
	if !gmailIDPattern.MatchString(p.DraftID) {
		return errors.New("draftId: missing or invalid Gmail draft id")
	}
	if p.DraftMessageID != "" && !gmailIDPattern.MatchString(p.DraftMessageID) {
		return errors.New("draftMessageId: invalid Gmail message id")
	}
	return nil
}

func validateModify(p *ActionPayload) error {
	n := len(p.MessageIDs) + len(p.ThreadIDs)
	if n == 0 {
		return errors.New("modify needs messageIds and/or threadIds")
	}
	if n > maxModifyTargets {
		return fmt.Errorf("modify: at most %d emails and conversations at a time", maxModifyTargets)
	}
	if err := validateIDs("messageIds", p.MessageIDs, gmailIDPattern); err != nil {
		return err
	}
	if err := validateIDs("threadIds", p.ThreadIDs, gmailIDPattern); err != nil {
		return err
	}
	if err := validateIDs("addLabelIds", p.AddLabelIDs, labelIDPattern); err != nil {
		return err
	}
	if err := validateIDs("removeLabelIds", p.RemoveLabelIDs, labelIDPattern); err != nil {
		return err
	}
	for _, l := range slices.Concat(p.AddLabelIDs, p.RemoveLabelIDs) {
		if labelsNotModifiable[l] {
			return fmt.Errorf("label %s cannot be added or removed (use trash/untrash for the trash)", l)
		}
	}
	for _, l := range p.AddLabelIDs {
		if slices.Contains(p.RemoveLabelIDs, l) {
			return fmt.Errorf("label %s is both added and removed", l)
		}
	}
	if p.Trash && p.Untrash {
		return errors.New("trash and untrash cannot both be set")
	}
	if len(p.AddLabelIDs)+len(p.RemoveLabelIDs) == 0 && !p.Trash && !p.Untrash {
		return errors.New("modify needs a change: addLabelIds, removeLabelIds, trash or untrash")
	}
	if len(p.AddLabelIDs)+len(p.RemoveLabelIDs) > 20 {
		return errors.New("modify: at most 20 label changes at a time")
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// retryDelay is 30s, 1m, 2m, 4m … capped at 30 minutes.
func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 7 {
		return 30 * time.Minute
	}
	d := 30 * time.Second << (attempt - 1)
	return min(d, 30*time.Minute)
}

const actionColumns = `id, type, status, payload, coalesce(idempotency_key, ''), attempts, max_attempts, not_before,
	lease_until, coalesce(last_error, ''), result, created_at, updated_at, finished_at`

func scanAction(sc interface{ Scan(...any) error }) (*Action, error) {
	var a Action
	var payload string
	var result sql.NullString
	var notBefore, created, updated int64
	var lease, finished sql.NullInt64
	if err := sc.Scan(&a.ID, &a.Type, &a.Status, &payload, &a.IdempotencyKey, &a.Attempts, &a.MaxAttempts,
		&notBefore, &lease, &a.LastError, &result, &created, &updated, &finished); err != nil {
		return nil, err
	}
	a.Payload = json.RawMessage(payload)
	if result.Valid {
		a.Result = json.RawMessage(result.String)
	}
	a.NotBefore, a.CreatedAt, a.UpdatedAt = msToRFC3339(notBefore), msToRFC3339(created), msToRFC3339(updated)
	if lease.Valid {
		a.LeaseUntil = msToRFC3339(lease.Int64)
	}
	if finished.Valid {
		a.FinishedAt = msToRFC3339(finished.Int64)
	}
	return &a, nil
}

func getActionTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (*Action, error) {
	a, err := scanAction(q.QueryRowContext(ctx, `SELECT `+actionColumns+` FROM actions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return a, err
}

func (s *Store) GetAction(ctx context.Context, id string) (*Action, error) {
	return getActionTx(ctx, s.db, id)
}

// CreateAction queues an action that Apps Script may perform from notBefore on
// (0 = immediately). With an idempotency key, repeating the same request returns
// the original action (created=false); reusing the key for a different request is
// a conflict.
func (s *Store) CreateAction(ctx context.Context, typ string, p ActionPayload, idemKey string, maxAttempts int, now, notBefore int64) (*Action, bool, error) {
	if notBefore < now {
		notBefore = now
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	if idemKey != "" {
		var id, existingType, existingPayload string
		err := tx.QueryRowContext(ctx, `SELECT id, type, payload FROM actions WHERE idempotency_key = ?`, idemKey).
			Scan(&id, &existingType, &existingPayload)
		if err == nil {
			if existingType != typ || existingPayload != string(payload) {
				return nil, false, fmt.Errorf("%w: idempotencyKey was already used for a different action (%s)", errConflict, id)
			}
			a, err := getActionTx(ctx, tx, id)
			return a, false, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
	}

	id := "act_" + randomHex(10)
	var key any
	if idemKey != "" {
		key = idemKey
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO actions
		(id, type, status, payload, idempotency_key, attempts, max_attempts, not_before, created_at, updated_at)
		VALUES (?, ?, 'pending', ?, ?, 0, ?, ?, ?, ?)`,
		id, typ, string(payload), key, maxAttempts, notBefore, now, now); err != nil {
		return nil, false, err
	}
	if typ == "modify" {
		// Show the change in the local copy at once; Gmail history confirms it
		// after Apps Script applies it, and revertLocalModify undoes it if not.
		undo, err := applyLocalModify(ctx, tx, p, now)
		if err != nil {
			return nil, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET local_undo = ? WHERE id = ?`, undo, id); err != nil {
			return nil, false, err
		}
	}
	a, err := getActionTx(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	return a, true, tx.Commit()
}

// FindRecentAction returns the newest action with exactly this type and payload
// created at or after since that has not been cancelled or failed, or nil. It lets
// the MCP tools recognise a repeated send request instead of mailing twice.
func (s *Store) FindRecentAction(ctx context.Context, typ string, p ActionPayload, since int64) (*Action, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var id string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM actions WHERE type = ? AND payload = ? AND created_at >= ?
		AND status NOT IN ('cancelled', 'failed') ORDER BY created_at DESC LIMIT 1`, typ, string(payload), since).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetAction(ctx, id)
}

// ClaimActions returns up to limit ready actions and leases them to the caller.
// Expired leases are recovered first.
func (s *Store) ClaimActions(ctx context.Context, limit int, now int64, lease time.Duration) ([]ClaimedAction, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status = 'pending', claim_id = NULL, lease_until = NULL,
		not_before = ?, updated_at = ?, last_error = 'lease expired without a report'
		WHERE status = 'claimed' AND lease_until < ? AND attempts < max_attempts`, now, now, now); err != nil {
		return nil, err
	}
	expired, err := queryStrings(ctx, tx, `SELECT id FROM actions
		WHERE status = 'claimed' AND lease_until < ? AND attempts >= max_attempts AND local_undo IS NOT NULL`, now)
	if err != nil {
		return nil, err
	}
	for _, id := range expired {
		if err := revertLocalModify(ctx, tx, id, now); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status = 'failed', claim_id = NULL, lease_until = NULL,
		updated_at = ?, finished_at = ?, last_error = 'lease expired on the final attempt; outcome unknown'
		WHERE status = 'claimed' AND lease_until < ? AND attempts >= max_attempts`, now, now, now); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT id FROM actions WHERE status = 'pending' AND not_before <= ?
		ORDER BY created_at, rowid LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	leaseUntil := now + lease.Milliseconds()
	out := make([]ClaimedAction, 0, len(ids))
	for _, id := range ids {
		claim := randomHex(16)
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET status = 'claimed', attempts = attempts + 1, claim_id = ?,
			lease_until = ?, updated_at = ? WHERE id = ?`, claim, leaseUntil, now, id); err != nil {
			return nil, err
		}
		var c ClaimedAction
		var payload string
		if err := tx.QueryRowContext(ctx, `SELECT id, type, attempts, payload FROM actions WHERE id = ?`, id).
			Scan(&c.ID, &c.Type, &c.Attempt, &payload); err != nil {
			return nil, err
		}
		c.ClaimID, c.LeaseUntil, c.Payload = claim, msToRFC3339(leaseUntil), json.RawMessage(payload)
		out = append(out, c)
	}
	if err := setKVTx(ctx, tx, "last_poll", fmt.Sprint(len(out)), now); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// CompleteAction records success. A completion is a fact (the mail was sent), so
// it is accepted whatever the current lease, and repeating it is harmless.
func (s *Store) CompleteAction(ctx context.Context, id string, res ActionResult, now int64) (*Action, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	a, err := getActionTx(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	if a.Status == "done" {
		return a, true, nil
	}
	result, _ := json.Marshal(res)
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status = 'done', result = ?, claim_id = NULL, lease_until = NULL,
		local_undo = NULL, updated_at = ?, finished_at = ? WHERE id = ?`, string(result), now, now, id); err != nil {
		return nil, false, err
	}
	a, err = getActionTx(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}
	return a, false, tx.Commit()
}

// FailAction records a failed attempt by the current claim holder. Retryable
// failures go back to pending with exponential backoff until max_attempts.
func (s *Store) FailAction(ctx context.Context, id, claimID, msg string, retryable bool, now int64) (*Action, error) {
	if len(msg) > maxErrorText {
		msg = truncateUTF8(msg, maxErrorText)
	}
	msg = strings.ToValidUTF8(msg, "?")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var status string
	var claim sql.NullString
	var attempts, maxAttempts int
	err = tx.QueryRowContext(ctx, `SELECT status, claim_id, attempts, max_attempts FROM actions WHERE id = ?`, id).
		Scan(&status, &claim, &attempts, &maxAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != "claimed" || !claim.Valid || claim.String != claimID {
		return nil, fmt.Errorf("%w: action is %s and this claim is no longer current", errStaleClaim, status)
	}
	if retryable && attempts < maxAttempts {
		notBefore := now + retryDelay(attempts).Milliseconds()
		_, err = tx.ExecContext(ctx, `UPDATE actions SET status = 'pending', claim_id = NULL, lease_until = NULL,
			not_before = ?, last_error = ?, updated_at = ? WHERE id = ?`, notBefore, msg, now, id)
	} else {
		if retryable {
			msg = "giving up after " + fmt.Sprint(attempts) + " attempts: " + msg
		}
		if err := revertLocalModify(ctx, tx, id, now); err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE actions SET status = 'failed', claim_id = NULL, lease_until = NULL,
			last_error = ?, updated_at = ?, finished_at = ? WHERE id = ?`, msg, now, now, id)
	}
	if err != nil {
		return nil, err
	}
	a, err := getActionTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return a, tx.Commit()
}

// CancelAction withdraws an action that Apps Script has not picked up yet.
func (s *Store) CancelAction(ctx context.Context, id string, now int64) (*Action, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	a, err := getActionTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if a.Status != "pending" {
		return a, fmt.Errorf("%w: only pending actions can be cancelled (status is %s)", errConflict, a.Status)
	}
	if err := revertLocalModify(ctx, tx, id, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE actions SET status = 'cancelled', updated_at = ?, finished_at = ? WHERE id = ?`, now, now, id); err != nil {
		return nil, err
	}
	a, err = getActionTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return a, tx.Commit()
}

func (s *Store) ListActions(ctx context.Context, status string, limit int) ([]*Action, error) {
	q := `SELECT ` + actionColumns + ` FROM actions`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Action{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
