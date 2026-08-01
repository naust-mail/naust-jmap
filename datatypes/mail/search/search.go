// Package search is the built-in Searcher: case-insensitive substring
// matching over the stored fast fields and the on-demand parsed message
// blob (RFC 8621 section 4.4.1 permits this - "the exact search semantics
// ... is deliberately not defined"). It satisfies the mail.Searcher
// interface structurally; it does not import the mail package, so a host
// that wants real relevance can plug an index-backed Searcher of its own
// (RegisterEmail's searcher argument) without this package in its build.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/private/rawjson"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// InProcess is the built-in Searcher: case-insensitive substring matching
// over the stored fast fields and the on-demand parsed message blob.
type InProcess struct {
	store blob.Store
}

// New builds an InProcess searcher reading message blobs from store.
func New(store blob.Store) *InProcess {
	return &InProcess{store: store}
}

// Match implements the section 4.4.1 text conditions by substring.
func (s *InProcess) Match(ctx context.Context, acct jmap.Id, obj objectdb.Object, field string, value json.RawMessage) (bool, error) {
	if field == "header" {
		var h []string
		json.Unmarshal(value, &h)
		return s.matchHeader(ctx, acct, obj, h)
	}
	q, _ := rawjson.String(value)
	switch field {
	case "from", "to", "cc", "bcc":
		return record.ContainsFold(record.AddressText(obj, field), q), nil
	case "subject":
		return record.ContainsFold(record.StoredSubject(obj), q), nil
	case "text", "body":
		// text also searches the From/To/Cc/Bcc/Subject header fields
		// (section 4.4.1); body is the body parts only.
		if field == "text" {
			hdr := record.AddressText(obj, "from") + " " + record.AddressText(obj, "to") + " " +
				record.AddressText(obj, "cc") + " " + record.AddressText(obj, "bcc") + " " + record.StoredSubject(obj)
			if record.ContainsFold(hdr, q) {
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
func (s *InProcess) matchHeader(ctx context.Context, acct jmap.Id, obj objectdb.Object, h []string) (bool, error) {
	if len(h) == 0 {
		return false, nil
	}
	msg, err := s.parse(ctx, acct, obj)
	if err != nil {
		return false, err
	}
	values := msg.Msg.HeaderInstances(h[0])
	if len(h) == 1 {
		return len(values) > 0, nil
	}
	for _, v := range values {
		if record.ContainsFold(message.TextForm(v), h[1]) {
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
func (s *InProcess) Snippet(ctx context.Context, acct jmap.Id, obj objectdb.Object, subjectTerms, bodyTerms []string) (subject, preview string) {
	if subj := record.StoredSubject(obj); subj != "" && len(subjectTerms) > 0 {
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
func (s *InProcess) scanBody(ctx context.Context, acct jmap.Id, obj objectdb.Object, terms []string) (bodyScan, error) {
	msg, err := s.parse(ctx, acct, obj)
	if err != nil {
		return bodyScan{}, err
	}
	m := newTextMatcher(terms)
	if m == nil || len(msg.TextBody) == 0 {
		return bodyScan{}, nil
	}
	key := strings.Join(m.terms, "\x00")
	pc, cached := parse.CacheFrom(ctx)
	if cached {
		if scan, ok := pc.Memo[key]; ok {
			return scan.(bodyScan), nil
		}
	}
	inBody := make(map[string]bool, len(msg.TextBody))
	for _, p := range msg.TextBody {
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
		if pc.Memo == nil {
			pc.Memo = map[string]any{}
		}
		pc.Memo[key] = scan
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

// parse returns the record's parsed structure, reusing the per-record cache
// when the query runtime has installed one (Email/query), else parsing
// directly (SearchSnippet/get, which parses each Email once anyway). It
// captures no content: the header conditions need only the header list, and the
// body conditions stream their own content pass (scanBody).
func (s *InProcess) parse(ctx context.Context, acct jmap.Id, obj objectdb.Object) (*parse.Parsed, error) {
	var blobID jmap.Id
	if err := json.Unmarshal(obj["blobId"], &blobID); err != nil {
		return nil, fmt.Errorf("mail: Email record has no blobId: %w", err)
	}
	if pc, ok := parse.CacheFrom(ctx); ok {
		if !pc.Done || pc.BlobID != string(blobID) {
			pc.Msg, pc.Err = s.parseBlob(ctx, acct, blobID)
			pc.BlobID, pc.Done = string(blobID), true
			pc.Memo = nil // the memoized scans belong to the previous message
		}
		return pc.Msg, pc.Err
	}
	return s.parseBlob(ctx, acct, blobID)
}

// parseBlob opens and parses a message blob with no caching.
func (s *InProcess) parseBlob(ctx context.Context, acct, blobID jmap.Id) (*parse.Parsed, error) {
	rc, _, err := s.store.Open(ctx, acct, blobID)
	if err != nil {
		return nil, fmt.Errorf("mail: opening message blob %s: %w", blobID, err)
	}
	defer rc.Close()
	return parse.ParseMessage(rc, parse.NewCapture())
}
