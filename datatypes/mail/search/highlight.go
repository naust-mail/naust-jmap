package search

// The section 5 highlighting transformations: escaping, <mark> wrapping, and
// the preview's octet budget.

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// maxPreviewOctets is the section 5 hard limit on a preview snippet.
const maxPreviewOctets = 255

// htmlEscaper replaces the three characters section 5 requires be escaped:
// &, <, and >. Quotes need not be escaped in element text content.
var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escapeHTML(s string) string { return htmlEscaper.Replace(s) }

// matchRanges returns the merged, sorted [start,end) byte ranges in text where
// any term occurs, folding ASCII case only so the offsets stay aligned with
// the original bytes (the default i;ascii-casemap collation).
func matchRanges(text string, terms []string) [][2]int {
	lower := asciiLower(text)
	var ranges [][2]int
	for _, term := range terms {
		if term == "" {
			continue
		}
		lt := asciiLower(term)
		from := 0
		for {
			i := strings.Index(lower[from:], lt)
			if i < 0 {
				break
			}
			start := from + i
			ranges = append(ranges, [2]int{start, start + len(lt)})
			from = start + len(lt)
		}
	}
	return mergeRanges(ranges)
}

// mergeRanges sorts ranges by start and merges any that touch or overlap.
func mergeRanges(r [][2]int) [][2]int {
	if len(r) < 2 {
		return r
	}
	sort.Slice(r, func(i, j int) bool { return r[i][0] < r[j][0] })
	out := r[:1]
	for _, cur := range r[1:] {
		last := &out[len(out)-1]
		if cur[0] <= last[1] {
			if cur[1] > last[1] {
				last[1] = cur[1]
			}
		} else {
			out = append(out, cur)
		}
	}
	return out
}

// highlightRanges escapes text (section 5) and wraps each range in
// <mark></mark>. Used for the subject, which has no length cap.
func highlightRanges(text string, ranges [][2]int) string {
	var b strings.Builder
	pos := 0
	for _, r := range ranges {
		b.WriteString(escapeHTML(text[pos:r[0]]))
		b.WriteString("<mark>")
		b.WriteString(escapeHTML(text[r[0]:r[1]]))
		b.WriteString("</mark>")
		pos = r[1]
	}
	b.WriteString(escapeHTML(text[pos:]))
	return b.String()
}

// bodyPreview returns a highlighted excerpt of the body around its first term
// match, escaped and never exceeding maxPreviewOctets octets (section 5). It
// brackets the excerpt with an ellipsis on any side that is not a body edge.
// The excerpt is emitted unit by unit under an octet budget, so it never
// splits a <mark> tag or an HTML entity and always closes an open mark.
func bodyPreview(scan bodyScan, terms []string) string {
	if !scan.matched {
		return ""
	}
	ranges := matchRanges(scan.window, terms)
	if len(ranges) == 0 {
		return ""
	}
	return highlightWindow(scan.window, ranges, scan.atStart, scan.atEnd)
}

// storedPreviewSnippet highlights term matches in the stored preview field
// (Config.FullBodySearch false: see search.go) instead of a scanBody window.
// The preview is already a bounded excerpt of the body (RFC 8621 section
// 4.1.4), so it is treated as its own complete window with no ellipsis.
func storedPreviewSnippet(text string, terms []string) string {
	ranges := matchRanges(text, terms)
	if len(ranges) == 0 {
		return ""
	}
	return highlightWindow(text, ranges, true, true)
}

// highlightWindow escapes and highlights window, keeping the result within
// maxPreviewOctets and bracketing it with an ellipsis on any side that is not
// a body edge. See bodyPreview.
func highlightWindow(window string, ranges [][2]int, atStart, atEnd bool) string {
	head, tail := "", ""
	if !atStart {
		head = "..."
	}
	if !atEnd {
		tail = "..."
	}
	budget := maxPreviewOctets - len(head) - len(tail)

	const openTag, closeTag = "<mark>", "</mark>"
	var b strings.Builder
	open := false
	for i := 0; i < len(window); {
		if open && atRangeEnd(ranges, i) {
			b.WriteString(closeTag)
			open = false
		}
		if !open && atRangeStart(ranges, i) {
			// Reserve room for both tags now so the close always fits later.
			if b.Len()+len(openTag)+len(closeTag) > budget {
				break
			}
			b.WriteString(openTag)
			open = true
		}
		_, sz := utf8.DecodeRuneInString(window[i:])
		esc := escapeHTML(window[i : i+sz])
		reserve := 0
		if open {
			reserve = len(closeTag)
		}
		if b.Len()+len(esc)+reserve > budget {
			break
		}
		b.WriteString(esc)
		i += sz
	}
	if open {
		b.WriteString(closeTag)
	}
	return head + b.String() + tail
}

func atRangeStart(ranges [][2]int, i int) bool {
	for _, r := range ranges {
		if r[0] == i {
			return true
		}
	}
	return false
}

func atRangeEnd(ranges [][2]int, i int) bool {
	for _, r := range ranges {
		if r[1] == i {
			return true
		}
	}
	return false
}
