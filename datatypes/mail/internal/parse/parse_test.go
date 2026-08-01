package parse

// White-box tests over the capture's internal sinks: the body value cap
// (RFC 8621 section 4.2) and the per-message preview budget (section
// 4.1.4) must not hold more than their declared bound, which can only be
// checked from inside this package since the sink fields are private.
//
// These duplicate the textMessage/filler/heapWatcher helpers of
// ../../parsestream_test.go, which covers the equivalent bounds for the
// search matcher (a root-package type): the two helpers cannot be shared
// across the package boundary the sinks' privacy requires.

import (
	"io"
	"runtime"
	"strings"
	"testing"
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
	c := NewCapture()
	c.Values, c.MaxValueBytes = true, cap

	w := watchHeap(textMessage("utf-8", filler(size)))
	p, err := ParseMessage(w, c)
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
	if len(p.TextBody) != 1 {
		t.Fatalf("textBody has %d parts, want 1", len(p.TextBody))
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
	c := NewCapture()
	c.Values, c.MaxValueBytes = true, 16

	body := strings.NewReader("clean ascii text, then: \xff\xfe not utf-8 at all")
	if _, err := ParseMessage(textMessage("utf-8", body), c); err != nil {
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
		c := NewCapture()
		c.Values, c.MaxValueBytes = true, cap
		// Each character is three octets, so every cap but a multiple of three
		// falls inside one.
		body := strings.NewReader(strings.Repeat("€", 8))
		if _, err := ParseMessage(textMessage("utf-8", body), c); err != nil {
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
	c := NewCapture()
	c.Preview = true

	w := watchHeap(manyTextParts(parts, size))
	p, err := ParseMessage(w, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Msg.Root.SubParts) != parts {
		t.Fatalf("parsed %d parts, want %d", len(p.Msg.Root.SubParts), parts)
	}
	if p.Preview() == "" {
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
