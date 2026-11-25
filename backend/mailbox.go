package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
)

// Mailbox changes (read/unread, star, archive, labels, trash) are "modify"
// actions performed by Apps Script. The local copy is changed as soon as one is
// queued so that searches reflect it at once; the previous label sets are kept
// in actions.local_undo and restored if the action is cancelled or finally fails.

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryStrings(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// modifiedLabels applies a modify payload's label changes to a label set.
func modifiedLabels(labels []string, p ActionPayload) []string {
	set := make(map[string]bool, len(labels)+len(p.AddLabelIDs)+1)
	for _, l := range labels {
		set[l] = true
	}
	for _, l := range p.RemoveLabelIDs {
		delete(set, l)
	}
	for _, l := range p.AddLabelIDs {
		set[l] = true
	}
	if p.Trash {
		set["TRASH"] = true
	}
	if p.Untrash {
		delete(set, "TRASH")
	}
	out := make([]string, 0, len(set))
	for l := range set {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// modifyTargets returns the stored emails a modify payload affects: the listed
// emails plus every stored email of the listed conversations.
func modifyTargets(ctx context.Context, q querier, p ActionPayload) ([]string, error) {
	ids := append([]string{}, p.MessageIDs...)
	for _, t := range p.ThreadIDs {
		more, err := queryStrings(ctx, q, `SELECT gmail_id FROM messages WHERE thread_id = ?`, t)
		if err != nil {
			return nil, err
		}
		ids = append(ids, more...)
	}
	return dedupe(ids), nil
}

func setMessageLabels(ctx context.Context, tx *sql.Tx, id string, labels []string, now int64) error {
	f := flagsFor(labels)
	_, err := tx.ExecContext(ctx, `UPDATE messages SET
		label_ids = ?, is_unread = ?, is_starred = ?, is_important = ?, in_inbox = ?, in_sent = ?,
		in_trash = ?, in_spam = ?, is_draft = ?, state_changed_at = ?
		WHERE gmail_id = ?`,
		canonicalLabels(labels), f.unread, f.starred, f.important, f.inbox, f.sent, f.trash, f.spam, f.draft, now, id)
	return err
}

// applyLocalModify changes the stored labels of the affected emails and returns
// the undo record (JSON {gmail_id: previous labels}), or nil if nothing changed.
func applyLocalModify(ctx context.Context, tx *sql.Tx, p ActionPayload, now int64) (any, error) {
	ids, err := modifyTargets(ctx, tx, p)
	if err != nil {
		return nil, err
	}
	undo := map[string][]string{}
	for _, id := range ids {
		var labels string
		err := tx.QueryRowContext(ctx, `SELECT label_ids FROM messages WHERE gmail_id = ? AND gmail_deleted = 0`, id).Scan(&labels)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		old := decodeLabels(labels)
		next := modifiedLabels(old, p)
		if canonicalLabels(next) == labels {
			continue
		}
		if err := setMessageLabels(ctx, tx, id, next, now); err != nil {
			return nil, err
		}
		undo[id] = old
	}
	if len(undo) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(undo)
	return string(b), err
}

// revertLocalModify restores the labels a modify action changed locally, except
// on emails whose labels have changed since (Gmail sync knows better).
func revertLocalModify(ctx context.Context, tx *sql.Tx, actionID string, now int64) error {
	var undo sql.NullString
	var payload string
	err := tx.QueryRowContext(ctx, `SELECT local_undo, payload FROM actions WHERE id = ?`, actionID).Scan(&undo, &payload)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !undo.Valid) {
		return nil
	}
	if err != nil {
		return err
	}
	var prior map[string][]string
	var p ActionPayload
	if json.Unmarshal([]byte(undo.String), &prior) != nil || json.Unmarshal([]byte(payload), &p) != nil {
		prior = nil
	}
	for id, old := range prior {
		var cur string
		err := tx.QueryRowContext(ctx, `SELECT label_ids FROM messages WHERE gmail_id = ?`, id).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if cur != canonicalLabels(modifiedLabels(old, p)) {
			continue
		}
		if err := setMessageLabels(ctx, tx, id, old, now); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE actions SET local_undo = NULL WHERE id = ?`, actionID)
	return err
}

// ---------------------------------------------------------------------------
// Labels

type LabelInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Messages int64  `json:"messages"`
	Unread   int64  `json:"unread"`
}

// ListLabels returns the mailbox's labels with the number of stored emails (and
// unread ones) carrying each, outside trash and spam.
func (s *Store) ListLabels(ctx context.Context) ([]LabelInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.label_id, l.name, l.type, coalesce(c.n, 0), coalesce(c.u, 0)
		FROM labels l LEFT JOIN (
			SELECT j.value AS id, count(*) AS n, sum(m.is_unread) AS u
			FROM messages m, json_each(m.label_ids) j
			WHERE m.in_trash = 0 AND m.in_spam = 0 AND m.gmail_deleted = 0
			GROUP BY j.value) c ON c.id = l.label_id
		ORDER BY l.type DESC, l.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LabelInfo{}
	for rows.Next() {
		var l LabelInfo
		if err := rows.Scan(&l.ID, &l.Name, &l.Type, &l.Messages, &l.Unread); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// systemLabelNames maps the names people use for Gmail's system labels to their IDs.
var systemLabelNames = map[string]string{
	"inbox": "INBOX", "unread": "UNREAD", "starred": "STARRED", "important": "IMPORTANT",
	"sent": "SENT", "draft": "DRAFT", "drafts": "DRAFT", "spam": "SPAM", "trash": "TRASH", "chat": "CHAT",
	"personal": "CATEGORY_PERSONAL", "social": "CATEGORY_SOCIAL", "promotions": "CATEGORY_PROMOTIONS",
	"updates": "CATEGORY_UPDATES", "forums": "CATEGORY_FORUMS",
	"category:personal": "CATEGORY_PERSONAL", "category:social": "CATEGORY_SOCIAL",
	"category:promotions": "CATEGORY_PROMOTIONS", "category:updates": "CATEGORY_UPDATES", "category:forums": "CATEGORY_FORUMS",
}

// ResolveLabel finds a label by ID or display name (case-insensitive; hyphens
// may stand for spaces, as in Gmail's label: search). Returns "" if unknown.
func (s *Store) ResolveLabel(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT label_id FROM labels WHERE label_id = ?
		UNION ALL SELECT label_id FROM labels WHERE name = ? COLLATE NOCASE
		UNION ALL SELECT label_id FROM labels WHERE replace(name, ' ', '-') = ? COLLATE NOCASE
		LIMIT 1`, name, name, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if sys, ok := systemLabelNames[strings.ToLower(name)]; ok {
		return sys, nil
	}
	if up := strings.ToUpper(name); strings.HasPrefix(up, "CATEGORY_") || slices.Contains(systemLabelIDs, up) {
		return up, nil
	}
	return "", nil
}

var systemLabelIDs = []string{"INBOX", "UNREAD", "STARRED", "IMPORTANT", "SENT", "DRAFT", "SPAM", "TRASH", "CHAT"}
