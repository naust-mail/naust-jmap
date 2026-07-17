package message

import (
	"bytes"
	"testing"
)

// captureSink keeps a leaf's whole decoded content. No production consumer does
// this - that is the point of the sink boundary - but a test wants to assert on
// what the pipeline produced, so it declares the one sink that keeps it all.
type captureSink struct{ buf []byte }

func (s *captureSink) Write(b []byte) (int, error) {
	s.buf = append(s.buf, b...)
	return len(b), nil
}

func (s *captureSink) Close() error { return nil }

// doc is the test view of a parsed message: the parse result, the decoded
// content of every leaf, and the values a consumer derives from the tree - the
// three body views of RFC 8621 4.1.4, hasAttachment, and the preview. A real
// consumer asks for only the slice of this it needs; the tests want all of it.
type doc struct {
	*Message
	Content       map[*Part][]byte
	TextBody      []*Part
	HTMLBody      []*Part
	Attachments   []*Part
	HasAttachment bool
	Preview       string
}

// parseDoc parses raw with every leaf captured, and derives the views.
func parseDoc(tb testing.TB, raw []byte) *doc {
	tb.Helper()
	sinks := map[*Part]*captureSink{}
	msg, err := Parse(bytes.NewReader(raw), func(p *Part) LeafSinks {
		s := &captureSink{}
		sinks[p] = s
		return LeafSinks{Identity: true, Sinks: []Sink{s}}
	})
	if err != nil {
		tb.Fatalf("parse: %v", err)
	}
	d := &doc{Message: msg, Content: make(map[*Part][]byte, len(sinks))}
	for p, s := range sinks {
		d.Content[p] = s.buf
	}
	d.TextBody, d.HTMLBody, d.Attachments = Flatten(msg.Root)
	d.HasAttachment = HasAttachment(d.Attachments)
	d.Preview = BuildPreview(d.previewSources(d.TextBody), d.previewSources(d.HTMLBody))
	return d
}

// previewSources mirrors what a preview-capture sink collects: each part's
// decoded content, cut to the preview budget and charset decoded.
func (d *doc) previewSources(parts []*Part) []PreviewSource {
	out := make([]PreviewSource, 0, len(parts))
	for _, p := range parts {
		limit := PreviewTextBytes
		if p.Type == "text/html" {
			limit = PreviewHTMLBytes
		}
		raw := d.Content[p]
		if len(raw) > limit {
			raw = raw[:limit]
		}
		text, _ := DecodeBody(raw, p.Charset)
		out = append(out, PreviewSource{Type: p.Type, Text: text})
	}
	return out
}

// Parts is every part of the message, depth first.
func (d *doc) Parts() []*Part {
	var out []*Part
	var walk func(p *Part)
	walk = func(p *Part) {
		out = append(out, p)
		for _, sub := range p.SubParts {
			walk(sub)
		}
	}
	walk(d.Root)
	return out
}
