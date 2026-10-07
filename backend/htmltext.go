package main

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Go's regexp is RE2 (linear time), so these are safe on large hostile input.
var (
	reHTMLDrop    = regexp.MustCompile(`(?is)<!--.*?-->|<head\b.*?</head\s*>|<script\b.*?</script\s*>|<style\b.*?</style\s*>`)
	reHTMLBreak   = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/tr|/li|/h[1-6]|/table|/blockquote|hr)\b[^>]*>`)
	reHTMLCell    = regexp.MustCompile(`(?i)<\s*/t[dh]\s*>`)
	reHTMLTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaceRun    = regexp.MustCompile(`[ \t\x{00a0}\x{200b}\x{200c}\x{200d}\x{feff}]+`)
	reBlankLines  = regexp.MustCompile(`\n[ \t]*(\n[ \t]*)+`)
	reLineSpacing = regexp.MustCompile(`[ \t]*\n[ \t]*`)
)

// htmlToText produces readable plain text from an HTML email body. It is used
// only when a message has no text/plain part, so full-text search still works.
func htmlToText(s string) string {
	s = reHTMLDrop.ReplaceAllString(s, " ")
	s = reHTMLBreak.ReplaceAllString(s, "\n")
	s = reHTMLCell.ReplaceAllString(s, " ")
	s = reHTMLTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r", "")
	s = reSpaceRun.ReplaceAllString(s, " ")
	s = reLineSpacing.ReplaceAllString(s, "\n")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// truncateUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
