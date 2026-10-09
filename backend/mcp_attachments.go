package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// get_attachment and the gmail-attachment:// resources. Each call downloads the
// file from Gmail through Apps Script, converts it in memory and lets it go when
// the response is written: attachment content is never stored or cached.

const attachmentURIPrefix = "gmail-attachment://"

func attachmentURI(messageID, partID string) string {
	return attachmentURIPrefix + messageID + "/" + url.PathEscape(partID)
}

func humanBytes(n int64) string {
	if n < 1<<20 {
		return fmt.Sprintf("%d KB", (n+1023)>>10)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

// findAttachment looks up an attachment of a synced email by its part ID.
func (s *Server) findAttachment(ctx context.Context, messageID, partID string) (*MessageOut, AttachmentOut, error) {
	m, err := s.loadEmail(ctx, messageID)
	if err != nil {
		return nil, AttachmentOut{}, err
	}
	for _, a := range m.Attachments {
		if a.PartID == partID {
			return m, a, nil
		}
	}
	var ids []string
	for _, a := range attachmentList(m.Attachments, false, false) {
		ids = append(ids, fmt.Sprintf("%s (%s)", a["attachment_id"], a["filename"]))
	}
	if len(ids) == 0 {
		return nil, AttachmentOut{}, userErr("email %s has no attachments", m.ID)
	}
	return nil, AttachmentOut{}, userErr("email %s has no attachment_id %q; its attachments are: %s", m.ID, truncateForError(partID), strings.Join(ids, ", "))
}

// downloadAttachment fetches the bytes from Gmail. At most two downloads run at a
// time so a burst of requests cannot exhaust the service's memory.
func (s *Server) downloadAttachment(ctx context.Context, messageID string, a AttachmentOut) ([]byte, error) {
	if a.Size > s.cfg.AttachmentMaxBytes {
		return nil, userErr("%s is %s, over this server's %s limit for attachments (ATTACHMENT_MAX_BYTES); open it in Gmail instead",
			a.Filename, humanBytes(a.Size), humanBytes(s.cfg.AttachmentMaxBytes))
	}
	fctx, cancel := context.WithTimeout(ctx, s.cfg.AttachmentFetchTimeout)
	defer cancel()
	select {
	case s.attachSem <- struct{}{}:
		defer func() { <-s.attachSem }()
	case <-fctx.Done():
		return nil, userErr("other attachments are being downloaded; try again in a moment")
	}
	start := time.Now()
	data, err := s.fetcher.Fetch(fctx, messageID, a.PartID, s.cfg.AttachmentMaxBytes)
	if err != nil {
		var te toolError
		if !errors.As(err, &te) && fctx.Err() != nil && ctx.Err() == nil {
			return nil, userErr("Gmail did not deliver the attachment within %s; try again", s.cfg.AttachmentFetchTimeout)
		}
		return nil, err
	}
	// Sizes only: never names or content.
	s.log.Info("attachment downloaded", "bytes", len(data), "ms", time.Since(start).Milliseconds())
	return data, nil
}

func (s *Server) toolGetAttachment(ctx context.Context, args json.RawMessage) (any, error) {
	var a struct {
		MessageID    string `json:"message_id"`
		AttachmentID string `json:"attachment_id"`
		MaxChars     int    `json:"max_chars"`
		Offset       int    `json:"offset"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	if a.Offset < 0 {
		return nil, userErr("offset must not be negative")
	}
	m, att, err := s.findAttachment(ctx, a.MessageID, a.AttachmentID)
	if err != nil {
		return nil, err
	}
	size := int(att.Size)
	var conv *converted
	if strings.HasPrefix(att.MimeType, "audio/") || strings.HasPrefix(att.MimeType, "video/") {
		// Nothing to extract: not worth downloading.
		conv = &converted{Kind: "unsupported", Notes: []string{"Audio and video files cannot be converted to text or an image here; open it in Gmail instead."}}
	} else {
		data, err := s.downloadAttachment(ctx, m.ID, att)
		if err != nil {
			return nil, err
		}
		size = len(data)
		cctx, cancel := context.WithTimeout(ctx, s.cfg.AttachmentConvertTimeout)
		defer cancel()
		conv, err = s.conv.convert(cctx, data, att.Filename, att.MimeType)
		data = nil // the download is no longer referenced once converted
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return nil, userErr("reading %s took longer than %s; open it in Gmail instead", att.Filename, s.cfg.AttachmentConvertTimeout)
			}
			return nil, err
		}
	}

	meta := map[string]any{
		"message_id": m.ID, "attachment_id": att.PartID, "filename": att.Filename, "mime_type": att.MimeType,
		"size_bytes": size, "format": conv.Kind, "gmail_url": s.gmailURL(ctx, m.ThreadID),
		"resource_uri": attachmentURI(m.ID, att.PartID),
	}
	if conv.Pages > 0 {
		meta["pages"] = conv.Pages
	}
	notes := conv.Notes
	content := []toolContent{{Type: "text"}} // metadata first, filled in below
	if conv.Text != "" {
		runes := []rune(conv.Text)
		if a.Offset >= len(runes) {
			return nil, userErr("offset %d is past the end of the text (%d characters)", a.Offset, len(runes))
		}
		end := min(len(runes), a.Offset+clamp(a.MaxChars, 40000, 1000, 200000))
		meta["text_chars"], meta["offset"] = len(runes), a.Offset
		if end < len(runes) {
			meta["truncated"], meta["next_offset"] = true, end
		}
		content = append(content, toolContent{Type: "text", Text: string(runes[a.Offset:end])})
	} else if a.Offset > 0 {
		return nil, userErr("this attachment has no text to continue")
	}
	if a.Offset == 0 && len(conv.Images) > 0 {
		var labels []string
		for _, img := range conv.Images {
			content = append(content, toolContent{Type: "image", MimeType: img.MimeType, Data: base64.StdEncoding.EncodeToString(img.Data)})
			if img.Label != "" {
				labels = append(labels, img.Label)
			}
		}
		if len(labels) > 0 {
			meta["images"] = labels
		}
		notes = append(notes, "Images are attached as MCP image content. If you cannot see them, tell the user and point them to gmail_url.")
	}
	if conv.Text == "" && len(conv.Images) == 0 && len(notes) == 0 {
		notes = append(notes, "The file contains no readable text.")
	}
	if len(notes) > 0 {
		meta["notes"] = notes
	}
	text, err := jsonText(meta)
	if err != nil {
		return nil, err
	}
	content[0].Text = text
	return toolResult{Content: content}, nil
}

// ---------------------------------------------------------------------------
// Resources: the original file as a binary (blob) resource, for clients that read
// MCP resources. The URI comes from get_attachment's resource_uri or the template.

var attachmentResourceTemplate = map[string]any{
	"uriTemplate": attachmentURIPrefix + "{message_id}/{attachment_id}",
	"name":        "email-attachment",
	"title":       "Email attachment (original file)",
	"description": "The original file of an email attachment, downloaded from Gmail on demand. " +
		"message_id and attachment_id come from search_emails, get_email or get_thread.",
}

func (s *Server) listAttachmentResources(ctx context.Context) ([]map[string]any, error) {
	recent, err := s.store.RecentAttachments(ctx, 50)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(recent))
	for _, r := range recent {
		out = append(out, map[string]any{
			"uri": attachmentURI(r.MessageID, r.PartID), "name": r.Filename, "mimeType": r.MimeType, "size": r.Size,
			"description": fmt.Sprintf("Attachment of %q from %s, %s", r.Subject, displayName(r.From), localTime(r.InternalDate)),
		})
	}
	return out, nil
}

var errResourceNotFound = errors.New("resource not found")

func (s *Server) readAttachmentResource(ctx context.Context, uri string) (map[string]any, error) {
	rest, ok := strings.CutPrefix(uri, attachmentURIPrefix)
	msgID, rawPart, ok2 := strings.Cut(rest, "/")
	partID, err := url.PathUnescape(rawPart)
	if !ok || !ok2 || err != nil || !utf8.ValidString(partID) {
		return nil, errResourceNotFound
	}
	m, att, err := s.findAttachment(ctx, msgID, partID)
	if err != nil {
		return nil, err
	}
	data, err := s.downloadAttachment(ctx, m.ID, att)
	if err != nil {
		return nil, err
	}
	mimeType := att.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return map[string]any{"contents": []map[string]any{{
		"uri": uri, "mimeType": mimeType, "blob": base64.StdEncoding.EncodeToString(data),
	}}}, nil
}
