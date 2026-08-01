package search

// The sink that reads a whole body part for the text a search term is
// matched against (RFC 8621 section 4.4.1) must not hold the part to read it:
// it is reachable by a request, so a message the server accepted at delivery
// becomes a memory cost per request against it if it buffers, and a term the
// client's choice of length makes long must not turn one query into
// quadratic work. The matching claim for the Email/get body value lives with
// the delivery pipeline in the root package (parsestream_test.go), which
// shares its heap-watching fixtures with this file through
// internal/testsupport.
//
// These tests measure that rather than assume it: they run a real parse over
// a large part and watch the live heap or the total allocation as its octets
// go past.

import (
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/testsupport"
)

// TestSearchIsBoundedAndStraddles: matching a term against an eight megabyte
// body holds only the matcher's window, and a term that lands across the seams
// of the stream - a read boundary, a decoder's write, a character - is still
// found. A search that buffered the body would be a way to spend the server's
// memory from a filter.
func TestSearchIsBoundedAndStraddles(t *testing.T) {
	const size = 8 << 20
	const term = "needle"
	m := newTextMatcher([]string{term})

	// The term is planted deep inside the part, where no plausible buffer edge is
	// aligned with it: it straddles whatever seam the stream happens to have.
	body := io.MultiReader(testsupport.Filler(size/2+7), strings.NewReader(term), testsupport.Filler(size/2))
	w := testsupport.WatchHeap(testsupport.TextMessage("utf-8", body))
	if _, err := message.Parse(w, func(p *message.Part) message.LeafSinks {
		return message.LeafSinks{Sinks: []message.Sink{newSearchSink(p, m)}}
	}); err != nil {
		t.Fatal(err)
	}
	scan := m.result()
	if !scan.matched {
		t.Fatal("the term was not found")
	}
	if !strings.Contains(scan.window, term) {
		t.Errorf("snippet %q does not contain the term", scan.window)
	}
	if scan.atStart || scan.atEnd {
		t.Errorf("snippet reaches an edge of an %d octet body: %v/%v", size, scan.atStart, scan.atEnd)
	}
	if used := w.Held(); used > size/8 {
		t.Errorf("matching a term against an %d octet body held %d octets of heap", size, used)
	}
}

// TestSearchWorkIsLinearInTheBody: the term a body is matched against comes from
// the filter, so its length is the client's choice, and the matcher re-reads the
// octets a term could straddle - as many as the term is long - on every scan. A
// matcher that scanned each small piece of decoded text as it arrived would
// therefore re-read a long term's worth of tail thousands of times over a large
// body: a query with one long term and one large message becomes gigabytes of
// work. Total allocation is what shows it - the live heap stays small either way,
// because the copies are made and dropped - so that is what is measured here.
func TestSearchWorkIsLinearInTheBody(t *testing.T) {
	const size = 8 << 20
	term := strings.Repeat("z", 256<<10) // a quarter megabyte of term
	m := newTextMatcher([]string{term})

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := message.Parse(testsupport.TextMessage("utf-8", testsupport.Filler(size)), func(p *message.Part) message.LeafSinks {
		return message.LeafSinks{Sinks: []message.Sink{newSearchSink(p, m)}}
	}); err != nil {
		t.Fatal(err)
	}
	if m.result().matched {
		t.Fatal("the term is not in the body, but it matched")
	}
	runtime.ReadMemStats(&after)

	// Linear work over the body allocates a few copies of it. Quadratic work
	// allocates the term's tail once per piece of decoded text, which for these
	// sizes is two orders of magnitude more.
	allocated := after.TotalAlloc - before.TotalAlloc
	if limit := uint64(size) * 12; allocated > limit {
		t.Errorf("matching a %d octet term against an %d octet body allocated %d octets (limit %d): the scan is not linear in the body",
			len(term), size, allocated, limit)
	}
}

// TestSearchIsChunkIndependent: the matcher is fed whatever pieces the decoder
// produces, so the same body must match - and produce the same snippet -
// whatever those pieces are. This is the claim the streaming decode rests on.
func TestSearchIsChunkIndependent(t *testing.T) {
	body := strings.Repeat("alpha beta ", 40) + "gamma needle delta " + strings.Repeat("omega ", 40)
	want := feedInChunks(body, len(body)) // fed whole
	for _, n := range []int{1, 2, 3, 5, 64, 1000} {
		if got := feedInChunks(body, n); got != want {
			t.Errorf("%d-octet pieces give snippet %+v, want %+v", n, got, want)
		}
	}
}

func feedInChunks(body string, n int) bodyScan {
	m := newTextMatcher([]string{"needle"})
	for len(body) > 0 {
		take := n
		if take > len(body) {
			take = len(body)
		}
		m.feed(body[:take])
		body = body[take:]
	}
	return m.result()
}
