package message

import (
	"bytes"
	"io"
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

// maxHeaderName bounds a field name. RFC 5322 2.1.1 caps a whole line at 998
// octets, so this is already far above anything a message may legitimately
// contain; a "name" longer than it is not a header field at all but a line of
// body text that happens to hold a colon, and is treated as the start of the
// body. Bounding it is what lets the header block be read as a stream: a field
// line is decided from a bounded prefix, never by buffering the line whole.
const maxHeaderName = 1024

// maxHeaderLine is the prefix of a line the header reader needs to decide a
// field completely: the longest name it will accept, the colon, and the longest
// value it will keep. Anything past it cannot affect the parse (the value is
// capped at maxHeaderValue), so it is read and dropped rather than held.
const maxHeaderLine = maxHeaderName + 1 + maxHeaderValue

// readHeaderBlock reads the header fields at the front of lr, leaving the body
// unread behind them. Both CRLF and bare LF line endings are accepted. The block
// ends at the first empty line, at a line that cannot be a header field or a
// fold (best-effort recovery for malformed input: that line starts the body, and
// is pushed back for the body to read), at the maxHeaders field cap, or at the
// end of the message. A field value is accumulated in a Builder and capped at
// maxHeaderValue, so folding stays linear and bounded rather than quadratic and
// unbounded. The only error is a failure reading lr.
func readHeaderBlock(lr *lineReader) ([]HeaderField, error) {
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
	// startsBody ends the header block at a line that is not part of it: the
	// line goes back to the reader, because it is the body's first line.
	startsBody := func(line []byte) ([]HeaderField, error) {
		flush()
		lr.unread(line)
		return headers, nil
	}

	for {
		line, trunc, err := lr.readLine(maxHeaderLine)
		if err == io.EOF {
			flush()
			return headers, nil
		}
		if err != nil {
			return nil, err
		}
		trimmed := trimLineEnding(line)
		if len(trimmed) == 0 {
			flush()
			return headers, nil // blank separator line: the body follows it
		}
		if trimmed[0] == ' ' || trimmed[0] == '\t' {
			// Fold line with no preceding field: malformed, body starts here.
			if !building {
				return startsBody(line)
			}
			if value.Len() < maxHeaderValue {
				value.WriteString("\r\n")
				add(trimmed)
			}
			if trunc {
				if err := lr.discardLine(); err != nil {
					return nil, err
				}
			}
			continue
		}
		colon := bytes.IndexByte(trimmed, ':')
		if colon <= 0 || colon > maxHeaderName || !validFieldName(trimmed[:colon]) {
			return startsBody(line)
		}
		flush()
		if len(headers) >= maxHeaders {
			return startsBody(line) // field cap: the rest of the message is body
		}
		name = string(trimmed[:colon])
		add(trimmed[colon+1:])
		building = true
		if trunc {
			if err := lr.discardLine(); err != nil {
				return nil, err
			}
		}
	}
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
// values produced by readHeaderBlock are already sanitized; this makes
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
