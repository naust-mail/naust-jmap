package message

import "strings"

// previewMaxChars is the hard cap of RFC 8621 4.1.4 ("MUST NOT be more
// than 256 characters in length").
const previewMaxChars = 256

// How much decoded content to consider per part when building the
// preview; more than enough for 256 characters after collapsing.
const previewTextBudget = 8 * 1024
const previewHTMLBudget = 32 * 1024

// preview derives the plaintext preview fragment from the flattened body
// lists, preferring plaintext parts and falling back to tag-stripped HTML.
func preview(textBody, htmlBody []*Part) string {
	text := bodyText(textBody)
	if strings.TrimSpace(text) == "" {
		text = bodyText(htmlBody)
	}
	collapsed := strings.Join(strings.Fields(text), " ")
	runes := []rune(collapsed)
	if len(runes) > previewMaxChars {
		runes = runes[:previewMaxChars]
	}
	return string(runes)
}

func bodyText(parts []*Part) string {
	var b strings.Builder
	for _, p := range parts {
		if b.Len() >= previewTextBudget {
			break
		}
		switch p.Type {
		case "text/plain":
			s, _ := DecodeBody(capBytes(p.Decoded, previewTextBudget), p.Charset)
			b.WriteString(s)
			b.WriteByte(' ')
		case "text/html":
			s, _ := DecodeBody(capBytes(p.Decoded, previewHTMLBudget), p.Charset)
			b.WriteString(stripHTML(s))
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func capBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// stripHTML reduces HTML to its text content, best effort: tags become
// spaces, script/style elements are dropped whole, and the handful of
// entities common in real mail are decoded.
func stripHTML(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		switch s[i] {
		case '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				return b.String() // truncated tag: drop the rest
			}
			inner := s[i+1 : i+end]
			closing := strings.HasPrefix(inner, "/")
			tag := strings.ToLower(strings.TrimLeft(inner, "/ "))
			i += end + 1
			for _, skip := range [...]string{"script", "style"} {
				if !closing && strings.HasPrefix(tag, skip) {
					if close := strings.Index(strings.ToLower(s[i:]), "</"+skip); close >= 0 {
						i += close
					} else {
						return b.String()
					}
					break
				}
			}
			b.WriteByte(' ')
		case '&':
			decoded, width := htmlEntity(s[i:])
			b.WriteString(decoded)
			i += width
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

func htmlEntity(s string) (string, int) {
	semi := strings.IndexByte(s, ';')
	if semi < 1 || semi > 8 {
		return "&", 1
	}
	switch s[1:semi] {
	case "amp":
		return "&", semi + 1
	case "lt":
		return "<", semi + 1
	case "gt":
		return ">", semi + 1
	case "quot":
		return `"`, semi + 1
	case "apos", "#39":
		return "'", semi + 1
	case "nbsp", "#160":
		return " ", semi + 1
	}
	return "&", 1
}
