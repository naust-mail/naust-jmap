package message

// Two behaviours on malformed input are fixed deliberately, to match what mature
// mail stores produce (RFC 2045 6.8, RFC 2046 5.1.1). These pin the decided
// answers as fixed expectations rather than by agreement with the oracle: the
// oracle was changed in step with the parser, so only a fixed value proves the
// two did not drift back to the old behaviour together.

import (
	"strings"
	"testing"
)

// TestBase64StopsAtPadding: padding marks the end of the encoded data
// (RFC 2045 6.8). "QUJDRA==" is the whole of "ABCD"; base64 characters after the
// padding are trailing content, so they are dropped and the part is flagged as
// an encoding problem, not decoded into extra octets. Decoding past the padding
// would change the content's size and digest, and so its blobId (RFC 8620 6.1).
func TestBase64StopsAtPadding(t *testing.T) {
	const trailing = "Content-Type: text/plain\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nQUJDRA==\r\nQ\r\n"
	m, err := Parse(strings.NewReader(trailing), captureAll)
	if err != nil {
		t.Fatal(err)
	}
	if m.Root.Size != 4 {
		t.Errorf("decoded %d octets, want 4 (\"ABCD\", stopping at the padding)", m.Root.Size)
	}
	if !m.Root.EncodingProblem {
		t.Error("trailing base64 after the padding must flag an encoding problem")
	}

	// The same encoded data without trailing content decodes to the same octets
	// and is not a problem: padding at the end of the data is normal.
	const clean = "Content-Type: text/plain\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nQUJDRA==\r\n"
	m, err = Parse(strings.NewReader(clean), captureAll)
	if err != nil {
		t.Fatal(err)
	}
	if m.Root.Size != 4 || m.Root.EncodingProblem {
		t.Errorf("clean padded base64: size=%d problem=%v, want 4 and false", m.Root.Size, m.Root.EncodingProblem)
	}
}

// TestBoundarylessMultipartDegradesToLeaf: a multipart declares a boundary to
// separate its parts (RFC 2046 5.1.1); with none - or an empty one - its body
// cannot be split, so it is one part. It becomes a readable text/plain leaf
// rather than a childless multipart, so the body is surfaced rather than
// discarded as a multipart's preamble.
func TestBoundarylessMultipartDegradesToLeaf(t *testing.T) {
	cases := map[string]string{
		"no boundary parameter": "Content-Type: multipart/mixed\r\n\r\nplain body",
		"empty boundary":        "Content-Type: multipart/mixed; boundary=\"\"\r\n\r\nplain body",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := Parse(strings.NewReader(raw), captureAll)
			if err != nil {
				t.Fatal(err)
			}
			if m.Root.SubParts != nil {
				t.Errorf("degraded part has children: %v", m.Root.SubParts)
			}
			if m.Root.Type != "text/plain" {
				t.Errorf("type = %q, want text/plain", m.Root.Type)
			}
			if m.Root.PartID == "" {
				t.Error("no partId: the degraded part is a leaf and must have one")
			}
			if m.Root.Size != uint64(len("plain body")) {
				t.Errorf("content = %d octets, want %d (the body must be readable)", m.Root.Size, len("plain body"))
			}
		})
	}
}
