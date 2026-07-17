package mail

// The body matcher: the naive searcher streams the message's text body past a
// matcher instead of assembling it (RFC 8621 section 4.4.1 leaves text search
// semantics to the server, but a term must not go missing because of where the
// server happened to cut the stream). These tests pin the two seams where that
// could happen - the join between two body parts, and the chunk boundaries
// within one - and the section 5 window the snippet is cut from.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
)

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

// searcherFor stores raw and returns a searcher and the record referring to it.
func searcherFor(t *testing.T, raw string) (naiveSearcher, objectdb.Object) {
	t.Helper()
	store := kvstore.New(memory.New())
	id := blob.IdFor([]byte(raw))
	if err := store.Put(context.Background(), testAccount, id, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	return naiveSearcher{store: store}, objectdb.Object{"blobId": mustJSON(id)}
}

func matchBody(t *testing.T, s naiveSearcher, obj objectdb.Object, term string) bool {
	t.Helper()
	got, err := s.Match(context.Background(), testAccount, obj, "body", json.RawMessage(mustJSON(term)))
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
