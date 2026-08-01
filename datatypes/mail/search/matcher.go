package search

// The body matcher: InProcess streams the message's text body past a matcher
// instead of assembling it, so a term is found wherever it falls in the body
// however the message happened to be cut into parts or the stream happened to
// be cut into reads (RFC 8621 section 4.4.1 leaves text search semantics to
// the server, but a term must not go missing because of that).

import (
	"strings"
	"unicode/utf8"
)

// bodyScan is what a streamed pass over the message's text body found: whether
// any term matched, and, if so, the excerpt around the first match that a
// snippet is built from - never the body itself.
type bodyScan struct {
	// matched is true if any term occurred in the body text.
	matched bool
	// window is the body text around the first match, with up to snippetContext
	// octets of context on each side, cut on rune boundaries.
	window string
	// atStart and atEnd report whether the window reaches the respective edge of
	// the body, which is what decides the section 5 ellipsis.
	atStart bool
	atEnd   bool
}

// snippetContext is how much body text surrounds the first match in a section 5
// preview, on each side.
const snippetContext = 60

// textMatcher scans the body text as it streams past, in bounded memory: it
// keeps only the octets a term could still straddle, plus the window around the
// first match. It never holds the body.
type textMatcher struct {
	terms []string // ASCII-lowered, non-empty (the i;ascii-casemap fold)
	span  int      // longest term: how much of the previous chunk a match can reach back into
	total int      // octets fed so far
	tail  []byte   // trailing octets kept for a straddling match and for the window's left context
	// pending is text fed but not yet scanned. Every scan re-reads the retained
	// tail, whose length the FILTER chooses (it is as long as the longest term),
	// so scanning each small piece of decoded text as it arrives would re-read
	// that tail once per piece: work quadratic in the body, on a path a client
	// reaches with a query. Pieces are accumulated to a batch first, which makes
	// the work linear in the body however long the terms are.
	pending []byte

	first    int // absolute offset of the first match, -1 until one is found
	firstEnd int
	window   []byte // the window under construction, from windowStart
	windowAt int    // absolute offset of window[0]
	windowTo int    // absolute offset the window ends at
}

// newTextMatcher builds a matcher for the non-empty terms, or nil when there is
// nothing to match.
func newTextMatcher(terms []string) *textMatcher {
	m := &textMatcher{first: -1}
	for _, t := range terms {
		if t == "" {
			continue
		}
		lt := asciiLower(t)
		m.terms = append(m.terms, lt)
		if len(lt) > m.span {
			m.span = len(lt)
		}
	}
	if len(m.terms) == 0 {
		return nil
	}
	return m
}

// keep is how many trailing octets the matcher must retain between chunks: what
// a term could straddle, plus the left context and rune slack a window needs.
func (m *textMatcher) keep() int { return m.span - 1 + snippetContext + utf8.UTFMax }

// scanBatch is the least text a scan is worth doing on. A batch is at least the
// longest term as well, so the tail a scan re-reads is never longer than the
// text it is reading it with.
const scanBatch = 4096

func (m *textMatcher) batch() int {
	if m.span > scanBatch {
		return m.span
	}
	return scanBatch
}

// feed passes the next run of body text through the matcher. The text is scanned
// in batches, so how finely the content was cut up on its way here changes what
// the matcher costs but not what it finds.
func (m *textMatcher) feed(text string) {
	if text == "" {
		return
	}
	m.pending = append(m.pending, text...)
	if len(m.pending) >= m.batch() {
		m.scan(string(m.pending))
		m.pending = m.pending[:0]
	}
}

// scan passes one batch of body text through the matcher.
func (m *textMatcher) scan(text string) {
	// Scan the retained tail together with the new text, so a term split across
	// the two is still found. Matches wholly inside the tail were found on the
	// previous feed, so nothing is counted twice. base is the body offset the
	// combined buffer starts at.
	base := m.total - len(m.tail)
	n := len(m.tail) + len(text)
	if n < len(m.tail) { // overflow: the sum wrapped past int's range
		n = 0 // fall back to growth-by-append, no bad capacity hint
	}
	buf := make([]byte, 0, n)
	buf = append(buf, m.tail...)
	buf = append(buf, text...)
	scan := asciiLower(string(buf))

	switch {
	case m.first < 0:
		start, end := -1, -1
		for _, t := range m.terms {
			if i := strings.Index(scan, t); i >= 0 && (start < 0 || i < start) {
				start, end = i, i+len(t)
			}
		}
		if start >= 0 {
			m.first, m.firstEnd = base+start, base+end
			// Open the window: snippetContext octets each side of the match, plus
			// the slack that lets both cuts land on a rune boundary. Its left half
			// is in the retained tail, which is why keep() reserves room for it.
			m.windowAt = m.first - snippetContext - utf8.UTFMax
			if m.windowAt < base {
				m.windowAt = base
			}
			if m.windowAt < 0 {
				m.windowAt = 0
			}
			m.windowTo = m.firstEnd + snippetContext + utf8.UTFMax
			end := len(buf)
			if to := m.windowTo - base; to < end {
				end = to // the rest of this run is past the window: do not keep it
			}
			m.window = append(m.window, buf[m.windowAt-base:end]...)
		}
	default:
		// Window still short of its right edge: take only what it is missing, so a
		// large part after the match is not pulled into memory.
		if need := m.windowTo - m.windowAt - len(m.window); need > 0 {
			if len(text) > need {
				m.window = append(m.window, text[:need]...)
			} else {
				m.window = append(m.window, text...)
			}
		}
	}
	m.total += len(text)

	if k := m.keep(); len(buf) > k {
		m.tail = append(m.tail[:0], buf[len(buf)-k:]...)
	} else {
		m.tail = append(m.tail[:0], buf...)
	}
}

// result closes the scan: it cuts the captured window down to its context
// bounds on rune boundaries (a snippet must be valid UTF-8, section 5) and
// reports whether it reaches the edges of the body, which is what decides the
// ellipsis.
func (m *textMatcher) result() bodyScan {
	if len(m.pending) > 0 {
		m.scan(string(m.pending)) // the last batch, however short it came out
		m.pending = m.pending[:0]
	}
	if m.first < 0 {
		return bodyScan{}
	}
	w := string(m.window)
	left := runeStartAt(w, m.first-snippetContext-m.windowAt)
	right := runeEndAt(w, m.firstEnd+snippetContext-m.windowAt)
	if right < left {
		right = left
	}
	return bodyScan{
		matched: true,
		window:  w[left:right],
		atStart: m.windowAt+left == 0,
		atEnd:   m.windowAt+right >= m.total,
	}
}

// asciiLower lowercases only ASCII A-Z, preserving byte length so match
// offsets align with the original text (the default i;ascii-casemap fold).
func asciiLower(s string) string {
	var changed bool
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			changed = true
			break
		}
	}
	if !changed {
		return s
	}
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// runeStartAt clamps i back to the start of the rune it lands in, so a window
// never begins mid-rune.
func runeStartAt(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// runeEndAt advances i to the next rune boundary, so a window never ends
// mid-rune.
func runeEndAt(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
