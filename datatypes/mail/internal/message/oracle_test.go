package message

// The differential harness: for any input, this parser must produce what the
// frozen buffered parser (internal/oracle) produced - the same header list, the
// same body-part tree, the same content identity per leaf (the digest behind
// RFC 8620 6.1 blobIds and the RFC 8621 4.1.4 size), the same decoded octets in
// the sinks, the same three body views, hasAttachment, and preview.
//
// It is the gate on rewriting the parser's internals to stream: the walker, the
// boundary handling, the transfer-encoding and charset decoding all change, and
// none of it may change a single observable output. A deliberate divergence, if
// one is ever accepted, belongs here as an explicit exception with the spec
// clause that permits it - not as a quietly edited expectation.

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/oracle"
)

// snapshot is everything a consumer can observe about a parsed message, in a
// form the two parsers can be compared in.
type snapshot struct {
	Headers     []string
	Parts       []partSnapshot
	TextBody    []string // partIds, in view order
	HTMLBody    []string
	Attachments []string
	HasAttach   bool
	Preview     string
}

// partSnapshot is one body part: its metadata, its content identity, the octets
// its sinks were fed, and the text a body value would be built from.
type partSnapshot struct {
	Path      string // position in the tree, e.g. "0.2.1"
	PartID    string
	Type      string
	Charset   string
	Disp      string
	Cid       string
	Location  string
	Name      string
	Language  []string
	Headers   []string
	Multipart bool
	SubParts  int
	Size      uint64
	Digest    [32]byte
	Problem   bool
	Content   string
	BodyValue string
}

func snapshotNew(raw []byte) (snapshot, error) {
	sinks := map[*Part]*captureSink{}
	m, err := Parse(bytes.NewReader(raw), func(p *Part) LeafSinks {
		s := &captureSink{}
		sinks[p] = s
		return LeafSinks{Identity: true, Sinks: []Sink{s}}
	})
	if err != nil {
		return snapshot{}, err
	}
	return snapshotOf(m, sinks), nil
}

// snapshotOf builds the snapshot of a parse whose every leaf was captured.
func snapshotOf(m *Message, sinks map[*Part]*captureSink) snapshot {
	snap := snapshot{Headers: headerStrings(m.Headers)}
	var walk func(p *Part, path string)
	walk = func(p *Part, path string) {
		var content []byte
		if s, ok := sinks[p]; ok { // multipart nodes have no content and no sink
			content = s.buf
		}
		value, _ := DecodeBody(content, p.Charset)
		snap.Parts = append(snap.Parts, partSnapshot{
			Path: path, PartID: p.PartID, Type: p.Type,
			Charset: deref(p.Charset), Disp: deref(p.Disposition), Cid: deref(p.Cid),
			Location: deref(p.Location), Name: deref(p.Name), Language: p.Language,
			Headers: headerStrings(p.Headers), Multipart: p.SubParts != nil,
			SubParts: len(p.SubParts), Size: p.Size, Digest: p.Digest,
			Problem: p.EncodingProblem, Content: string(content), BodyValue: value,
		})
		for i, sub := range p.SubParts {
			walk(sub, fmt.Sprintf("%s.%d", path, i))
		}
	}
	walk(m.Root, "0")
	tb, hb, at := Flatten(m.Root)
	snap.TextBody, snap.HTMLBody, snap.Attachments = partIDs(tb), partIDs(hb), partIDs(at)
	snap.HasAttach = HasAttachment(at)
	snap.Preview = BuildPreview(previewSourcesOf(tb, sinks), previewSourcesOf(hb, sinks))
	return snap
}

// snapshotOracle is snapshotNew against the frozen parser. The two are kept
// deliberately separate rather than made generic: the oracle's types must never
// be reachable from the parser under test.
func snapshotOracle(raw []byte) (snapshot, error) {
	sinks := map[*oracle.Part]*oracleSink{}
	m, err := oracle.Parse(bytes.NewReader(raw), func(p *oracle.Part) oracle.LeafSinks {
		s := &oracleSink{}
		sinks[p] = s
		return oracle.LeafSinks{Identity: true, Sinks: []oracle.Sink{s}}
	})
	if err != nil {
		return snapshot{}, err
	}
	snap := snapshot{Headers: oracleHeaderStrings(m.Headers)}
	var walk func(p *oracle.Part, path string)
	walk = func(p *oracle.Part, path string) {
		var content []byte
		if s, ok := sinks[p]; ok {
			content = s.buf
		}
		value, _ := oracle.DecodeBody(content, p.Charset)
		snap.Parts = append(snap.Parts, partSnapshot{
			Path: path, PartID: p.PartID, Type: p.Type,
			Charset: deref(p.Charset), Disp: deref(p.Disposition), Cid: deref(p.Cid),
			Location: deref(p.Location), Name: deref(p.Name), Language: p.Language,
			Headers: oracleHeaderStrings(p.Headers), Multipart: p.SubParts != nil,
			SubParts: len(p.SubParts), Size: p.Size, Digest: p.Digest,
			Problem: p.EncodingProblem, Content: string(content), BodyValue: value,
		})
		for i, sub := range p.SubParts {
			walk(sub, fmt.Sprintf("%s.%d", path, i))
		}
	}
	walk(m.Root, "0")
	tb, hb, at := oracle.Flatten(m.Root)
	snap.TextBody, snap.HTMLBody, snap.Attachments = oraclePartIDs(tb), oraclePartIDs(hb), oraclePartIDs(at)
	snap.HasAttach = oracle.HasAttachment(at)
	snap.Preview = oracle.BuildPreview(oraclePreviewSources(tb, sinks), oraclePreviewSources(hb, sinks))
	return snap, nil
}

type oracleSink struct{ buf []byte }

func (s *oracleSink) Write(b []byte) (int, error) { s.buf = append(s.buf, b...); return len(b), nil }
func (s *oracleSink) Close() error                { return nil }

func previewSourcesOf(parts []*Part, sinks map[*Part]*captureSink) []PreviewSource {
	out := make([]PreviewSource, 0, len(parts))
	for _, p := range parts {
		text, _ := DecodeBody(capTo(sinks[p].buf, previewBudget(p.Type)), p.Charset)
		out = append(out, PreviewSource{Type: p.Type, Text: text})
	}
	return out
}

func oraclePreviewSources(parts []*oracle.Part, sinks map[*oracle.Part]*oracleSink) []oracle.PreviewSource {
	out := make([]oracle.PreviewSource, 0, len(parts))
	for _, p := range parts {
		text, _ := oracle.DecodeBody(capTo(sinks[p].buf, previewBudget(p.Type)), p.Charset)
		out = append(out, oracle.PreviewSource{Type: p.Type, Text: text})
	}
	return out
}

// previewBudget is the byte budget a preview-capture sink retains for a part of
// this media type (the same rule the mail package's preview sink applies).
func previewBudget(typ string) int {
	if typ == "text/html" {
		return PreviewHTMLBytes
	}
	return PreviewTextBytes
}

func capTo(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func deref(s *string) string {
	if s == nil {
		return "\x00nil"
	}
	return *s
}

func headerStrings(hs []HeaderField) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.Name+":"+h.Value)
	}
	return out
}

func oracleHeaderStrings(hs []oracle.HeaderField) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.Name+":"+h.Value)
	}
	return out
}

func partIDs(parts []*Part) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.PartID)
	}
	return out
}

func oraclePartIDs(parts []*oracle.Part) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.PartID)
	}
	return out
}

// acceptedDivergence reports the one input class where this parser is knowingly
// not the oracle: a header field name longer than maxHeaderName.
//
// The buffered parser accepted such a line as a field and retained a name of
// unbounded length - exactly the unbounded retention the streaming parser exists
// to remove - so the streaming reader decides a field from a bounded prefix of
// its line and treats a longer "name" as the first line of the body instead. RFC
// 5322 2.1.1 limits a whole header line to 998 octets, so no legitimate message
// can reach this; TestHeaderNameCap pins the behavior directly.
//
// A header block starts the message and starts every multipart body part, so any
// line of the input may be read as a header field. The test is therefore made
// over every line, which declines to compare a few inputs that would not in fact
// have diverged (a body line carrying a colon more than maxHeaderName octets in).
// That errs the safe way: it costs a little coverage, and hides nothing that a
// smaller input would not also show.
// The second accepted class is the mirror of the first, in the body: RFC 2046
// 5.1.1 allows white space after a boundary delimiter, and the buffered parser
// allowed it without limit, so it would recognize a delimiter padded to any
// length. The walker decides a delimiter from a bounded prefix of the line
// (maxDelimLine) and treats a line too long to have been read whole as content -
// a boundary is at most 70 characters, so again no real message is affected.
// TestWalkOverLongDelimiterLine pins that behavior directly.
func acceptedDivergence(raw []byte) bool {
	for pos := 0; pos < len(raw); {
		line, next := nextLine(raw, pos)
		trimmed := trimLineEnding(line)
		if colon := bytes.IndexByte(trimmed, ':'); colon > maxHeaderName {
			return true
		}
		if len(trimmed) > maxDelimLine && bytes.HasPrefix(trimmed, []byte("--")) {
			return true
		}
		pos = next
	}
	return false
}

// assertSameAsOracle fails with the first field that differs.
func assertSameAsOracle(t *testing.T, raw []byte) {
	t.Helper()
	if acceptedDivergence(raw) {
		return
	}
	got, err := snapshotNew(raw)
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	want, err := snapshotOracle(raw)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if reflect.DeepEqual(got, want) {
		return
	}
	if !reflect.DeepEqual(got.Headers, want.Headers) {
		t.Errorf("headers differ:\n got %q\nwant %q", got.Headers, want.Headers)
	}
	if len(got.Parts) != len(want.Parts) {
		t.Fatalf("part count = %d, oracle has %d", len(got.Parts), len(want.Parts))
	}
	for i := range got.Parts {
		if !reflect.DeepEqual(got.Parts[i], want.Parts[i]) {
			t.Errorf("part %s differs:\n got %+v\nwant %+v", want.Parts[i].Path, got.Parts[i], want.Parts[i])
		}
	}
	for _, v := range []struct {
		name      string
		got, want any
	}{
		{"textBody", got.TextBody, want.TextBody},
		{"htmlBody", got.HTMLBody, want.HTMLBody},
		{"attachments", got.Attachments, want.Attachments},
		{"hasAttachment", got.HasAttach, want.HasAttach},
		{"preview", got.Preview, want.Preview},
	} {
		if !reflect.DeepEqual(v.got, v.want) {
			t.Errorf("%s differs:\n got %v\nwant %v", v.name, v.got, v.want)
		}
	}
}

// TestDifferentialCorpus runs every fixture and fuzz seed through both parsers.
func TestDifferentialCorpus(t *testing.T) {
	cases := append([]string{crlf(akMessage)}, fuzzSeeds()...)
	for i, raw := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			assertSameAsOracle(t, []byte(raw))
		})
	}
}

// FuzzDifferential is the real gate: any input at all, and the streaming parser
// must agree with the frozen one.
func FuzzDifferential(f *testing.F) {
	for _, seed := range fuzzSeeds() {
		f.Add([]byte(seed))
	}
	f.Add([]byte(crlf(akMessage)))
	f.Fuzz(func(t *testing.T, data []byte) {
		assertSameAsOracle(t, data)
	})
}
