package mail

// Text search: the Searcher socket, its naive in-process default, and
// SearchSnippet/get (RFC 8621 section 5).
//
// Searcher is the pluggable boundary for the RFC 8621 section 4.4.1 text
// filter conditions (text/from/to/cc/bcc/subject/body/header) and the section
// 5 search snippets. The default naiveSearcher scans the stored fast fields
// and the message blob with case-insensitive substring matching, which
// section 4.4.1 permits ("the exact search semantics ... is deliberately not
// defined"). A host that wants real relevance plugs an index-backed Searcher
// when it registers Email (RegisterEmail's searcher argument).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// Searcher answers the text-search parts of Email that the structural query
// planner cannot: the RFC 8621 section 4.4.1 text FilterConditions and the
// section 5 search snippets. Both take a loaded Email record and may read its
// message blob.
type Searcher interface {
	// Match reports whether the Email matches one text FilterCondition
	// (RFC 8621 section 4.4.1): field is text, from, to, cc, bcc, subject,
	// body, or header, and value is the condition's raw JSON value. It may
	// read the message blob, so it returns an error for an I/O failure, which
	// the query fails on.
	Match(ctx context.Context, acct jmap.Id, obj objectdb.Object, field string, value json.RawMessage) (bool, error)

	// Snippet produces the highlighted subject and body preview for an Email
	// (RFC 8621 section 5). subjectTerms are the filter's text terms that
	// apply to the subject, bodyTerms those that apply to the body. Each
	// return is the empty string when that part does not match or the searcher
	// cannot produce it - section 5 requires null for a part the server cannot
	// determine, and SearchSnippet/get maps "" to JSON null. Unlike Match it
	// returns no error: section 5 makes snippets best-effort (a later fetch
	// MAY differ), so a read failure yields no snippet rather than failing.
	Snippet(ctx context.Context, acct jmap.Id, obj objectdb.Object, subjectTerms, bodyTerms []string) (subject, preview string)
}

// naiveSearcher is the default Searcher: case-insensitive substring matching
// over the stored fast fields and the on-demand parsed message blob.
type naiveSearcher struct {
	store blob.Store
}

// Match implements the section 4.4.1 text conditions by substring.
func (s naiveSearcher) Match(ctx context.Context, acct jmap.Id, obj objectdb.Object, field string, value json.RawMessage) (bool, error) {
	if field == "header" {
		var h []string
		json.Unmarshal(value, &h)
		return s.matchHeader(ctx, acct, obj, h)
	}
	q, _ := decodeString(value)
	switch field {
	case "from", "to", "cc", "bcc":
		return containsFold(addressText(obj, field), q), nil
	case "subject":
		return containsFold(storedSubject(obj), q), nil
	case "text", "body":
		// text also searches the From/To/Cc/Bcc/Subject header fields
		// (section 4.4.1); body is the body parts only.
		if field == "text" {
			hdr := addressText(obj, "from") + " " + addressText(obj, "to") + " " +
				addressText(obj, "cc") + " " + addressText(obj, "bcc") + " " + storedSubject(obj)
			if containsFold(hdr, q) {
				return true, nil
			}
		}
		scan, err := s.scanBody(ctx, acct, obj, []string{q})
		if err != nil {
			return false, err
		}
		return scan.matched, nil
	}
	return false, nil
}

// matchHeader matches the header condition: presence of the named field, or
// (when a second element is given) a substring of any of its values.
func (s naiveSearcher) matchHeader(ctx context.Context, acct jmap.Id, obj objectdb.Object, h []string) (bool, error) {
	if len(h) == 0 {
		return false, nil
	}
	msg, err := s.parse(ctx, acct, obj)
	if err != nil {
		return false, err
	}
	values := msg.msg.HeaderInstances(h[0])
	if len(h) == 1 {
		return len(values) > 0, nil
	}
	for _, v := range values {
		if containsFold(message.TextForm(v), h[1]) {
			return true, nil
		}
	}
	return false, nil
}

// Snippet produces section 5 snippets by the same substring matching as
// Match, so a part is highlighted exactly when its text condition matches. The
// subject is highlighted in full (section 5 caps only the preview); the
// preview is a window of the plaintext body around the first match, escaped
// and capped at 255 octets. A blob read failure yields no preview (section 5:
// return null when unable).
func (s naiveSearcher) Snippet(ctx context.Context, acct jmap.Id, obj objectdb.Object, subjectTerms, bodyTerms []string) (subject, preview string) {
	if subj := storedSubject(obj); subj != "" && len(subjectTerms) > 0 {
		if r := matchRanges(subj, subjectTerms); len(r) > 0 {
			subject = highlightRanges(subj, r)
		}
	}
	if len(bodyTerms) > 0 {
		if scan, err := s.scanBody(ctx, acct, obj, bodyTerms); err == nil {
			preview = bodyPreview(scan, bodyTerms)
		}
	}
	return subject, preview
}

// scanBody matches terms against the message's text body - the concatenated
// decoded text of its section 4.1.4 textBody parts, joined by a space - by
// streaming that text through a matcher rather than assembling it: the matcher
// keeps only the window a snippet needs, so a body of any size is scanned in
// bounded memory.
//
// Which parts make up the textBody view is known only from the flattened tree,
// so the structure is parsed first - that parse is cached per record and shared
// by every condition on it - and the content pass then feeds the matcher exactly
// those parts. So a record costs one structural pass plus one content pass per
// distinct set of terms searched for (both are memoized for the record). The
// structural pass decodes nothing at all, and the content pass decodes only the
// body text, where the single pass it replaces decoded every part of the
// message, attachments included.
func (s naiveSearcher) scanBody(ctx context.Context, acct jmap.Id, obj objectdb.Object, terms []string) (bodyScan, error) {
	msg, err := s.parse(ctx, acct, obj)
	if err != nil {
		return bodyScan{}, err
	}
	m := newTextMatcher(terms)
	if m == nil || len(msg.textBody) == 0 {
		return bodyScan{}, nil
	}
	key := strings.Join(m.terms, "\x00")
	pc, cached := ctx.Value(parseCacheKey{}).(*parseCache)
	if cached {
		if scan, ok := pc.scans[key]; ok {
			return scan, nil
		}
	}
	inBody := make(map[string]bool, len(msg.textBody))
	for _, p := range msg.textBody {
		inBody[p.PartID] = true
	}
	var blobID jmap.Id
	if err := json.Unmarshal(obj["blobId"], &blobID); err != nil {
		return bodyScan{}, fmt.Errorf("mail: Email record has no blobId: %w", err)
	}
	rc, _, err := s.store.Open(ctx, acct, blobID)
	if err != nil {
		return bodyScan{}, fmt.Errorf("mail: opening message blob %s: %w", blobID, err)
	}
	defer rc.Close()
	if _, err := message.Parse(rc, func(p *message.Part) message.LeafSinks {
		if !inBody[p.PartID] {
			return message.LeafSinks{}
		}
		return message.LeafSinks{Sinks: []message.Sink{newSearchSink(p, m)}}
	}); err != nil {
		return bodyScan{}, err
	}
	scan := m.result()
	if cached {
		if pc.scans == nil {
			pc.scans = map[string]bodyScan{}
		}
		pc.scans[key] = scan
	}
	return scan, nil
}

// searchSink hands one text part's content to the message's shared matcher,
// charset decoded as it streams. It retains nothing of its own, and the matcher
// keeps only what a term could straddle and the window around its match, so
// matching a term against a message never holds a body part - which matters
// most here, where the text being scanned is chosen by a filter rather than by
// the sender. Each part is followed by the space that joins it to the next, so a
// term spanning the boundary between two body parts still matches.
type searchSink struct {
	w *message.TextWriter
	m *textMatcher
}

func newSearchSink(p *message.Part, m *textMatcher) *searchSink {
	return &searchSink{w: message.NewTextWriter(matcherWriter{m: m}, p.Charset), m: m}
}

func (s *searchSink) Write(b []byte) (int, error) { return s.w.Write(b) }

func (s *searchSink) Close() error {
	if err := s.w.Close(); err != nil {
		return err
	}
	s.m.feed(" ")
	return nil
}

// matcherWriter passes decoded text on to the matcher as it is produced. The
// matcher scans across the pieces it is fed, so how the content was cut up on
// its way in does not decide what matches.
type matcherWriter struct{ m *textMatcher }

func (w matcherWriter) Write(b []byte) (int, error) {
	w.m.feed(string(b))
	return len(b), nil
}

// parseCacheKey is the context key under which emailFilter.EnterRecord
// stores the per-record parse cache.
type parseCacheKey struct{}

// parseCache memoizes the parsed structure of the record currently being
// matched, so the several text conditions evaluated against one record
// (RFC 8621 section 4.4.1) open and parse its blob once rather than once
// per condition. It holds a single record's result; the query runtime
// installs a fresh one per record via emailFilter.EnterRecord.
type parseCache struct {
	blobID jmap.Id
	msg    *parsed
	err    error
	done   bool
	// scans memoizes the body scans made for this record, keyed by the terms
	// scanned for, so conditions that search for the same text (a "text" and a
	// "body" condition on the same string) stream the blob once between them.
	scans map[string]bodyScan
}

// parse returns the record's parsed structure, reusing the per-record cache
// when the query runtime has installed one (Email/query), else parsing
// directly (SearchSnippet/get, which parses each Email once anyway). It
// captures no content: the header conditions need only the header list, and the
// body conditions stream their own content pass (scanBody).
func (s naiveSearcher) parse(ctx context.Context, acct jmap.Id, obj objectdb.Object) (*parsed, error) {
	var blobID jmap.Id
	if err := json.Unmarshal(obj["blobId"], &blobID); err != nil {
		return nil, fmt.Errorf("mail: Email record has no blobId: %w", err)
	}
	if pc, ok := ctx.Value(parseCacheKey{}).(*parseCache); ok {
		if !pc.done || pc.blobID != blobID {
			pc.msg, pc.err = s.parseBlob(ctx, acct, blobID)
			pc.blobID, pc.done = blobID, true
			pc.scans = nil // the memoized scans belong to the previous message
		}
		return pc.msg, pc.err
	}
	return s.parseBlob(ctx, acct, blobID)
}

// parseBlob opens and parses a message blob with no caching.
func (s naiveSearcher) parseBlob(ctx context.Context, acct, blobID jmap.Id) (*parsed, error) {
	rc, _, err := s.store.Open(ctx, acct, blobID)
	if err != nil {
		return nil, fmt.Errorf("mail: opening message blob %s: %w", blobID, err)
	}
	defer rc.Close()
	return parseMessage(rc, newCapture())
}

// ---- SearchSnippet/get (RFC 8621 section 5.1) ----

// SearchSnippet is one section 5 snippet: the relevant, highlighted portion of
// an Email that matched a search. Unlike most types it has no id.
type SearchSnippet struct {
	// EmailId is the Email the snippet applies to.
	EmailId jmap.Id `json:"emailId"`
	// Subject is the highlighted subject when the filter text matched it, else
	// null (section 5).
	Subject *string `json:"subject"`
	// Preview is the highlighted body excerpt when the filter text matched the
	// body, else null; never larger than 255 octets (section 5).
	Preview *string `json:"preview"`
}

// searchSnippet handles SearchSnippet/get. It is a custom method (SearchSnippet
// has no id, state, or standard methods, section 5), so it is registered
// directly rather than derived from a descriptor.
type searchSnippet struct {
	db       *objectdb.DB
	searcher Searcher
	core     jmap.CoreCapabilities
}

type snippetGetArgs struct {
	AccountId jmap.Id         `json:"accountId"`
	Filter    json.RawMessage `json:"filter"`
	EmailIds  []jmap.Id       `json:"emailIds"`
}

type snippetGetResponse struct {
	AccountId jmap.Id         `json:"accountId"`
	List      []SearchSnippet `json:"list"`
	// NotFound is null when every requested id was found (section 5.1).
	NotFound []jmap.Id `json:"notFound"`
}

// get implements SearchSnippet/get (RFC 8621 section 5.1): for each requested
// Email it returns the highlighted subject and body preview against the same
// filter Email/query takes, null for a part that did not match, and notFound
// for ids that do not exist.
func (h searchSnippet) get(ctx context.Context, call *runtime.Call) []jmap.Invocation {
	var a snippetGetArgs
	if err := runtime.DecodeArgs(call.Args, &a); err != nil {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := runtime.CheckAccount(call, a.AccountId, false); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	// Too many ids for one call is requestTooLarge (section 5.1); the get cap
	// is the analogous per-call object bound.
	if int64(len(a.EmailIds)) > h.core.MaxObjectsInGet {
		return runtime.Fail(call.CallID, jmap.ErrRequestTooLarge, "")
	}
	// The filter is the Email/query filter language; an unprocessable one is
	// unsupportedFilter (section 5.1), validated exactly as Email/query does.
	if errType, desc := runtime.ValidateFilter(EmailType(), emailFilter{}, a.Filter); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	subjectTerms, bodyTerms := snippetTerms(a.Filter)

	resp := snippetGetResponse{AccountId: a.AccountId, List: make([]SearchSnippet, 0, len(a.EmailIds))}
	for _, id := range a.EmailIds {
		obj, err := h.db.Get(ctx, a.AccountId, TypeEmail, id)
		if errors.Is(err, objectdb.ErrNotFound) {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		if err != nil {
			return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		subject, preview := h.searcher.Snippet(ctx, a.AccountId, obj, subjectTerms, bodyTerms)
		snip := SearchSnippet{EmailId: id}
		if subject != "" {
			snip.Subject = &subject
		}
		if preview != "" {
			snip.Preview = &preview
		}
		resp.List = append(resp.List, snip)
	}
	return runtime.Reply("SearchSnippet/get", call.CallID, resp)
}

// snippetTerms collects the filter's text terms that apply to the subject and
// to the body for section 5 highlighting: a "text" condition applies to both,
// "subject" to the subject, "body" to the body. FilterOperator nodes
// (AND/OR/NOT) are traversed; conditions that are not free-text (inMailbox,
// dates, keywords, addresses, header) contribute no highlight terms. A term
// under NOT simply never occurs in a matching Email, so it highlights nothing.
func snippetTerms(filter json.RawMessage) (subjectTerms, bodyTerms []string) {
	if len(filter) == 0 || isNullRaw(filter) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(filter, &m) != nil {
		return nil, nil
	}
	if conds, ok := m["conditions"]; ok {
		var arr []json.RawMessage
		json.Unmarshal(conds, &arr)
		for _, c := range arr {
			st, bt := snippetTerms(c)
			subjectTerms = append(subjectTerms, st...)
			bodyTerms = append(bodyTerms, bt...)
		}
		return subjectTerms, bodyTerms
	}
	for name, raw := range m {
		s, ok := decodeString(raw)
		if !ok {
			continue
		}
		switch name {
		case "text":
			subjectTerms = append(subjectTerms, s)
			bodyTerms = append(bodyTerms, s)
		case "subject":
			subjectTerms = append(subjectTerms, s)
		case "body":
			bodyTerms = append(bodyTerms, s)
		}
	}
	return subjectTerms, bodyTerms
}

// ---- highlighting (RFC 8621 section 5 transformations) ----

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

// bodyPreview returns a highlighted excerpt of the body around its first term
// match, escaped and never exceeding maxPreviewOctets octets (section 5). It
// brackets the excerpt with an ellipsis on any side that is not a body edge.
// The excerpt is emitted unit by unit under an octet budget, so it never
// splits a <mark> tag or an HTML entity and always closes an open mark.
func bodyPreview(scan bodyScan, terms []string) string {
	if !scan.matched {
		return ""
	}
	window := scan.window
	ranges := matchRanges(window, terms)
	if len(ranges) == 0 {
		return ""
	}

	head, tail := "", ""
	if !scan.atStart {
		head = "..."
	}
	if !scan.atEnd {
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
