package oracle

import (
	"encoding/base64"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// TextForm converts a Raw header value to the Text form (RFC 8621
// 4.1.2.2): unfolded, leading white space removed, syntactically correct
// RFC 2047 encoded-words with known charsets decoded (placement rules
// enforced: an encoded-word must be a whole white-space-delimited token),
// and the result normalized to NFC.
func TextForm(raw string) string {
	s := strings.TrimLeft(unfold(sanitizeString(raw)), " \t")
	return norm.NFC.String(decodeWords(s))
}

// decodeWords decodes RFC 2047 encoded-words appearing as complete
// white-space-delimited tokens. White space between two adjacent
// encoded-words is dropped, per RFC 2047 section 6.2.
func decodeWords(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	pendingWS := "" // white space not yet emitted
	prevDecoded := false
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && s[j] != ' ' && s[j] != '\t' {
			j++
		}
		token := s[i:j]
		if token != "" {
			if dec, ok := decodeEncodedWord(token); ok {
				if !prevDecoded {
					b.WriteString(pendingWS)
				}
				b.WriteString(dec)
				prevDecoded = true
			} else {
				b.WriteString(pendingWS)
				b.WriteString(token)
				prevDecoded = false
			}
			pendingWS = ""
		}
		i = j
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		pendingWS += s[j:i]
	}
	b.WriteString(pendingWS)
	return b.String()
}

// decodeEncodedWord decodes a single token iff it is entirely one
// RFC 2047 encoded-word ("=?charset?enc?text?=") with a known charset.
// A structurally valid word whose payload fails to decode yields U+FFFD
// (the parser "SHOULD insert a unicode replacement character and attempt
// to continue"); an unknown charset leaves the token undecoded.
func decodeEncodedWord(token string) (string, bool) {
	if len(token) < 8 || !strings.HasPrefix(token, "=?") || !strings.HasSuffix(token, "?=") {
		return "", false
	}
	inner := token[2 : len(token)-2]
	parts := strings.Split(inner, "?")
	if len(parts) != 3 {
		return "", false
	}
	charset, enc, text := parts[0], parts[1], parts[2]
	if charset == "" || text == "" {
		return "", false
	}
	// RFC 2231 language suffix ("utf-8*en") is stripped.
	if star := strings.IndexByte(charset, '*'); star >= 0 {
		charset = charset[:star]
	}
	if _, _, known := lookupCharset(charset); !known {
		return "", false
	}
	var payload []byte
	switch enc {
	case "b", "B":
		var err error
		payload, err = base64.StdEncoding.DecodeString(text)
		if err != nil {
			if payload, err = base64.RawStdEncoding.DecodeString(text); err != nil {
				return "�", true
			}
		}
	case "q", "Q":
		var ok bool
		if payload, ok = decodeQ(text); !ok {
			return "�", true
		}
	default:
		return "", false
	}
	s, _ := decodeCharset(payload, charset)
	return dropControls(s), true
}

func decodeQ(text string) ([]byte, bool) {
	out := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		switch c := text[i]; c {
		case '_':
			out = append(out, ' ')
		case '=':
			if i+2 >= len(text) {
				return nil, false
			}
			hi, ok1 := unhex(text[i+1])
			lo, ok2 := unhex(text[i+2])
			if !ok1 || !ok2 {
				return nil, false
			}
			out = append(out, hi<<4|lo)
			i += 2
		default:
			out = append(out, c)
		}
	}
	return out, true
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// dropControls removes NUL octets and control characters from a decoded
// encoded-word value (RFC 8621 4.1.2.2 rule 4).
func dropControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
