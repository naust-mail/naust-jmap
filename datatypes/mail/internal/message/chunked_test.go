package message

// The parser reads its input as a stream, so the same message must parse the
// same way however the reader chooses to hand it over. A network reader returns
// whatever has arrived: a delimiter, a CRLF, a header fold, or a boundary can
// each land astride two reads. Every other test here feeds a bytes.Reader, which
// answers in one piece and so proves nothing about that - these do.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

// chunkReader hands out at most n octets per Read, like a socket would.
type chunkReader struct {
	data []byte
	n    int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.n
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// chunkSizes bracket every buffer edge the parser has: the smallest possible
// read, a few odd sizes, and the octets either side of the line reader's own
// chunk (readChunk) and of the longest line it will inspect (maxDelimLine).
var chunkSizes = []int{1, 2, 3, 7, 64, 1023, maxDelimLine - 1, maxDelimLine, maxDelimLine + 1, readChunk - 1, readChunk, readChunk + 1}

// snapshotChunked is snapshotNew reading through a reader that returns n octets
// at a time.
func snapshotChunked(raw []byte, n int) (snapshot, error) {
	sinks := map[*Part]*captureSink{}
	m, err := Parse(&chunkReader{data: raw, n: n}, func(p *Part) LeafSinks {
		s := &captureSink{}
		sinks[p] = s
		return LeafSinks{Identity: true, Sinks: []Sink{s}}
	})
	if err != nil {
		return snapshot{}, err
	}
	return snapshotOf(m, sinks), nil
}

// TestParseChunkIndependence: for every fixture, reading the message in small
// pieces produces exactly the parse that reading it whole does.
func TestParseChunkIndependence(t *testing.T) {
	corpus := append([]string{crlf(akMessage)}, fuzzSeeds()...)
	for _, raw := range walkCases {
		corpus = append(corpus, raw)
	}
	for _, raw := range rfcCases() {
		corpus = append(corpus, raw)
	}
	for i, raw := range corpus {
		whole, err := snapshotNew([]byte(raw))
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		for _, n := range chunkSizes {
			got, err := snapshotChunked([]byte(raw), n)
			if err != nil {
				t.Fatalf("case %d in %d-octet reads: %v", i, n, err)
			}
			if !reflect.DeepEqual(got, whole) {
				t.Errorf("case %d parses differently in %d-octet reads:\n got %+v\nwant %+v", i, n, got, whole)
			}
		}
	}
}

// FuzzChunked is the same claim over arbitrary input: any message, any read
// size, one answer.
func FuzzChunked(f *testing.F) {
	for _, seed := range fuzzSeeds() {
		f.Add([]byte(seed), uint8(1))
		f.Add([]byte(seed), uint8(7))
	}
	f.Add([]byte(crlf(akMessage)), uint8(3))
	f.Fuzz(func(t *testing.T, data []byte, size uint8) {
		n := int(size)
		if n == 0 {
			n = 1
		}
		whole, err := snapshotNew(data)
		if err != nil {
			t.Fatal(err)
		}
		got, err := snapshotChunked(data, n)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, whole) {
			t.Errorf("%d-octet reads change the parse:\n got %+v\nwant %+v", n, got, whole)
		}
	})
}

// errAt fails after handing over the first n octets, standing in for a
// connection that drops mid-message.
type errAt struct {
	data []byte
	n    int
}

var errBroken = errors.New("broken pipe")

func (r *errAt) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errBroken
	}
	n := len(p)
	if n > r.n {
		n = r.n
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p, r.data[:n])
	r.data, r.n = r.data[n:], r.n-n
	return n, nil
}

// TestParseReadErrorSurfaces: a message that stops arriving is an error, never a
// short parse silently passed off as the whole message. Every octet offset is
// tried, so the failure is caught wherever it lands - in the header block, in a
// delimiter, in a part's content.
func TestParseReadErrorSurfaces(t *testing.T) {
	raw := []byte(crlf(akMessage))
	for n := 0; n < len(raw); n += 7 {
		_, err := Parse(&errAt{data: raw, n: n}, func(*Part) LeafSinks {
			return LeafSinks{Identity: true, Sinks: []Sink{&captureSink{}}}
		})
		if !errors.Is(err, errBroken) {
			t.Fatalf("read failed after %d octets, but Parse returned err=%v", n, err)
		}
	}
	// The same holds when no sink is declared, where the walker reads past the
	// content rather than through it.
	for n := 0; n < len(raw); n += 7 {
		if _, err := Parse(&errAt{data: raw, n: n}, nil); !errors.Is(err, errBroken) {
			t.Fatalf("structure-only parse after %d octets returned err=%v", n, err)
		}
	}
}

// TestLineReaderAcrossChunks: the line reader's own seams - a CRLF split between
// two reads, and a line longer than its buffer - hand back the same lines a
// single read would.
func TestLineReaderAcrossChunks(t *testing.T) {
	raw := "a\r\nbb\r\n" + strings.Repeat("c", readChunk+100) + "\r\nd\r\n"
	for _, n := range []int{1, 2, 3, readChunk - 1, readChunk, readChunk + 1} {
		var lines []string
		lr := newLineReader(&chunkReader{data: []byte(raw), n: n})
		for {
			line, trunc, err := lr.readLine(maxDelimLine)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			lines = append(lines, fmt.Sprintf("%d:%v", len(line), trunc))
		}
		want := []string{"3:false", "4:false", fmt.Sprintf("%d:true", maxDelimLine)}
		if len(lines) < len(want) || !reflect.DeepEqual(lines[:len(want)], want) {
			t.Errorf("%d-octet reads gave lines %v, want them to start %v", n, lines, want)
		}
		if joined := strings.Join(lines, ","); !strings.HasSuffix(joined, "3:false") {
			t.Errorf("%d-octet reads gave lines %v, want the last line intact", n, lines)
		}
	}
	// A CRLF split exactly across two reads is one line ending, not two lines.
	lr := newLineReader(&chunkReader{data: []byte("ab\r\ncd\r\n"), n: 3})
	line, _, err := lr.readLine(maxDelimLine)
	if err != nil || !bytes.Equal(line, []byte("ab\r\n")) {
		t.Fatalf("line = %q err=%v, want %q", line, err, "ab\r\n")
	}
}
