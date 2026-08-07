package search

// The body matcher: InProcess streams the message's text body past a
// matcher instead of assembling it (RFC 8621 section 4.4.1 leaves text search
// semantics to the server, but a term must not go missing because of where the
// server happened to cut the stream). These tests pin the two seams where that
// could happen - the join between two body parts, and the chunk boundaries
// within one - and the section 5 window the snippet is cut from, plus the
// per-record parse cache InProcess shares its conditions through. They run
// with FullBodySearch true, since that is the mode that reaches this code.

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// testAccount is the account id fixture the tests in this package store
// blobs under.
const testAccount jmap.Id = "Atest1"

// twoPartBody is a message whose textBody view is two inline parts: the body
// text they make up is "alpha foo" + " " + "bar omega", so the term "foo bar"
// exists only across the join between them.
//
//	alpha foo bar omega
const twoPartBody = "Content-Type: multipart/mixed; boundary=b\r\n" +
	"Subject: two parts\r\n\r\n" +
	"--b\r\nContent-Type: text/plain\r\nContent-Disposition: inline\r\n\r\nalpha foo\r\n" +
	"--b\r\nContent-Type: text/plain\r\nContent-Disposition: inline\r\n\r\nbar omega\r\n" +
	"--b--\r\n"

// simpleMessage is a minimal single-part message whose body contains "free",
// used by TestSearcherParseCachePerRecord to count blob opens.
const simpleMessage = "From: Joe Bloggs <joe@example.com>\r\n" +
	"To: Jane Doe <jane@example.com>\r\n" +
	"Subject: Dinner on Thursday?\r\n" +
	"Message-ID: <msg1@example.com>\r\n" +
	"Date: Wed, 03 Mar 2021 10:00:00 +0000\r\n" +
	"\r\n" +
	"Hi Jane, are you free on Thursday evening?\r\n"

// searcherFor stores raw and returns a searcher and the record referring to it.
func searcherFor(t *testing.T, raw string) (*InProcess, objectdb.Object) {
	t.Helper()
	store := kvstore.New(memory.New())
	id := blob.IdFor([]byte(raw))
	if err := store.Put(context.Background(), testAccount, id, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	return New(store, Config{FullBodySearch: true}), objectdb.Object{"blobId": record.MustJSON(id)}
}

func matchBody(t *testing.T, s *InProcess, obj objectdb.Object, term string) bool {
	t.Helper()
	got, err := s.Match(context.Background(), testAccount, obj, "body", json.RawMessage(record.MustJSON(term)))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestBodyMatchSpansParts: the body is the textBody parts joined by a space, so
// a term that straddles two parts still matches - the matcher carries the tail
// of one part into the next rather than scanning each part alone.
func TestBodyMatchSpansParts(t *testing.T) {
	s, obj := searcherFor(t, twoPartBody)
	for _, term := range []string{"alpha", "omega", "foo bar", "a foo bar o"} {
		if !matchBody(t, s, obj, term) {
			t.Errorf("body term %q did not match", term)
		}
	}
	// The join is a space, not nothing: the parts are not run together.
	for _, term := range []string{"foobar", "needle"} {
		if matchBody(t, s, obj, term) {
			t.Errorf("body term %q matched but is not in the body", term)
		}
	}
}

// TestBodySnippetSpansParts: a snippet for a term that straddles the join is
// still produced, highlighted, and bracketed by nothing on the sides that are
// the edges of the body.
func TestBodySnippetSpansParts(t *testing.T) {
	s, obj := searcherFor(t, twoPartBody)
	_, preview := s.Snippet(context.Background(), testAccount, obj, nil, []string{"foo bar"})
	if !strings.Contains(preview, "<mark>foo bar</mark>") {
		t.Errorf("preview = %q, want the straddling match highlighted", preview)
	}
	if strings.HasPrefix(preview, "...") {
		t.Errorf("preview = %q: the window reaches the start of the body, so no ellipsis", preview)
	}
}

// TestTextMatcherChunks: the matcher is fed the body in arbitrary runs - one per
// body part, and in the streaming parser one per read - so a term must be found
// however the text is cut up.
func TestTextMatcherChunks(t *testing.T) {
	for _, chunks := range [][]string{
		{"hello world"},
		{"hello wor", "ld"},
		{"h", "e", "l", "l", "o", " ", "w", "o", "r", "l", "d"},
		{"hello ", "", "world"},
	} {
		m := newTextMatcher([]string{"LO WOR"}) // matching folds ASCII case
		for _, c := range chunks {
			m.feed(c)
		}
		scan := m.result()
		if !scan.matched {
			t.Errorf("chunks %q: no match", chunks)
			continue
		}
		if scan.window != "hello world" || !scan.atStart || !scan.atEnd {
			t.Errorf("chunks %q: window = %q atStart=%v atEnd=%v", chunks, scan.window, scan.atStart, scan.atEnd)
		}
	}
}

// TestTextMatcherWindow: the window is the match with snippetContext octets of
// body either side, and it reports which edges of the body it reached - that is
// what puts the section 5 ellipsis on the right sides.
func TestTextMatcherWindow(t *testing.T) {
	pad := strings.Repeat("x", 200)
	m := newTextMatcher([]string{"needle"})
	m.feed(pad + " needle " + pad)
	scan := m.result()
	if !scan.matched {
		t.Fatal("no match")
	}
	if scan.atStart || scan.atEnd {
		t.Errorf("window reaches an edge it should not: atStart=%v atEnd=%v", scan.atStart, scan.atEnd)
	}
	if !strings.Contains(scan.window, "needle") {
		t.Fatalf("window = %q, want it to hold the match", scan.window)
	}
	want := 2*snippetContext + len("needle")
	if len(scan.window) != want {
		t.Errorf("window = %d octets, want %d (the match plus context each side)", len(scan.window), want)
	}
	// The matcher keeps only its window, never the body it scanned.
	if len(m.window) > want+8 || len(m.tail) > m.keep() {
		t.Errorf("matcher retained %d window and %d tail octets of a %d octet body",
			len(m.window), len(m.tail), m.total)
	}
}

// TestTextMatcherRuneBoundaries: the window is cut on rune boundaries, because
// a snippet is HTML text and must be valid UTF-8 (section 5).
func TestTextMatcherRuneBoundaries(t *testing.T) {
	pad := strings.Repeat("é", 100) // two octets per rune, so a byte cut splits one
	m := newTextMatcher([]string{"needle"})
	m.feed(pad + "needle" + pad)
	scan := m.result()
	if !scan.matched {
		t.Fatal("no match")
	}
	if !utf8.ValidString(scan.window) {
		t.Errorf("window is not valid UTF-8: %q", scan.window)
	}
}

// countingStore wraps a blob.Store and counts Open calls.
type countingStore struct {
	blob.Store
	opens int
}

func (c *countingStore) Open(ctx context.Context, acct, blobID jmap.Id) (io.ReadCloser, int64, error) {
	c.opens++
	return c.Store.Open(ctx, acct, blobID)
}

// TestSearcherParseCachePerRecord: within one record's per-record scope the
// blob is read a fixed number of times however many text conditions the filter
// has - once for the structure, once per distinct set of terms searched for -
// and without a scope every condition pays afresh. Both conditions here search
// for the same term, so they share one body scan.
func TestSearcherParseCachePerRecord(t *testing.T) {
	cs := &countingStore{Store: kvstore.New(memory.New())}
	raw := []byte(simpleMessage)
	blobID := blob.IdFor(raw)
	if err := cs.Put(context.Background(), testAccount, blobID, raw); err != nil {
		t.Fatal(err)
	}
	s := New(cs, Config{FullBodySearch: true})
	obj := objectdb.Object{"blobId": record.MustJSON(blobID)}

	ctx := parse.NewRecordContext(context.Background())
	for _, f := range []string{"body", "text"} {
		if _, err := s.Match(ctx, testAccount, obj, f, json.RawMessage(`"free"`)); err != nil {
			t.Fatal(err)
		}
	}
	if cs.opens != 2 {
		t.Fatalf("per-record cache: want 2 blob opens (structure, body scan), got %d", cs.opens)
	}

	cs.opens = 0
	for _, f := range []string{"body", "text"} {
		if _, err := s.Match(context.Background(), testAccount, obj, f, json.RawMessage(`"free"`)); err != nil {
			t.Fatal(err)
		}
	}
	if cs.opens != 4 {
		t.Fatalf("no scope: want 4 blob opens, got %d", cs.opens)
	}
}
