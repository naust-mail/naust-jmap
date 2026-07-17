package message

import (
	"bytes"
	"testing"
)

// TestNoSinkNoDecode is the parser's core guarantee: a leaf whose factory
// declares no sinks has its content left alone. Nothing decodes it, nothing
// hashes it, and no consequence of decoding it can be observed - which is what
// lets an unauthenticated delivery walk a hostile message for its structure
// without ever touching an attachment's octets.
func TestNoSinkNoDecode(t *testing.T) {
	// Base64 that would decode with a problem flagged if it were decoded at all.
	raw := []byte("Content-Transfer-Encoding: base64\r\n\r\n!!!not base64!!!\r\n")

	for _, tc := range []struct {
		name    string
		factory SinkFactory
	}{
		{"no factory", nil},
		{"empty sinks", func(*Part) LeafSinks { return LeafSinks{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := Parse(bytes.NewReader(raw), tc.factory)
			if err != nil {
				t.Fatal(err)
			}
			p := m.Root
			if p.Type != "text/plain" || p.PartID != "1" {
				t.Fatalf("structure lost: type=%q partId=%q", p.Type, p.PartID)
			}
			if p.EncodingProblem {
				t.Error("EncodingProblem set: the content was decoded although no sink asked for it")
			}
			if p.Size != 0 || p.Digest != [32]byte{} {
				t.Errorf("content identity produced unasked: size=%d digest=%x", p.Size, p.Digest)
			}
		})
	}
}

// TestIdentityWithoutSinks: identity alone decodes the content and yields the
// part's size and digest (the blobId of RFC 8620 6.1), with no sink involved.
func TestIdentityWithoutSinks(t *testing.T) {
	raw := []byte("Content-Transfer-Encoding: base64\r\n\r\naGVsbG8=\r\n")
	m, err := Parse(bytes.NewReader(raw), func(*Part) LeafSinks {
		return LeafSinks{Identity: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Root.Size != 5 {
		t.Errorf("size = %d, want 5 (the decoded octets)", m.Root.Size)
	}
	plain, err := Parse(bytes.NewReader([]byte("\r\nhello")), func(*Part) LeafSinks {
		return LeafSinks{Identity: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	// The digest is of the decoded content, so the same content under a
	// different transfer encoding is the same blob (RFC 8621 4.1.4).
	if m.Root.Digest != plain.Root.Digest {
		t.Error("identical decoded content produced different digests")
	}
}

// TestSinkFactorySeesLeavesOnly: a multipart node carries no content of its own,
// so the factory is never consulted for one and no identity is produced for it.
func TestSinkFactorySeesLeavesOnly(t *testing.T) {
	raw := []byte(crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

hi
--b--
`))
	var seen []string
	m, err := Parse(bytes.NewReader(raw), func(p *Part) LeafSinks {
		seen = append(seen, p.Type)
		return LeafSinks{Identity: true}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "text/plain" {
		t.Errorf("factory saw %v, want only the leaf", seen)
	}
	if m.Root.Size != 0 || m.Root.Digest != [32]byte{} {
		t.Errorf("multipart root has content identity: size=%d", m.Root.Size)
	}
}

// TestSinkReceivesDecodedContent: every declared sink is fed the same decoded
// octets, and its Close runs once the part has ended.
func TestSinkReceivesDecodedContent(t *testing.T) {
	raw := []byte("Content-Transfer-Encoding: quoted-printable\r\n\r\ncaf=C3=A9\r\n")
	a, b := &captureSink{}, &captureSink{}
	closed := 0
	m, err := Parse(bytes.NewReader(raw), func(*Part) LeafSinks {
		return LeafSinks{Identity: true, Sinks: []Sink{a, b, closeCounter{&closed}}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(a.buf) != "café\r\n" || string(b.buf) != string(a.buf) {
		t.Errorf("sinks got %q and %q", a.buf, b.buf)
	}
	if uint64(len(a.buf)) != m.Root.Size {
		t.Errorf("size %d does not match the octets the sinks saw (%d)", m.Root.Size, len(a.buf))
	}
	if closed != 1 {
		t.Errorf("sink closed %d times, want once", closed)
	}
}

type closeCounter struct{ n *int }

func (c closeCounter) Write(b []byte) (int, error) { return len(b), nil }
func (c closeCounter) Close() error                { *c.n++; return nil }
