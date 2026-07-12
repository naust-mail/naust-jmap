package message

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// AddressesForm parses a Raw header value as an address-list into the
// Addresses form (RFC 8621 4.1.2.3): one EmailAddress per mailbox, group
// and comment structure discarded. Parsing is best effort; it never fails.
func AddressesForm(raw string) []Address {
	var flat []Address
	for _, g := range GroupedAddressesForm(raw) {
		flat = append(flat, g.Addresses...)
	}
	return flat
}

// GroupedAddressesForm parses a Raw header value as an address-list into
// the GroupedAddresses form (RFC 8621 4.1.2.4). Consecutive mailboxes that
// are not part of a group are collected under a nil-named group.
func GroupedAddressesForm(raw string) []AddressGroup {
	s := unfold(sanitizeString(raw))
	var groups []AddressGroup
	var run []Address
	flushRun := func() {
		if len(run) > 0 {
			groups = append(groups, AddressGroup{Name: nil, Addresses: run})
			run = nil
		}
	}
	i := 0
	for i < len(s) {
		j, ch := scanTop(s, i, ",:;")
		if ch == ':' {
			// Group: display-name ':' mailbox-list ';'. Nested groups are
			// not legal; a ':' inside the member list is consumed as text.
			name := displayName(s[i:j])
			var members []Address
			k := j + 1
			for k <= len(s) {
				m, sep := scanTop(s, k, ",;")
				if a := parseMailbox(s[k:m]); a != nil {
					members = append(members, *a)
				}
				k = m + 1
				if sep == ';' || sep == 0 {
					break
				}
			}
			flushRun()
			groups = append(groups, AddressGroup{Name: name, Addresses: members})
			i = k
			continue
		}
		if a := parseMailbox(s[i:j]); a != nil {
			run = append(run, *a)
		}
		i = j + 1
	}
	flushRun()
	return groups
}

// parseMailbox parses one mailbox (best effort). It returns nil for
// segments with no usable content (empty, white space, or comments only).
func parseMailbox(seg string) *Address {
	lt, found := scanTop(seg, 0, "<")
	if found == '<' {
		gt, _ := scanTop(seg, lt+1, ">") // len(seg) when unterminated
		email := cleanAddrSpec(seg[lt+1 : gt])
		name := displayName(seg[:lt])
		if name == nil {
			name = commentName(seg)
		}
		if email == "" && name == nil {
			return nil
		}
		return &Address{Name: name, Email: email}
	}
	email := cleanAddrSpec(seg)
	if email == "" {
		return nil
	}
	return &Address{Name: commentName(seg), Email: email}
}

// commentName returns the last comment in seg as a display name (the
// "comment immediately following the addr-spec" fallback of 4.1.2.3), or
// nil if there is none.
func commentName(seg string) *string {
	_, comments := stripComments(seg)
	for i := len(comments) - 1; i >= 0; i-- {
		if s := normalizeName(comments[i]); s != "" {
			return &s
		}
	}
	return nil
}

// displayName processes a display-name: comments stripped, quoted-strings
// unquoted with quoted-pairs decoded, encoded-words decoded, white space
// trimmed, NFC. Returns nil when nothing remains.
func displayName(raw string) *string {
	clean, _ := stripComments(raw)
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		if inQuote {
			switch c {
			case '\\':
				if i+1 < len(clean) {
					i++
					b.WriteByte(clean[i])
				}
			case '"':
				inQuote = false
			default:
				b.WriteByte(c)
			}
			continue
		}
		if c == '"' {
			inQuote = true
			continue
		}
		b.WriteByte(c)
	}
	if s := normalizeName(b.String()); s != "" {
		return &s
	}
	return nil
}

func normalizeName(s string) string {
	return norm.NFC.String(strings.TrimSpace(decodeWords(s)))
}

// cleanAddrSpec normalizes an addr-spec candidate: comments stripped,
// white space outside quoted-strings removed (undoes folding), and an
// obsolete leading route ("@a,@b:") dropped. Quoted local parts keep
// their quotes; the result is the raw addr-spec value.
func cleanAddrSpec(s string) string {
	clean, _ := stripComments(s)
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		if inQuote {
			b.WriteByte(c)
			switch c {
			case '\\':
				if i+1 < len(clean) {
					i++
					b.WriteByte(clean[i])
				}
			case '"':
				inQuote = false
			}
			continue
		}
		switch c {
		case ' ', '\t', '\r', '\n':
		case '"':
			inQuote = true
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	out := b.String()
	if strings.HasPrefix(out, "@") {
		if colon := strings.IndexByte(out, ':'); colon >= 0 {
			out = out[colon+1:]
		}
	}
	return out
}

// scanTop scans s from start for the first byte of targets at the top
// level: outside quoted-strings, comments, and angle brackets. It returns
// (len(s), 0) if none is found. Targets themselves are matched before the
// state transitions, so scanning for '<' finds the bracket itself.
func scanTop(s string, start int, targets string) (int, byte) {
	inQuote := false
	comment := 0
	angle := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inQuote {
			switch c {
			case '\\':
				i++
			case '"':
				inQuote = false
			}
			continue
		}
		if comment > 0 {
			switch c {
			case '\\':
				i++
			case '(':
				comment++
			case ')':
				comment--
			}
			continue
		}
		if !angle && strings.IndexByte(targets, c) >= 0 {
			return i, c
		}
		switch c {
		case '"':
			inQuote = true
		case '(':
			comment++
		case '<':
			angle = true
		case '>':
			angle = false
		}
	}
	return len(s), 0
}

// stripComments removes RFC 5322 comments (nested, quote-aware) from s and
// returns them separately with quoted-pairs decoded.
func stripComments(s string) (string, []string) {
	var b, cur strings.Builder
	var comments []string
	inQuote := false
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuote {
			b.WriteByte(c)
			switch c {
			case '\\':
				if i+1 < len(s) {
					i++
					b.WriteByte(s[i])
				}
			case '"':
				inQuote = false
			}
			continue
		}
		if depth > 0 {
			switch c {
			case '\\':
				if i+1 < len(s) {
					i++
					cur.WriteByte(s[i])
				}
			case '(':
				depth++
				cur.WriteByte(c)
			case ')':
				depth--
				if depth == 0 {
					comments = append(comments, cur.String())
					cur.Reset()
				} else {
					cur.WriteByte(c)
				}
			default:
				cur.WriteByte(c)
			}
			continue
		}
		switch c {
		case '"':
			inQuote = true
			b.WriteByte(c)
		case '(':
			depth = 1
		default:
			b.WriteByte(c)
		}
	}
	if depth > 0 && cur.Len() > 0 { // unterminated comment
		comments = append(comments, cur.String())
	}
	return b.String(), comments
}
