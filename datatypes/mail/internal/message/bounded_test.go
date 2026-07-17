package message

// What the streaming parser is for: a message costs the parser memory in
// proportion to its structure, not to its size. A part nobody asked about is
// read past, not held; a part somebody asked about is held only by the sink that
// asked, and only as much as that sink keeps. These tests measure it rather than
// assert it in a comment, because the whole rewrite is worthless if it quietly
// buffers again.

import (
	"io"
	"runtime"
	"strings"
	"testing"
)

// bigMessage is a multipart with one small text part and one attachment of size
// octets, which is what an unauthenticated sender can make the server look at.
func bigMessage(size int) io.Reader {
	head := "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nhello\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n"
	tail := "\r\n--b--\r\n"
	// The attachment is base64 lines, as a real one is (RFC 2045 6.8): whole
	// four-character groups, with padding only ever at the end of the data.
	line := strings.Repeat("QUJD", 16) + "\r\n"
	body := strings.Repeat(line, size/len(line)+1)
	return io.MultiReader(
		strings.NewReader(head),
		strings.NewReader(body),
		strings.NewReader(tail),
	)
}

// peakReader passes a message through and, as it goes, measures the live heap.
// Total allocation would prove nothing - a streaming parser copies every octet a
// few times, and so allocates in proportion to the message either way. What must
// stay small is what is HELD at any moment, so the heap is collected and measured
// as the message flows past: a parser holding the message cannot hide from this.
type peakReader struct {
	r     io.Reader
	base  uint64
	peak  uint64
	reads int
}

func newPeakReader(r io.Reader) *peakReader {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return &peakReader{r: r, base: ms.HeapAlloc}
}

func (p *peakReader) Read(b []byte) (int, error) {
	if p.reads%8 == 0 { // sampling: a full GC per read would take all day
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > p.peak {
			p.peak = ms.HeapAlloc
		}
	}
	p.reads++
	return p.r.Read(b)
}

// held is the live heap the parse added, at its worst moment.
func (p *peakReader) held() uint64 {
	if p.peak < p.base {
		return 0
	}
	return p.peak - p.base
}

// TestStructureOnlyParseIsBounded: parsing a large message for its structure
// alone - which is what a delivery does, since it needs the headers and the two
// fast fields and no attachment content at all - must not cost the size of the
// message. A parser that buffers the body would allocate at least 8 MB here.
func TestStructureOnlyParseIsBounded(t *testing.T) {
	const size = 8 << 20
	pr := newPeakReader(bigMessage(size))
	m, err := Parse(pr, func(p *Part) LeafSinks {
		// Only the text part is of interest, and only its first octets: exactly
		// what a preview needs. The attachment declares no sink.
		if p.Type != "text/plain" {
			return LeafSinks{}
		}
		return LeafSinks{Sinks: []Sink{&captureSink{}}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Root.SubParts) != 2 {
		t.Fatalf("parsed %d parts, want 2", len(m.Root.SubParts))
	}
	if used := pr.held(); used > size/8 {
		t.Errorf("parsing an %d octet message held %d octets of heap; it must not hold the message", size, used)
	}
	// The attachment was read past, not decoded: no identity, no problem flag.
	att := m.Root.SubParts[1]
	if att.Size != 0 || att.Digest != [32]byte{} || att.EncodingProblem {
		t.Errorf("the attachment was processed although no sink asked for it: size=%d problem=%v", att.Size, att.EncodingProblem)
	}
}

// TestIdentityOnlyParseIsBounded: asking for a part's blobId and size means
// hashing and counting its content, which is a stream, not a buffer. The parser
// must still not hold the part.
func TestIdentityOnlyParseIsBounded(t *testing.T) {
	const size = 8 << 20
	pr := newPeakReader(bigMessage(size))
	m, err := Parse(pr, func(*Part) LeafSinks {
		return LeafSinks{Identity: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	att := m.Root.SubParts[1]
	if att.Size < size/2 {
		t.Fatalf("attachment size = %d, want roughly the decoded content", att.Size)
	}
	if att.Digest == [32]byte{} {
		t.Error("no digest, so no blobId: identity was asked for")
	}
	if used := pr.held(); used > size/4 {
		t.Errorf("hashing an %d octet message held %d octets of heap; it must not hold the part", size, used)
	}
}

// TestSinkHoldsOnlyWhatItKeeps: a sink that keeps a bounded prefix - the shape
// every real one has - bounds the parse, even on a huge text part.
func TestSinkHoldsOnlyWhatItKeeps(t *testing.T) {
	const size = 8 << 20
	const kept = 4096
	raw := "Content-Type: text/plain\r\n\r\n" + strings.Repeat("word ", size/5)
	pr := newPeakReader(strings.NewReader(raw))
	if _, err := Parse(pr, func(*Part) LeafSinks {
		return LeafSinks{Sinks: []Sink{&prefixSink{limit: kept}}}
	}); err != nil {
		t.Fatal(err)
	}
	if used := pr.held(); used > size/4 {
		t.Errorf("a sink keeping %d octets of an %d octet part held %d octets of heap", kept, size, used)
	}
}

// prefixSink keeps the first limit octets and drops the rest, as the preview
// sink does.
type prefixSink struct {
	limit int
	buf   []byte
}

func (s *prefixSink) Write(b []byte) (int, error) {
	if room := s.limit - len(s.buf); room > 0 {
		if len(b) > room {
			b = b[:room]
		}
		s.buf = append(s.buf, b...)
	}
	return len(b), nil
}

func (s *prefixSink) Close() error { return nil }

// TestMixedFactoryAcrossParts: a factory that asks for some parts and not others
// - which is what every real consumer does - gets exactly what it asked for, and
// the parts it skipped do not disturb the walk of the ones it wanted.
func TestMixedFactoryAcrossParts(t *testing.T) {
	raw := crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

first
--b
Content-Type: application/octet-stream
Content-Transfer-Encoding: base64

!!!not base64!!!
--b
Content-Type: text/plain

second
--b--
`)
	got := map[string]*captureSink{}
	m, err := Parse(strings.NewReader(raw), func(p *Part) LeafSinks {
		if !strings.HasPrefix(p.Type, "text/") {
			return LeafSinks{}
		}
		s := &captureSink{}
		got[p.PartID] = s
		return LeafSinks{Identity: true, Sinks: []Sink{s}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Root.SubParts) != 3 {
		t.Fatalf("got %d parts, want 3", len(m.Root.SubParts))
	}
	if len(got) != 2 || string(got["1"].buf) != "first" || string(got["3"].buf) != "second" {
		t.Errorf("captured %d parts (%q, %q), want the two text parts", len(got), got["1"].buf, got["3"].buf)
	}
	// The attachment's broken base64 was never decoded, so no problem is
	// reported for it - the walk simply read past it and found the part after.
	if att := m.Root.SubParts[1]; att.EncodingProblem || att.Size != 0 {
		t.Errorf("the skipped attachment was decoded: problem=%v size=%d", att.EncodingProblem, att.Size)
	}
}
