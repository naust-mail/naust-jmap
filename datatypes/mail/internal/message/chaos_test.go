package message

// Messages built to hurt the parser. Everything here is something a sender can
// put on the wire, and a mail server parses what it is sent: it does not get to
// require a well-formed message, and it does not get to fall over on a malformed
// one. Two kinds of hostile input are covered.
//
// The structural cases are small and go through the differential harness, so
// what they must produce is not a guess: it is what the parser being replaced
// produced. The bombs are large and go through the live-heap sampler, because
// their whole point is to make the parser hold something - a header block, a
// preamble, an epilogue, a line without an ending - that a careless
// implementation would keep and a streaming one must read past.

import (
	"io"
	"reflect"
	"strings"
	"testing"
)

// chaosCases: malformed, adversarial, and merely deranged structures.
var chaosCases = map[string]string{
	// A boundary is matched as a whole token, not as a prefix: a sender who
	// declares "b" must not have their parts cut by a "--bb" line, and a nested
	// multipart whose boundary extends the outer one must keep its own parts.
	"boundary is a prefix of another boundary": crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: multipart/mixed; boundary=bb

--bb
Content-Type: text/plain

inner, delimited by the longer boundary
--bb--
--b
Content-Type: text/plain

outer part, after the inner multipart closed
--b--
`),
	// RFC 2046 5.1.1 allows transport padding - white space - after a delimiter.
	"delimiter with trailing white space": "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b \t \r\nContent-Type: text/plain\r\n\r\npadded delimiter\r\n--b--  \r\n",
	// A message can stop anywhere, including in the middle of a delimiter.
	"message ends inside a delimiter":  "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\nbody\r\n--b",
	"message ends on a lone dash":      "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\nbody\r\n--b-",
	"message ends in the header block": "Content-Type: multipart/mixed;\r\n boundary=b",
	"nothing but a blank line":         "\r\n",
	"nothing at all":                   "",
	"a single line ending":             "\n",
	// A boundary the sender chose to make awkward: it looks like a delimiter, it
	// contains the delimiter marker, and it needs quoting.
	"boundary containing dashes":        "Content-Type: multipart/mixed; boundary=\"--x--\"\r\n\r\n----x--\r\nContent-Type: text/plain\r\n\r\ncontent\r\n----x----\r\n",
	"boundary quoted with white space":  "Content-Type: multipart/mixed; boundary=\"b b\"\r\n\r\n--b b\r\nContent-Type: text/plain\r\n\r\ncontent\r\n--b b--\r\n",
	"empty boundary parameter":          "Content-Type: multipart/mixed; boundary=\"\"\r\n\r\n--\r\nContent-Type: text/plain\r\n\r\ncontent\r\n----\r\n",
	"multipart with no boundary at all": "Content-Type: multipart/mixed\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\ncontent\r\n--b--\r\n",
	// Header blocks that are not header blocks.
	"part with no header block":    "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nstraight into content\r\n--b--\r\n",
	"header line with no colon":    "Content-Type: text/plain\r\nthis is not a header\r\n\r\nbody\r\n",
	"fold with no field before it": "Content-Type: text/plain\r\n   orphan fold\r\n\r\nbody\r\n",
	"colon in the first column":    ": no name\r\n\r\nbody\r\n",
	"header block never ends":      "Subject: a\r\nFrom: b\r\nTo: c\r\n",
	"content-type declared twice":  "Content-Type: text/html\r\nContent-Type: text/plain\r\n\r\nwhich one wins\r\n",
	"content-type is nonsense":     "Content-Type: not a media type at all\r\n\r\nbody\r\n",
	"content-type with no subtype": "Content-Type: text\r\n\r\nbody\r\n",
	"nul octets in a header value": "Subject: null\x00byte\r\nContent-Type: text/plain\r\n\r\nbo\x00dy\r\n",
	"eight bit octets in a header": "Subject: \xff\xfe\xfd\r\nContent-Type: text/plain\r\n\r\nbody\r\n",
	"bare LF line endings":         "Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\n\nunix line endings\n--b--\n",
	"mixed CRLF and LF":            "Content-Type: multipart/mixed; boundary=b\r\n\n--b\nContent-Type: text/plain\r\n\nmixed\r\n--b--\n",
	"lone CR inside a part":        "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\na\rb\r\n--b--\r\n",
	"delimiter looking line inside base64": "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n" +
		"Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nLS1i\r\n--b--\r\n",
	// Content-Transfer-Encoding a sender got wrong, on purpose or otherwise.
	"base64 with an impossible length":   "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nQUJDRA==\r\nQ\r\n",
	"base64 with foreign octets":         "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\nQU!J@DR#A=\r\n",
	"base64 that is only padding":        "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n====\r\n",
	"quoted-printable broken escapes":    "Content-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\na=ZZb=3Dc=\r\nd=\r\n",
	"quoted-printable ends on an escape": "Content-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nends here =",
	"unknown transfer encoding":          "Content-Type: text/plain\r\nContent-Transfer-Encoding: x-made-up\r\n\r\nbody\r\n",
	"transfer encoding in odd case":      "Content-Type: text/plain\r\nContent-Transfer-Encoding: BaSe64\r\n\r\nQUJDRA==\r\n",
	// Parts that are empty in every way a part can be empty.
	"empty parts throughout": "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\n--b\r\n\r\n--b\r\n\r\n--b--\r\n",
	"close delimiter first":  "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b--\r\n--b\r\nContent-Type: text/plain\r\n\r\nafter the close\r\n",
	"two close delimiters":   "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\nx\r\n--b--\r\n--b--\r\n",
	"digest default type":    "Content-Type: multipart/digest; boundary=b\r\n\r\n--b\r\n\r\nSubject: embedded\r\n\r\nthe default type here is message/rfc822\r\n--b--\r\n",
}

// TestChaosCorpus runs every malformed structure through the differential
// harness: the streaming parser must make of each exactly what the buffered one
// made of it.
func TestChaosCorpus(t *testing.T) {
	for name, raw := range chaosCases {
		t.Run(name, func(t *testing.T) {
			assertSameAsOracle(t, []byte(raw))
		})
	}
}

// TestChaosCorpusChunked: the same messages, read one octet at a time. A parser
// that is right only when the message arrives in one piece is not right.
func TestChaosCorpusChunked(t *testing.T) {
	for name, raw := range chaosCases {
		t.Run(name, func(t *testing.T) {
			whole, err := snapshotNew([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range []int{1, 2, 3} {
				got, err := snapshotChunked([]byte(raw), n)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, whole) {
					t.Errorf("%d-octet reads change the parse:\n got %+v\nwant %+v", n, got, whole)
				}
			}
		})
	}
}

// repeating produces the same octets forever, so a test can hand the parser a
// message far larger than the test itself could hold.
type repeating struct {
	s   string
	off int
}

func (r *repeating) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.s[r.off:])
		r.off = (r.off + c) % len(r.s)
		n += c
	}
	return n, nil
}

// flood is about size octets of s, repeated a whole number of times - a bomb cut
// off in the middle of its pattern would put a half line in front of whatever
// follows it, which is a different test from the one intended.
func flood(s string, size int) io.Reader {
	return io.LimitReader(&repeating{s: s}, int64(floodSize(s, size)))
}

// floodSize is how many octets flood actually produces.
func floodSize(s string, size int) int { return size / len(s) * len(s) }

const bombSize = 8 << 20

// captureAll is the factory that asks for everything, which is the worst case:
// no bomb below is allowed to cost memory even when every leaf is being decoded
// and hashed.
func captureAll(*Part) LeafSinks { return LeafSinks{Identity: true} }

// TestBombsAreBounded: each of these messages is designed to make a parser hold
// something. None of them may. The parse is also required to still be correct
// afterwards - reading past a bomb is not the same as losing the message behind
// it.
func TestBombsAreBounded(t *testing.T) {
	cases := map[string]struct {
		msg   func() io.Reader
		check func(t *testing.T, m *Message)
		// limit is the heap the parse may hold, when what it is entitled to hold is
		// not simply "next to nothing".
		limit uint64
	}{
		// A message with no line ending anywhere: every line-oriented parser has
		// somewhere it accumulates a line, and this is what finds it.
		"a single line of eight megabytes": {
			msg: func() io.Reader {
				return io.MultiReader(
					strings.NewReader("Content-Type: text/plain\r\n\r\n"),
					flood("no line endings in here at all ", bombSize),
				)
			},
			check: func(t *testing.T, m *Message) {
				if want := uint64(floodSize("no line endings in here at all ", bombSize)); m.Root.Size != want {
					t.Errorf("part size %d, want %d", m.Root.Size, want)
				}
			},
		},
		// A header block that never becomes a body. The field cap ends it; the rest
		// must be read past, not kept.
		"eight megabytes of header fields": {
			msg: func() io.Reader { return flood("X-Filler: filling\r\n", bombSize) },
			check: func(t *testing.T, m *Message) {
				if len(m.Headers) != maxHeaders {
					t.Errorf("kept %d header fields, want the cap of %d", len(m.Headers), maxHeaders)
				}
			},
		},
		// One field, folded across the whole message: the value cap must bound what
		// is retained, and the fold must not become quadratic.
		"one header field folded across eight megabytes": {
			msg: func() io.Reader {
				return io.MultiReader(
					strings.NewReader("Subject: start\r\n"),
					flood(" folded continuation\r\n", bombSize),
				)
			},
			check: func(t *testing.T, m *Message) {
				if len(m.Headers) != 1 {
					t.Fatalf("got %d header fields, want 1", len(m.Headers))
				}
				if v := len(m.Headers[0].Value); v > maxHeaderValue+64 {
					t.Errorf("header value is %d octets, want it capped at %d", v, maxHeaderValue)
				}
			},
		},
		// Everything before the first delimiter is preamble, and everything after
		// the close delimiter is epilogue. Neither is a part, and neither may be
		// held while the parser looks for what is.
		"eight megabyte preamble and epilogue": {
			msg: func() io.Reader {
				return io.MultiReader(
					strings.NewReader("Content-Type: multipart/mixed; boundary=b\r\n\r\n"),
					flood("preamble text that is not a part\r\n", bombSize),
					strings.NewReader("--b\r\nContent-Type: text/plain\r\n\r\nthe only part\r\n--b--\r\n"),
					flood("epilogue text that is not a part\r\n", bombSize),
				)
			},
			check: func(t *testing.T, m *Message) {
				if len(m.Root.SubParts) != 1 {
					t.Fatalf("got %d parts, want the one real part", len(m.Root.SubParts))
				}
				if got := m.Root.SubParts[0].Size; got != uint64(len("the only part")) {
					t.Errorf("the part's content is %d octets, want %d", got, len("the only part"))
				}
			},
		},
		// Base64 that is almost entirely white space: legal (RFC 2045 6.8 folds
		// base64 into lines), decodes to nearly nothing, and must cost nothing.
		"eight megabytes of base64 white space": {
			msg: func() io.Reader {
				return io.MultiReader(
					strings.NewReader("Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n"),
					flood("\r\n", bombSize),
					strings.NewReader("QUJDRA==\r\n"),
				)
			},
			check: func(t *testing.T, m *Message) {
				if m.Root.Size != 4 { // "ABCD"
					t.Errorf("decoded %d octets, want the 4 real ones", m.Root.Size)
				}
				if m.Root.EncodingProblem {
					t.Error("white space between base64 lines is legal, not an encoding problem")
				}
			},
		},
		// Quoted-printable soft line breaks, forever: each decodes to nothing.
		"eight megabytes of soft line breaks": {
			msg: func() io.Reader {
				return io.MultiReader(
					strings.NewReader("Content-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n"),
					flood("=\r\n", bombSize),
					strings.NewReader("end"),
				)
			},
			check: func(t *testing.T, m *Message) {
				if m.Root.Size != 3 {
					t.Errorf("decoded %d octets, want the 3 real ones", m.Root.Size)
				}
			},
		},
		// Breadth: a million parts, far more than the tree is allowed to have. What
		// the parser keeps is the capped tree - maxParts nodes, which is a real cost
		// and a bounded one - and not the surplus, which is read past. The message
		// here is forty megabytes so that the two are told apart: a parser retaining
		// what it read would be an order of magnitude over the limit.
		"a million parts": {
			msg: func() io.Reader {
				const part = "--b\r\nContent-Type: text/plain\r\n\r\npart\r\n"
				return io.MultiReader(
					strings.NewReader("Content-Type: multipart/mixed; boundary=b\r\n\r\n"),
					flood(part, 1000000*len(part)),
					strings.NewReader("--b--\r\n"),
				)
			},
			check: func(t *testing.T, m *Message) {
				if n := len(m.Root.SubParts); n > maxParts {
					t.Errorf("built %d parts, want at most the cap of %d", n, maxParts)
				}
				for _, p := range m.Root.SubParts {
					if p.Size != 4 { // "part"
						t.Fatalf("a part inside the bomb decoded to %d octets, want 4", p.Size)
					}
				}
			},
			limit: 6 << 20, // the capped tree: maxParts nodes with their headers
		},
		// Depth: far deeper than the tree is allowed to nest. Beyond the cap a
		// multipart is not split, and its body is read past.
		"two hundred nested multiparts": {
			msg: func() io.Reader {
				var b strings.Builder
				b.WriteString("Content-Type: multipart/mixed; boundary=b0\r\n\r\n")
				for i := 0; i < 200; i++ {
					b.WriteString("--b" + itoa(i) + "\r\nContent-Type: multipart/mixed; boundary=b" + itoa(i+1) + "\r\n\r\n")
				}
				b.WriteString("--b200\r\nContent-Type: text/plain\r\n\r\ndeep\r\n")
				for i := 200; i >= 0; i-- {
					b.WriteString("--b" + itoa(i) + "--\r\n")
				}
				return strings.NewReader(b.String())
			},
			check: func(t *testing.T, m *Message) {
				depth := 0
				for p := m.Root; len(p.SubParts) > 0; p = p.SubParts[0] {
					depth++
				}
				if depth > maxMultipartDepth {
					t.Errorf("nested %d deep, want at most the cap of %d", depth, maxMultipartDepth)
				}
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pr := newPeakReader(tc.msg())
			m, err := Parse(pr, captureAll)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, m)
			limit := tc.limit
			if limit == 0 {
				limit = bombSize / 8
			}
			if used := pr.held(); used > limit {
				t.Errorf("held %d octets of heap parsing it (limit %d): the message must not be retained", used, limit)
			}
		})
	}
}

// hostileSink is a consumer behaving as badly as the Sink contract allows: it
// reports short writes, it fails, and it fails again when closed. None of that
// is the message's fault, so none of it may cost the message its parse - the
// other sinks, and the structure, must come out whole.
type hostileSink struct {
	writes int
	closed bool
}

func (s *hostileSink) Write(b []byte) (int, error) {
	s.writes++
	return len(b) / 2, errSinkAngry
}

func (s *hostileSink) Close() error {
	s.closed = true
	return errSinkAngry
}

var errSinkAngry = io.ErrClosedPipe

// TestHostileSinkDoesNotBreakTheParse: a sink that refuses its content does not
// stop the parser, does not stop the sinks beside it, and does not stop the
// leaf's identity being computed. A sink is a consumer, not a gate.
func TestHostileSinkDoesNotBreakTheParse(t *testing.T) {
	raw := crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

first part
--b
Content-Type: text/plain

second part
--b--
`)
	var hostile []*hostileSink
	good := map[string]*captureSink{}
	m, err := Parse(strings.NewReader(raw), func(p *Part) LeafSinks {
		h := &hostileSink{}
		g := &captureSink{}
		hostile = append(hostile, h)
		good[p.PartID] = g
		return LeafSinks{Identity: true, Sinks: []Sink{h, g}}
	})
	if err != nil {
		t.Fatalf("a misbehaving sink failed the parse: %v", err)
	}
	if len(m.Root.SubParts) != 2 {
		t.Fatalf("got %d parts, want 2", len(m.Root.SubParts))
	}
	if string(good["1"].buf) != "first part" || string(good["2"].buf) != "second part" {
		t.Errorf("the well-behaved sinks lost content: %q, %q", good["1"].buf, good["2"].buf)
	}
	for i, h := range hostile {
		if h.writes == 0 || !h.closed {
			t.Errorf("hostile sink %d: writes=%d closed=%v, want it fed and closed", i, h.writes, h.closed)
		}
	}
	for _, p := range m.Root.SubParts {
		if p.Size == 0 || p.Digest == [32]byte{} {
			t.Errorf("part %s has no identity, although identity was asked for", p.PartID)
		}
	}
}
