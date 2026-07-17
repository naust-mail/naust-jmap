package message

// The multipart walker's hard cases, each run through the differential harness:
// whatever a slicing parser made of these, the streaming one must make of them
// too. A slicing parser cuts the outer body first and recurses into the pieces,
// so it gets several of these right by construction - an outer delimiter ending
// an inner part, a missing close delimiter, an epilogue - where the streaming
// walker has to be told. These are the cases a fuzzer is unlikely to build.

import (
	"strings"
	"testing"
)

// walkCases are messages whose structure exercises the boundary stack. Each is
// asserted against the frozen buffered parser, so the expectation is not a
// guess: it is what the parser being replaced did.
var walkCases = map[string]string{
	// Every line of this part begins at a line start with '-', so each is a
	// delimiter candidate, and none is one: bare dashes, a signature separator,
	// lines sharing the boundary's prefix ("--b-", "--bx"), a candidate line
	// too long to be a delimiter, and a candidate cut short by the close
	// delimiter right behind it. All of it is content, octet for octet.
	"dash lines that are not delimiters": crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

-x
-
--
--b- almost
--bx not it
-- signature --
`) + "-" + strings.Repeat("y", maxDelimLine+50) + "\r\n--\r\n--b--\r\n",
	"outer delimiter ends inner part": crlf(`Content-Type: multipart/mixed; boundary=out

--out
Content-Type: multipart/mixed; boundary=in

--in
Content-Type: text/plain

inner text
--out
Content-Type: text/plain

after the inner multipart was cut short
--out--
`),
	"inner multipart never closes": crlf(`Content-Type: multipart/mixed; boundary=out

--out
Content-Type: multipart/alternative; boundary=in

--in
Content-Type: text/plain

a
--in
Content-Type: text/html

<p>a</p>
--out--
`),
	"no close delimiter at all": crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

body that just stops
`),
	"same boundary nested": crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

whose part am I
--b--
`),
	"empty parts": crlf(`Content-Type: multipart/mixed; boundary=b

--b
--b

--b
Content-Type: text/plain

x
--b--
`),
	"part with headers but no body": crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain
Content-Disposition: inline
--b--
`),
	"body is only delimiters": crlf(`Content-Type: multipart/mixed; boundary=b

--b
--b
--b--
`),
	"delimiter lookalikes in preamble and epilogue": crlf(`Content-Type: multipart/mixed; boundary=b

--b-x
--bb
not a delimiter
--b
Content-Type: text/plain

x
--b--
--b
epilogue that looks like a part
--b--
`),
	"boundary text inside a part body": crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

a line that mentions --b in the middle of it
 --b indented, so not a delimiter
--b--
`),
	"mixed CRLF and bare LF delimiters": "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\nContent-Type: text/plain\n\nbare lf part\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\ncrlf part\r\n--b--\r\n",
	"delimiter with trailing whitespace": crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

x
--b--
`),
	"preamble and epilogue text": crlf(`Content-Type: multipart/mixed; boundary=b

this preamble is discarded
--b
Content-Type: text/plain

x
--b--
this epilogue is discarded too
`),
	"blank line before delimiter": crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

x

--b--
`),
	"digest default type": crlf(`Content-Type: multipart/digest; boundary=b

--b

Subject: embedded

hi
--b--
`),
	"base64 part holding boundary-like text": crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: application/octet-stream
Content-Transfer-Encoding: base64

LS1iCg==
--b--
`),
}

func TestWalkAdversarial(t *testing.T) {
	for name, raw := range walkCases {
		t.Run(name, func(t *testing.T) {
			assertSameAsOracle(t, []byte(raw))
		})
	}
}

// TestWalkBoundaryBomb: a message declaring far more parts than maxParts builds
// no more than the cap, and the streaming walker still reads to the end of the
// message - the parts it refuses to build are read past, not left in the stream.
func TestWalkBoundaryBomb(t *testing.T) {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=b\r\n\r\n")
	for i := 0; i < maxParts*3; i++ {
		b.WriteString("--b\r\nContent-Type: text/plain\r\n\r\nx\r\n")
	}
	b.WriteString("--b--\r\nepilogue\r\n")
	assertSameAsOracle(t, []byte(b.String()))

	m := parseDoc(t, []byte(b.String()))
	if total := 1 + len(m.Root.SubParts); total > maxParts {
		t.Errorf("built %d parts, want <= maxParts (%d)", total, maxParts)
	}
}

// TestWalkDeepNesting: nesting past maxMultipartDepth is treated as a leaf
// multipart with no children, and the walker does not recurse itself to death.
func TestWalkDeepNesting(t *testing.T) {
	var b strings.Builder
	depth := maxMultipartDepth * 2
	b.WriteString("Content-Type: multipart/mixed; boundary=b0\r\n\r\n")
	for i := 1; i < depth; i++ {
		b.WriteString("--b" + itoa(i-1) + "\r\n")
		b.WriteString("Content-Type: multipart/mixed; boundary=b" + itoa(i) + "\r\n\r\n")
	}
	b.WriteString("--b" + itoa(depth-1) + "\r\nContent-Type: text/plain\r\n\r\ndeep\r\n")
	for i := depth - 1; i >= 0; i-- {
		b.WriteString("--b" + itoa(i) + "--\r\n")
	}
	assertSameAsOracle(t, []byte(b.String()))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	return string(d)
}

// TestWalkOverLongDelimiterLine pins the second place the streaming walker
// deliberately parts company with the buffered one: RFC 2046 5.1.1 permits
// trailing white space after a delimiter, and the buffered parser allowed it
// without limit. The walker decides a delimiter from a bounded prefix of the
// line (maxDelimLine), so a "delimiter" padded past that is content instead.
// A boundary is at most 70 characters (5.1.1) and a line at most 998 octets
// (RFC 5322 2.1.1), so no legitimate message comes close.
func TestWalkOverLongDelimiterLine(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nx\r\n" +
		"--b" + strings.Repeat(" ", maxDelimLine) + "\r\n" +
		"still the same part\r\n--b--\r\n"
	m := parseDoc(t, []byte(raw))
	if len(m.Root.SubParts) != 1 {
		t.Fatalf("got %d parts, want 1: the padded line is content, not a delimiter", len(m.Root.SubParts))
	}
	if !strings.Contains(string(m.Content[m.Root.SubParts[0]]), "still the same part") {
		t.Error("the padded delimiter line and what follows it belong to the part's content")
	}
	// A delimiter padded up to the limit is still a delimiter: the cut is the
	// only thing that disqualifies a line, not the padding itself.
	ok := "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nx\r\n" +
		"--b" + strings.Repeat(" ", 100) + "\r\nContent-Type: text/plain\r\n\r\ny\r\n--b--\r\n"
	assertSameAsOracle(t, []byte(ok))
	if n := len(parseDoc(t, []byte(ok)).Root.SubParts); n != 2 {
		t.Errorf("got %d parts, want 2: trailing white space is allowed on a delimiter", n)
	}
}
