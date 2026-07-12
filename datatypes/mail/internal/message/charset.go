package message

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/ianaindex"
)

// lookupCharset resolves a charset name to a decoder. ok is false for
// unknown charsets. UTF-8 and US-ASCII return a nil Encoding and are
// handled directly by decodeCharset.
func lookupCharset(name string) (enc encoding.Encoding, ascii bool, ok bool) {
	name = strings.Trim(strings.TrimSpace(strings.ToLower(name)), `"`)
	switch name {
	case "utf-8", "utf8", "":
		return nil, false, true
	case "us-ascii", "ascii", "ansi_x3.4-1968":
		return nil, true, true
	}
	e, err := ianaindex.MIME.Encoding(name)
	if err != nil || e == nil {
		return nil, false, false
	}
	return e, false, true
}

// decodeCharset decodes raw octets in the named charset to a unicode
// string, best effort: malformed sections become U+FFFD and problem is
// set. Unknown charsets decode as UTF-8 with replacement, also flagged.
func decodeCharset(data []byte, charset string) (s string, problem bool) {
	enc, ascii, ok := lookupCharset(charset)
	switch {
	case !ok:
		s = strings.ToValidUTF8(string(data), "�")
		return s, true
	case ascii:
		return decodeASCII(data)
	case enc == nil: // UTF-8
		if utf8.Valid(data) {
			return string(data), false
		}
		return strings.ToValidUTF8(string(data), "�"), true
	}
	out, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		// x/text decoders substitute rather than error in practice; treat
		// an error as a malformed tail and keep whatever was produced.
		return strings.ToValidUTF8(string(out), "�"), true
	}
	s = string(out)
	if strings.ContainsRune(s, '�') {
		problem = true
	}
	return s, problem
}

func decodeASCII(data []byte) (string, bool) {
	problem := false
	var b strings.Builder
	b.Grow(len(data))
	for _, c := range data {
		if c > 0x7f {
			b.WriteRune('�')
			problem = true
			continue
		}
		b.WriteByte(c)
	}
	return b.String(), problem
}

// DecodeBody produces the EmailBodyValue value string for a text part:
// charset decoded (best effort, U+FFFD on malformed sections) and CRLF
// normalized to LF (RFC 8621 4.1.4). charset == nil is treated as the
// implicit us-ascii.
func DecodeBody(data []byte, charset *string) (string, bool) {
	cs := "us-ascii"
	if charset != nil {
		cs = *charset
	}
	s, problem := decodeCharset(data, cs)
	return strings.ReplaceAll(s, "\r\n", "\n"), problem
}
