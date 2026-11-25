package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// parseMailQuery turns a Gmail-style search string into SearchParams, so ChatGPT
// can use the operators it already knows:
//
//	from:  to:  cc:  bcc:  subject:  label:  category:  filename:
//	is:unread|read|starred|important  in:inbox|sent|trash|spam|anywhere|starred
//	has:attachment  after:/before:/newer:/older: (YYYY/MM/DD, YYYY-MM-DD or Unix time)
//	newer_than:/older_than: (Nh, Nd, Nw, Nm, Ny)  "exact phrase"  a OR b  word*
//	-word  -"phrase"  -from:  -to:  -subject:  -label:  -category:  -filename:  -is:  -in:  -has:
//
// Plain words are matched with the full-text index (all must match); a leading
// minus excludes. Operators it cannot express (unknown keys, negated dates) are
// returned in ignored, so the caller can say so instead of silently returning
// different results.
func parseMailQuery(q string, now time.Time, me string) (SearchParams, []string, error) {
	var p SearchParams
	var ignored []string
	var groups [][]string // full-text terms; terms in one group are ORed, groups are ANDed
	var excluded []string // full-text terms to exclude
	orNext := false

	addTerm := func(term string) {
		if orNext && len(groups) > 0 {
			groups[len(groups)-1] = append(groups[len(groups)-1], term)
		} else {
			groups = append(groups, []string{term})
		}
		orNext = false
	}
	setBool := func(dst **bool, v bool) { *dst = &v }
	addLabel := func(neg bool, label string) {
		if neg {
			p.NotLabels = append(p.NotLabels, label)
		} else {
			p.Label = label
		}
	}

	for _, tok := range tokenizeQuery(q) {
		if tok == "OR" || tok == "|" {
			orNext = len(groups) > 0
			continue
		}
		neg := strings.HasPrefix(tok, "-") && len(tok) > 1
		orig := tok
		if neg {
			tok = tok[1:]
			orNext = false // "a OR -b" is read as "a -b"
		}
		key, val, isOp := strings.Cut(tok, ":")
		key = strings.ToLower(key)
		if !isOp || !queryOperators[key] || val == "" {
			if term := ftsTerm(tok); term != "" {
				if neg {
					excluded = append(excluded, term)
				} else {
					addTerm(term)
				}
			}
			continue
		}
		val = strings.Trim(val, `"`)
		lower := strings.ToLower(val)
		switch key {
		case "from":
			if lower == "me" {
				if me == "" {
					addLabel(neg, "SENT")
					continue
				}
				val = me
			}
			if neg {
				p.NotFrom = append(p.NotFrom, val)
			} else {
				p.From = val
			}
		case "to", "cc", "bcc":
			if lower == "me" {
				if me == "" {
					ignored = append(ignored, orig+" (your address is not known yet)")
					continue
				}
				val = me
			}
			if neg {
				p.NotTo = append(p.NotTo, val)
			} else {
				p.To = val
			}
		case "subject":
			if neg {
				p.NotSubject = append(p.NotSubject, val)
			} else {
				p.Subject = val
			}
		case "label":
			addLabel(neg, val)
		case "category":
			addLabel(neg, "CATEGORY_"+strings.ToUpper(val))
		case "filename":
			if neg {
				p.NotAttachName = append(p.NotAttachName, val)
			} else {
				p.AttachName = val
			}
		case "thread":
			if neg {
				ignored = append(ignored, orig+" (exclusion is not supported)")
				continue
			}
			p.ThreadID = val
		case "is":
			switch lower {
			case "unread":
				setBool(&p.Unread, !neg)
			case "read":
				setBool(&p.Unread, neg)
			case "starred":
				setBool(&p.Starred, !neg)
			case "important":
				addLabel(neg, "IMPORTANT")
			default:
				ignored = append(ignored, orig)
			}
		case "in":
			switch lower {
			case "inbox":
				setBool(&p.InInbox, !neg)
			case "sent":
				addLabel(neg, "SENT")
			case "starred":
				setBool(&p.Starred, !neg)
			case "trash", "spam":
				if neg {
					continue // already left out unless in:anywhere
				}
				p.Label, p.WithTrash = strings.ToUpper(lower), true
			case "anywhere":
				if neg {
					ignored = append(ignored, orig)
					continue
				}
				p.WithTrash = true
			default:
				ignored = append(ignored, orig)
			}
		case "has":
			if lower == "attachment" || lower == "attachments" {
				setBool(&p.HasAttach, !neg)
			} else {
				ignored = append(ignored, orig)
			}
		case "after", "newer", "before", "older":
			ms, err := parseQueryDate(val)
			if err != nil {
				return p, nil, fmt.Errorf("%s: %w", orig, err)
			}
			if neg {
				// -after:X is before:X and the other way round.
				if key == "after" || key == "newer" {
					p.Before = ms
				} else {
					p.After = ms
				}
			} else if key == "after" || key == "newer" {
				p.After = ms
			} else {
				p.Before = ms
			}
		case "newer_than", "older_than":
			d, err := parseRelative(lower)
			if err != nil {
				return p, nil, fmt.Errorf("%s: %w", orig, err)
			}
			if (key == "newer_than") != neg {
				p.After = now.Add(-d).UnixMilli()
			} else {
				p.Before = now.Add(-d).UnixMilli()
			}
		}
	}

	var parts []string
	for _, g := range groups {
		if len(g) == 1 {
			parts = append(parts, g[0])
		} else {
			parts = append(parts, "("+strings.Join(g, " OR ")+")")
		}
		if len(parts) == 16 {
			ignored = append(ignored, "search terms after the 16th")
			break
		}
	}
	p.FTS = strings.Join(parts, " AND ")
	if len(excluded) > 16 {
		excluded = excluded[:16]
		ignored = append(ignored, "excluded terms after the 16th")
	}
	p.NotFTS = strings.Join(excluded, " OR ")
	for _, l := range []*[]string{&p.NotFrom, &p.NotTo, &p.NotSubject, &p.NotLabels, &p.NotAttachName} {
		if len(*l) > 16 {
			*l = (*l)[:16]
			ignored = append(ignored, "exclusions after the 16th of one kind")
		}
	}
	return p, ignored, nil
}

var queryOperators = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true, "subject": true, "label": true, "category": true,
	"filename": true, "thread": true, "is": true, "in": true, "has": true, "after": true, "before": true,
	"newer": true, "older": true, "newer_than": true, "older_than": true,
}

// tokenizeQuery splits on whitespace outside double quotes; quotes are kept.
func tokenizeQuery(q string) []string {
	var toks []string
	var b strings.Builder
	inQuote := false
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case unicode.IsSpace(r) && !inQuote:
			if b.Len() > 0 {
				toks = append(toks, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		toks = append(toks, b.String())
	}
	return toks
}

// ftsTerm makes one safe FTS5 term: a quoted phrase, optionally with a trailing *
// for prefix search. Quotes inside the text are dropped, so nothing in user input
// is ever interpreted as FTS5 syntax.
func ftsTerm(tok string) string {
	prefix := strings.HasSuffix(tok, "*")
	t := strings.TrimSpace(strings.ReplaceAll(strings.TrimRight(tok, "*"), `"`, ""))
	if t == "" {
		return ""
	}
	if r := []rune(t); len(r) > 128 {
		t = string(r[:128])
	}
	t = `"` + t + `"`
	if prefix {
		t += "*"
	}
	return t
}

// parseQueryDate accepts YYYY/MM/DD or YYYY-MM-DD (local midnight, as Gmail does),
// RFC 3339, or Unix seconds.
func parseQueryDate(v string) (int64, error) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n * 1000, nil
	}
	v = strings.ReplaceAll(v, "/", "-")
	if t, err := time.ParseInLocation("2006-1-2", v, time.Local); err == nil {
		return t.UnixMilli(), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli(), nil
	}
	return 0, fmt.Errorf("use YYYY/MM/DD")
}

// parseRelative parses Gmail's newer_than/older_than values: 12h, 3d, 2w, 6m, 1y.
func parseRelative(v string) (time.Duration, error) {
	if len(v) < 2 {
		return 0, fmt.Errorf("use a number and a unit, e.g. 7d")
	}
	n, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || n < 0 || n > 100000 {
		return 0, fmt.Errorf("use a number and a unit, e.g. 7d")
	}
	day := 24 * time.Hour
	switch v[len(v)-1] {
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * day, nil
	case 'w':
		return time.Duration(n) * 7 * day, nil
	case 'm':
		return time.Duration(n) * 30 * day, nil
	case 'y':
		return time.Duration(n) * 365 * day, nil
	}
	return 0, fmt.Errorf("unit must be h, d, w, m or y")
}
