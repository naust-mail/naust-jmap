package message

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// parseHeaderBlock splits raw into the header fields and the body. Both
// CRLF and bare LF line endings are accepted. The header block ends at the
// first empty line, at a line that cannot be a header field or a fold
// (best-effort recovery for malformed input: that line starts the body),
// or at end of input.
func parseHeaderBlock(raw []byte) ([]HeaderField, []byte) {
	var headers []HeaderField
	pos := 0
	for pos < len(raw) {
		line, next := nextLine(raw, pos)
		trimmed := trimLineEnding(line)
		if len(trimmed) == 0 {
			return headers, raw[next:] // blank separator line
		}
		if trimmed[0] == ' ' || trimmed[0] == '\t' {
			// Fold line with no preceding field: malformed, body starts here.
			if len(headers) == 0 {
				return headers, raw[pos:]
			}
			headers[len(headers)-1].Value += "\r\n" + sanitizeRaw(trimmed)
			pos = next
			continue
		}
		colon := bytes.IndexByte(trimmed, ':')
		if colon <= 0 || !validFieldName(trimmed[:colon]) {
			return headers, raw[pos:]
		}
		headers = append(headers, HeaderField{
			Name:  string(trimmed[:colon]),
			Value: sanitizeRaw(trimmed[colon+1:]),
		})
		pos = next
	}
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
