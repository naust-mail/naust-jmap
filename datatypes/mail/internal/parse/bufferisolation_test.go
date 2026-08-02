package parse

// Guard for the walk's shared copy buffer: content flows to every sink
// through one buffer reused leaf after leaf, which is safe only while
// every sink copies what it keeps inside its Write call (the io.Writer
// contract). A sink that retains the slice instead would show a later
// part's bytes inside an earlier part's captured value. This test
// parses a message whose every part carries distinct content through
// the real production sinks and checks each captured value is exactly
// its own part's content - cross-part bleed of any kind fails it.

import (
	"fmt"
	"strings"
	"testing"
)

func TestSharedBufferSinkIsolation(t *testing.T) {
	const parts = 300
	var b strings.Builder
	b.WriteString("MIME-Version: 1.0\r\nFrom: a@example.com\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 0; i < parts; i++ {
		b.WriteString("--B\r\nContent-Type: text/plain\r\n\r\n")
		fmt.Fprintf(&b, "content-of-part-%04d\r\n", i)
	}
	b.WriteString("--B--\r\n")

	c := NewCapture()
	c.Identity, c.Values, c.Preview = true, true, true
	p, err := ParseMessage(strings.NewReader(b.String()), c)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(p.Msg.Root.SubParts); got != parts {
		t.Fatalf("parts = %d, want %d", got, parts)
	}
	for i, part := range p.Msg.Root.SubParts {
		v, problem, truncated, ok := p.BodyValue(part)
		if !ok || problem || truncated {
			t.Fatalf("part %d: value ok=%v problem=%v truncated=%v", i, ok, problem, truncated)
		}
		// The CRLF before each boundary delimiter belongs to the
		// boundary (RFC 2046 section 5.1.1), so the captured value ends
		// without it.
		if want := fmt.Sprintf("content-of-part-%04d", i); v != want {
			t.Fatalf("part %d captured %q, want %q - a sink is retaining the shared copy buffer", i, v, want)
		}
	}
}
