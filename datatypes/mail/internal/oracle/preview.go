package oracle

import "strings"

// previewMaxChars is the hard cap of RFC 8621 4.1.4 ("MUST NOT be more
// than 256 characters in length").
const previewMaxChars = 256

// PreviewTextBytes and PreviewHTMLBytes bound how much decoded content a
// preview-capture sink retains per part before charset decoding: more than
// enough for 256 characters after collapsing. A caller's preview sink caps its
// capture at these so no whole part is ever held to build a preview.
const (
	PreviewTextBytes = 8 * 1024
	PreviewHTMLBytes = 32 * 1024
)

// PreviewSource is one flattened body part's captured, charset-decoded text and
// its media type, for BuildPreview. The caller supplies these in body order
// (textBody preferred, htmlBody as fallback); each Text is what that part's
// preview sink captured, already bounded by PreviewTextBytes/PreviewHTMLBytes.
type PreviewSource struct {
	Type string
	Text string
}

// BuildPreview derives the plaintext preview fragment (RFC 8621 4.1.4) from the
// captured text of the flattened body parts, preferring the textBody parts and
// falling back to tag-stripped htmlBody. White space is collapsed and the
// result is capped at previewMaxChars. Because the algorithm may change over
// time, 4.1.4 allows a later fetch to differ; the previous value is not wrong.
func BuildPreview(textBody, htmlBody []PreviewSource) string {
	text := previewJoin(textBody)
	if strings.TrimSpace(text) == "" {
		text = previewJoin(htmlBody)
	}
	collapsed := strings.Join(strings.Fields(text), " ")
	runes := []rune(collapsed)
	if len(runes) > previewMaxChars {
		runes = runes[:previewMaxChars]
	}
	return string(runes)
}

// previewJoin concatenates the plaintext of the given parts up to the text
// budget, stripping HTML tags from text/html parts and ignoring non-text parts.
func previewJoin(parts []PreviewSource) string {
	var b strings.Builder
	for _, p := range parts {
		if b.Len() >= PreviewTextBytes {
			break
		}
		switch p.Type {
		case "text/plain":
			b.WriteString(p.Text)
			b.WriteByte(' ')
		case "text/html":
			b.WriteString(stripHTML(p.Text))
			b.WriteByte(' ')
		}
	}
	return b.String()
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
