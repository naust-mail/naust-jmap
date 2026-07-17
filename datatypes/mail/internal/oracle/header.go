package oracle

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// maxHeaderValue bounds one header field's value (including its folded
// continuation lines). A field can be folded across arbitrarily many lines,
// so an unbounded value is both a memory sink (the value is stored on the
// record) and, accumulated naively, quadratic; this caps it far above any
// real header (about a thousand message-ids or two thousand addresses).
const maxHeaderValue = 64 * 1024

// maxHeaders bounds the number of fields in one header block. A block that
// is nothing but tiny fields would otherwise allocate one HeaderField per
// line - a large multiple of the input. Real messages have at most a few
// dozen fields even after heavy relaying; fields beyond the cap are treated
// as the start of the body.
const maxHeaders = 1024

// parseHeaderBlock splits raw into the header fields and the body. Both
// CRLF and bare LF line endings are accepted. The header block ends at the
// first empty line, at a line that cannot be a header field or a fold
// (best-effort recovery for malformed input: that line starts the body),
// at the maxHeaders field cap, or at end of input. A field value is built
// with a Builder and capped at maxHeaderValue, so folding stays linear and
// bounded rather than quadratic and unbounded.
func parseHeaderBlock(raw []byte) ([]HeaderField, []byte) {
	var headers []HeaderField
	var name string
	var value strings.Builder
	building := false

	flush := func() {
		if building {
			headers = append(headers, HeaderField{Name: name, Value: value.String()})
			value.Reset()
			building = false
		}
	}
	// add appends header-value octets up to the remaining maxHeaderValue
	// budget; once the field is full, further octets (and fold lines) are
	// dropped, keeping accumulation linear.
	add := func(b []byte) {
		if remain := maxHeaderValue - value.Len(); remain > 0 {
			if len(b) > remain {
				b = b[:remain]
			}
			value.WriteString(sanitizeRaw(b))
		}
	}

	pos := 0
	for pos < len(raw) {
		line, next := nextLine(raw, pos)
		trimmed := trimLineEnding(line)
		if len(trimmed) == 0 {
			flush()
			return headers, raw[next:] // blank separator line
		}
		if trimmed[0] == ' ' || trimmed[0] == '\t' {
			// Fold line with no preceding field: malformed, body starts here.
			if !building {
				return headers, raw[pos:]
			}
			if value.Len() < maxHeaderValue {
				value.WriteString("\r\n")
				add(trimmed)
			}
			pos = next
			continue
		}
		colon := bytes.IndexByte(trimmed, ':')
		if colon <= 0 || !validFieldName(trimmed[:colon]) {
			flush()
			return headers, raw[pos:]
		}
		flush()
		if len(headers) >= maxHeaders {
			return headers, raw[pos:] // field cap: treat the rest as body
		}
		name = string(trimmed[:colon])
		add(trimmed[colon+1:])
		building = true
		pos = next
	}
	flush()
	return headers, nil
}

// nextLine returns the line starting at pos (without any line ending) and
// the offset of the following line.
func nextLine(raw []byte, pos int) ([]byte, int) {
	nl := bytes.IndexByte(raw[pos:], '\n')
	if nl < 0 {
		return raw[pos:], len(raw)
	}
	return raw[pos : pos+nl+1], pos + nl + 1
}

func trimLineEnding(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}

// validFieldName reports whether b is a plausible header field name:
// printable ASCII (%x21-%x7e), which is what RFC 5322 allows. Anything
// else means we misidentified the header/body boundary.
func validFieldName(b []byte) bool {
	for _, c := range b {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// sanitizeRaw converts raw header value octets to the Raw form string:
// NUL octets dropped (MUST, 4.1.2.1), any octet run violating UTF-8
// replaced with U+FFFD.
func sanitizeRaw(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = bytes.ReplaceAll(b, []byte{0}, nil)
	}
	return strings.ToValidUTF8(string(b), "�")
}

// sanitizeString is sanitizeRaw for values that arrive as strings. Header
// values produced by parseHeaderBlock are already sanitized; this makes
// the exported form functions total on arbitrary input as well.
func sanitizeString(s string) string {
	if strings.IndexByte(s, 0) < 0 && utf8.ValidString(s) {
		return s
	}
	return sanitizeRaw([]byte(s))
}

// unfold removes CRLF/LF that is followed by white space (RFC 5322 2.2.3;
// the WSP itself stays), yielding a single-line value.
func unfold(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			i++
			c = '\n'
		}
		if c == '\n' {
			continue // the following WSP (if any) is kept as the separator
		}
		b.WriteByte(c)
	}
	return b.String()
}
