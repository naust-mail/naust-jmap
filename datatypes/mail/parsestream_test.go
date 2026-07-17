package mail

// The two sinks that read a whole body part - the body value of an Email/get
// (RFC 8621 section 4.2) and the text a search term is matched against (section
// 4.4.1) - must not hold the part to read it. Both are reachable by a request,
// so a message the server accepted at delivery becomes a memory cost per request
// against it if either buffers, and a client that asks for a small value of a
// large part must pay for the small value.
//
// These tests measure that rather than assume it: they run a real parse over a
// large part and watch the live heap as its octets go past.

import (
	"context"
	"crypto/sha256"
	"hash"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// textMessage is a single text part of size octets in the given charset.
func textMessage(charset string, body io.Reader) io.Reader {
	return io.MultiReader(
		strings.NewReader("Content-Type: text/plain; charset="+charset+"\r\n\r\n"),
		body,
	)
}

// filler is size octets of plain text, produced without ever holding them.
func filler(size int) io.Reader {
	return io.LimitReader(&repeat{s: "the quick brown fox jumps over the lazy dog "}, int64(size))
}

type repeat struct {
	s   string
	off int
}

func (r *repeat) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.s[r.off:])
		r.off = (r.off + c) % len(r.s)
		n += c
	}
	return n, nil
}

// heapWatcher passes a message through and measures the live heap as it goes.
// What must stay small is what the parse HOLDS at any moment, so the heap is
// collected and read as the octets flow past; a sink that is buffering the part
// cannot hide from it.
type heapWatcher struct {
	r     io.Reader
	base  uint64
	peak  uint64
	reads int
}

func watchHeap(r io.Reader) *heapWatcher {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return &heapWatcher{r: r, base: ms.HeapAlloc}
}

func (w *heapWatcher) Read(p []byte) (int, error) {
	if w.reads%8 == 0 { // sampling: a full GC per read would take all day
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > w.peak {
			w.peak = ms.HeapAlloc
		}
	}
	w.reads++
	return w.r.Read(p)
}

// held is the live heap the parse added, at its worst moment.
func (w *heapWatcher) held() uint64 {
	if w.peak < w.base {
		return 0
	}
	return w.peak - w.base
}

// TestBodyValueCapIsBounded: a client asking for a few kilobytes of an eight
// megabyte text part is served from a few kilobytes of memory. The part is
// decoded to its end - that is what makes the encoding problem beside the value
// describe the part rather than the piece of it that fit - but only the value it
// asked for is kept.
func TestBodyValueCapIsBounded(t *testing.T) {
	const size = 8 << 20
	const cap = 4096
	c := newCapture()
	c.values, c.maxValueBytes = true, cap

	w := watchHeap(textMessage("utf-8", filler(size)))
	p, err := parseMessage(w, c)
	if err != nil {
		t.Fatal(err)
	}
	var s *valueSink
	for _, v := range c.valueSinks {
		s = v
	}
	if s == nil {
		t.Fatal("no value captured for the text part")
	}
	if len(s.value) != cap || !s.truncated {
		t.Errorf("value is %d octets (truncated=%v), want the %d octet cap", len(s.value), s.truncated, cap)
	}
	if len(p.textBody) != 1 {
		t.Fatalf("textBody has %d parts, want 1", len(p.textBody))
	}
	if used := w.held(); used > size/8 {
		t.Errorf("serving a %d octet value of an %d octet part held %d octets of heap", cap, size, used)
	}
}

// TestBodyValueProblemDescribesWholePart: the content past the cap is decoded
// and discarded, not skipped, so a part that stops being valid in its declared
// charset after the cap is still reported as an encoding problem (RFC 8621
// section 4.1.4). What a client is told about a part must not depend on how much
// of it that client asked to see.
func TestBodyValueProblemDescribesWholePart(t *testing.T) {
	c := newCapture()
	c.values, c.maxValueBytes = true, 16

	body := strings.NewReader("clean ascii text, then: \xff\xfe not utf-8 at all")
	if _, err := parseMessage(textMessage("utf-8", body), c); err != nil {
		t.Fatal(err)
	}
	var s *valueSink
	for _, v := range c.valueSinks {
		s = v
	}
	if s.value != "clean ascii tex" && s.value != "clean ascii text" {
		t.Errorf("value = %q, want the first 16 octets", s.value)
	}
	if !s.truncated {
		t.Error("isTruncated is false, but the part is longer than the cap")
	}
	if !s.problem {
		t.Error("isEncodingProblem is false: the malformed octets past the cap were never decoded")
	}
}

// TestBodyValueCapCutsOnARuneBoundary: section 4.2 forbids splitting a
// codepoint, and a cap that falls inside a multi-byte character is the case
// where a streaming decoder could produce half of one.
func TestBodyValueCapCutsOnARuneBoundary(t *testing.T) {
	for cap := int64(1); cap <= 12; cap++ {
		c := newCapture()
		c.values, c.maxValueBytes = true, cap
		// Each character is three octets, so every cap but a multiple of three
		// falls inside one.
		body := strings.NewReader(strings.Repeat("€", 8))
		if _, err := parseMessage(textMessage("utf-8", body), c); err != nil {
			t.Fatal(err)
		}
		for _, s := range c.valueSinks {
			if int64(len(s.value)) > cap {
				t.Errorf("cap %d: value is %d octets", cap, len(s.value))
			}
			if strings.ContainsRune(s.value, '�') || len(s.value)%3 != 0 {
				t.Errorf("cap %d: value %q splits a character", cap, s.value)
			}
			if !s.truncated {
				t.Errorf("cap %d: isTruncated is false", cap)
			}
		}
	}
}

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
	body := io.MultiReader(filler(size/2+7), strings.NewReader(term), filler(size/2))
	w := watchHeap(textMessage("utf-8", body))
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
	if used := w.held(); used > size/8 {
		t.Errorf("matching a term against an %d octet body held %d octets of heap", size, used)
	}
}

// manyTextParts is a multipart of n text/html parts of size octets each, built
// without the test holding it: this is the shape that makes a per-part capture
// expensive, since every one of those parts is a part the preview might come
// from.
func manyTextParts(n, size int) io.Reader {
	body := strings.Repeat("x", size) // one string, shared by every part
	rs := []io.Reader{strings.NewReader("Content-Type: multipart/mixed; boundary=b\r\n\r\n")}
	for i := 0; i < n; i++ {
		rs = append(rs,
			strings.NewReader("--b\r\nContent-Type: text/html\r\n\r\n"),
			strings.NewReader(body),
			strings.NewReader("\r\n"),
		)
	}
	return io.MultiReader(append(rs, strings.NewReader("--b--\r\n"))...)
}

// TestPreviewCaptureIsBoundedPerMessage: the preview is built from the leading
// text of a message (RFC 8621 section 4.1.4), and the parse cannot know which
// part that text will come from until the tree is walked, so it captures the
// leading octets of EVERY text part. What the preview can actually use is a few
// kilobytes; what a sender can put in front of it is a part count. The capture is
// therefore bounded per MESSAGE, not per part - otherwise one delivery of a
// message made of many text parts would cost the part count times the per-part
// budget, and an ingest that streams the message would go back to holding
// megabytes of it in the sinks instead.
func TestPreviewCaptureIsBoundedPerMessage(t *testing.T) {
	const parts, size = 1000, 32 << 10 // the per-part HTML preview budget, each
	c := newCapture()
	c.preview = true

	w := watchHeap(manyTextParts(parts, size))
	p, err := parseMessage(w, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.msg.Root.SubParts) != parts {
		t.Fatalf("parsed %d parts, want %d", len(p.msg.Root.SubParts), parts)
	}
	if p.preview() == "" {
		t.Error("no preview was built, although the message is nothing but text")
	}
	// The retained preview text, summed over every sink.
	var kept int
	for _, s := range c.previews {
		kept += len(s.raw) + len(s.text)
	}
	if kept > maxPreviewCapture*2 {
		t.Errorf("the preview sinks retained %d octets across %d parts, want the per-message budget of %d",
			kept, parts, maxPreviewCapture)
	}
	if used := w.held(); used > 8<<20 {
		t.Errorf("parsing a %d part message held %d octets of heap", parts, used)
	}
}

// TestDeliveryDoesNotHoldTheMessage: an ingest is the unauthenticated surface,
// and it is where holding the message would hurt most - anyone who can reach the
// MTA can send one. The message must flow through the blob store and the parser
// together, so the server pays for a buffer and not for what it was sent, and
// the Email must still come out right at the end of it.
func TestDeliveryDoesNotHoldTheMessage(t *testing.T) {
	const size = 8 << 20
	ts, db, _ := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	d := NewDeliverer(db, streamingStore{}, mapResolver{"jane@example.com": testAccount})

	msg := io.MultiReader(
		strings.NewReader("From: joe@example.com\r\nSubject: large\r\n"),
		textMessage("utf-8", filler(size)),
	)
	w := watchHeap(msg)
	evs := d.Deliver(context.Background(), deliveryEnv("joe@example.com", "jane@example.com"), w)

	if len(evs) != 1 || evs[0].Outcome != Accepted {
		t.Fatalf("delivery failed: %+v", evs)
	}
	if evs[0].Size < size {
		t.Errorf("stored size %d, want the whole message", evs[0].Size)
	}
	if used := w.held(); used > size/4 {
		t.Errorf("delivering an %d octet message held %d octets of heap", size, used)
	}
}

// TestDeliveryOversizeIsRejectedAsItArrives: the size limit is enforced on the
// octets as they pass, not on a buffer already full of them, so a sender cannot
// make the server hold a message it is going to refuse. The blob that was being
// written is aborted, so the refused message leaves nothing behind.
func TestDeliveryOversizeIsRejectedAsItArrives(t *testing.T) {
	const limit = 1 << 20
	const sent = 16 << 20
	ts, db, _ := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	d := NewDeliverer(db, streamingStore{}, mapResolver{"jane@example.com": testAccount}, WithMaxMessageSize(limit))

	w := watchHeap(textMessage("utf-8", filler(sent)))
	evs := d.Deliver(context.Background(), deliveryEnv("joe@example.com", "jane@example.com"), w)

	if len(evs) != 1 || evs[0].Outcome != Rejected || evs[0].Reason != "message too large" {
		t.Fatalf("want a rejection for the size limit, got %+v", evs)
	}
	if evs[0].EmailId != "" {
		t.Error("a rejected message created an Email")
	}
	// The parse stops at the limit, so the octets behind it are never even read:
	// what is held cannot be more than the limit itself.
	if used := w.held(); used > limit*2 {
		t.Errorf("rejecting a %d octet message held %d octets of heap (limit %d)", sent, used, limit)
	}
}

// streamingStore is a blob store that retains nothing: it computes the content
// address (RFC 8620 section 6.1) as the octets pass and drops them. The store the
// server ships with streams too, but the in-memory one the rest of these tests
// use holds every blob by design - measuring a delivery against it would measure
// the store, not the pipeline, which is what these two tests are about.
type streamingStore struct{}

func (streamingStore) Create(context.Context, jmap.Id) (blob.BlobWriter, error) {
	return &streamingWriter{h: sha256.New()}, nil
}

func (streamingStore) Put(context.Context, jmap.Id, jmap.Id, []byte) error { return nil }

func (streamingStore) Open(context.Context, jmap.Id, jmap.Id) (io.ReadCloser, int64, error) {
	return io.NopCloser(strings.NewReader("")), 0, nil
}

func (streamingStore) Delete(context.Context, jmap.Id, jmap.Id) error { return nil }

type streamingWriter struct {
	h hash.Hash
	n int64
}

func (w *streamingWriter) Write(b []byte) (int, error) {
	w.n += int64(len(b))
	return w.h.Write(b)
}

func (w *streamingWriter) ID() jmap.Id {
	var sum [sha256.Size]byte
	w.h.Sum(sum[:0])
	return blob.IdFromDigest(sum)
}

func (w *streamingWriter) Commit() (jmap.Id, error) {
	return w.ID(), nil
}

func (w *streamingWriter) Abort() error { return nil }

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
	if _, err := message.Parse(textMessage("utf-8", filler(size)), func(p *message.Part) message.LeafSinks {
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
