package oracle

import (
	"net/mail"
	"strings"
	"time"
)

// MessageIDsForm parses a Raw header value as a list of msg-id values
// (RFC 8621 4.1.2.5): CFWS and surrounding angle brackets removed. A nil
// result means parse failure (the JSON value is null).
func MessageIDsForm(raw string) []string {
	return angleList(raw)
}

// URLsForm parses a Raw header value as an RFC 2369 URL list (RFC 8621
// 4.1.2.7): angle brackets and comments removed. A nil result means parse
// failure (the JSON value is null).
func URLsForm(raw string) []string {
	return angleList(raw)
}

func angleList(raw string) []string {
	clean, _ := stripComments(unfold(sanitizeString(raw)))
	var vals []string
	for i := 0; i < len(clean); {
		lt := strings.IndexByte(clean[i:], '<')
		if lt < 0 {
			break
		}
		start := i + lt + 1
		gt := strings.IndexByte(clean[start:], '>')
		if gt < 0 {
			break
		}
		if v := removeWSP(clean[start : start+gt]); v != "" {
			vals = append(vals, v)
		}
		i = start + gt + 1
	}
	return vals
}

func removeWSP(s string) string {
	if !strings.ContainsAny(s, " \t\r\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// DateForm parses a Raw header value as an RFC 5322 date-time (RFC 8621
// 4.1.2.6). A nil result means parse failure (the JSON value is null).
func DateForm(raw string) *time.Time {
	t, err := mail.ParseDate(strings.TrimSpace(unfold(raw)))
	if err != nil {
		return nil
	}
	return &t
}
