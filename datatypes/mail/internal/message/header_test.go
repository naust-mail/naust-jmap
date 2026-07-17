package message

// The header block is read as a stream: a field is decided from a bounded
// prefix of its line, and no line is ever buffered whole. These tests pin the
// seams that creates - the prefix boundary, an over-long line, and the one
// place the streaming reader deliberately parts company with the buffered one.

import (
	"bytes"
	"strings"
	"testing"
)

func headersOf(t *testing.T, raw string) ([]HeaderField, string) {
	t.Helper()
	lr := newLineReader(strings.NewReader(raw))
	headers, err := readHeaderBlock(lr)
	if err != nil {
		t.Fatalf("readHeaderBlock: %v", err)
	}
	var body bytes.Buffer
	if _, err := body.ReadFrom(lr); err != nil {
		t.Fatalf("body: %v", err)
	}
	return headers, body.String()
}

// TestHeaderBodySplit: every way the header block can end hands the body exactly
// the octets that follow it - including the line that ended the block, when that
// line is the body's first (malformed input recovers by starting the body there).
func TestHeaderBodySplit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		fields  int
		body    string
		comment string
	}{
		{"blank line", "A: 1\r\nB: 2\r\n\r\nbody\r\n", 2, "body\r\n", "the separator is consumed"},
		{"no colon", "A: 1\r\nthis is body\r\nmore\r\n", 1, "this is body\r\nmore\r\n", "the line starts the body"},
		{"leading fold", " folded first\r\nrest\r\n", 0, " folded first\r\nrest\r\n", "a fold with no field is body"},
		{"bad name", "A: 1\r\nnot a name: v\r\n", 1, "not a name: v\r\n", "a space in the name is not a field"},
		{"empty name", ": v\r\nx\r\n", 0, ": v\r\nx\r\n", "an empty name is not a field"},
		{"eof in headers", "A: 1\r\nB: 2\r\n", 2, "", "no body at all"},
		{"empty message", "", 0, "", "nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers, body := headersOf(t, tc.raw)
			if len(headers) != tc.fields {
				t.Errorf("got %d fields, want %d (%s)", len(headers), tc.fields, tc.comment)
			}
			if body != tc.body {
				t.Errorf("body = %q, want %q (%s)", body, tc.body, tc.comment)
			}
		})
	}
}

// TestHeaderValueCapStreamed: a value folded across many lines is capped at
// maxHeaderValue, and the octets past the cap are dropped without being held -
// the line they sit on is read and discarded, never buffered.
func TestHeaderValueCapStreamed(t *testing.T) {
	raw := "Subject: x\r\n" + strings.Repeat(" "+strings.Repeat("y", 900)+"\r\n", 200) + "\r\nbody\r\n"
	headers, body := headersOf(t, raw)
	if len(headers) != 1 {
		t.Fatalf("got %d fields, want 1", len(headers))
	}
	if n := len(headers[0].Value); n > maxHeaderValue {
		t.Errorf("value = %d octets, want <= %d", n, maxHeaderValue)
	}
	if body != "body\r\n" {
		t.Errorf("body = %q: the capped fold lines must not leak into it", body)
	}
}

// TestHeaderOverLongLine: a single field line longer than the prefix the reader
// decides from still yields the field, with its value capped, and the rest of
// that line is dropped rather than becoming body.
func TestHeaderOverLongLine(t *testing.T) {
	raw := "Subject: " + strings.Repeat("z", maxHeaderLine+5000) + "\r\nTo: a@b\r\n\r\nbody\r\n"
	headers, body := headersOf(t, raw)
	if len(headers) != 2 || headers[0].Name != "Subject" || headers[1].Name != "To" {
		t.Fatalf("fields = %v", headers)
	}
	if n := len(headers[0].Value); n != maxHeaderValue {
		t.Errorf("value = %d octets, want the cap (%d)", n, maxHeaderValue)
	}
	if body != "body\r\n" {
		t.Errorf("body = %q, want the real body: the line's tail must be dropped", body)
	}
}

// TestHeaderNameCap is the one place the streaming reader deliberately differs
// from the buffered parser it replaces: a "field name" longer than maxHeaderName
// is not treated as a field. RFC 5322 2.1.1 limits a whole line to 998 octets,
// so no legitimate message reaches this; the buffered parser would have accepted
// the line and retained a name of unbounded length, which is precisely the
// unbounded retention the streaming parser exists to remove. The line becomes
// the first line of the body instead.
func TestHeaderNameCap(t *testing.T) {
	name := strings.Repeat("n", maxHeaderName+1)
	headers, body := headersOf(t, "A: 1\r\n"+name+": v\r\nrest\r\n")
	if len(headers) != 1 || headers[0].Name != "A" {
		t.Fatalf("fields = %v, want only the well-formed one", headers)
	}
	if !strings.HasPrefix(body, name+": v\r\n") {
		t.Errorf("body = %.40q..., want the over-long line to start it", body)
	}
	// A name right at the cap is still a field.
	headers, _ = headersOf(t, strings.Repeat("n", maxHeaderName)+": v\r\n\r\n")
	if len(headers) != 1 {
		t.Errorf("a name of exactly maxHeaderName octets must still parse: %v", headers)
	}
}

// TestLineReaderPushback: the reader hands back a line unchanged, whatever the
// line ending, and the octets after it are still there.
func TestLineReaderPushback(t *testing.T) {
	for _, raw := range []string{"one\r\ntwo\r\n", "one\ntwo\n", "one\r\ntwo"} {
		lr := newLineReader(strings.NewReader(raw))
		line, trunc, err := lr.readLine(maxHeaderLine)
		if err != nil || trunc {
			t.Fatalf("readLine(%q) = %q trunc=%v err=%v", raw, line, trunc, err)
		}
		lr.unread(line)
		var all bytes.Buffer
		if _, err := all.ReadFrom(lr); err != nil {
			t.Fatal(err)
		}
		if all.String() != raw {
			t.Errorf("after pushback the stream is %q, want %q", all.String(), raw)
		}
	}
}

// TestLineReaderTruncation: a line longer than the limit comes back cut, with
// the rest of it still unread, and discardLine drops exactly that rest.
func TestLineReaderTruncation(t *testing.T) {
	long := strings.Repeat("x", 100)
	lr := newLineReader(strings.NewReader(long + "\r\nnext\r\n"))
	line, trunc, err := lr.readLine(10)
	if err != nil || !trunc || string(line) != long[:10] {
		t.Fatalf("readLine = %q trunc=%v err=%v", line, trunc, err)
	}
	if err := lr.discardLine(); err != nil {
		t.Fatal(err)
	}
	line, trunc, err = lr.readLine(10)
	if err != nil || trunc || string(line) != "next\r\n" {
		t.Fatalf("after discard: %q trunc=%v err=%v", line, trunc, err)
	}
	// A line that ends exactly at the limit is not truncated.
	lr = newLineReader(strings.NewReader("abcdefghij\r\nz"))
	line, trunc, err = lr.readLine(10)
	if err != nil || trunc {
		t.Fatalf("line at exactly the limit: %q trunc=%v err=%v", line, trunc, err)
	}
}
