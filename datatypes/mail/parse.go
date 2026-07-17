package mail

// Parsing a message for the mail package: what each consumer asks the parser to
// capture from body content, and the parsed view it reads back.
//
// The parser (internal/message) yields structure and metadata only; a decoded
// body part is never handed out whole. Every value derived from content - the
// preview and hasAttachment fast fields (RFC 8621 section 4.1.4), a per-part
// blobId and size (4.1.4), the bodyValues of an Email/get (4.2), the naive
// searcher's body match (4.4.1) - is collected by a sink as the message is
// walked. A consumer declares the sinks it wants through a capture, runs the
// parse, and reads its results back; content nothing asked for is never decoded.

import (
	"io"
	"strings"
	"unicode/utf8"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// capture declares what one parse should collect from body content, and holds
// the sinks that collect it. Delivery and Email/import want only the preview;
// Email/get wants per-part identity and, when the fetch arguments ask for them,
// bodyValues; Email/parse wants both. The flags are fixed when the consumer
// builds the capture, so factory decides per part from the part's metadata
// alone (RFC 8621 request semantics never reach into the parser).
type capture struct {
	// identity requests each leaf's content identity: the digest behind its
	// blobId and its decoded size (section 4.1.4). Set only when the request
	// actually renders those properties, so a caller reading headers alone never
	// decodes a hostile attachment.
	identity bool
	// preview requests the bounded text the preview fast field is built from.
	preview bool
	// values requests the text of every text/* leaf for bodyValues (section
	// 4.2). The parse cannot yet know which leaves the fetch flags will select -
	// that needs the flattened views, which exist only after the walk - so every
	// text/* leaf is captured and the selection is made afterwards.
	values bool
	// maxValueBytes is the request's maxBodyValueBytes: the cap on a captured
	// value, 0 meaning unlimited (section 4.2).
	maxValueBytes int64
	// previewBudget is what this message has left to spend on preview text,
	// shared by every preview sink it installs (maxPreviewCapture).
	previewBudget int

	previews   map[*message.Part]*previewSink
	valueSinks map[*message.Part]*valueSink
}

// factory is the SinkFactory this capture installs. It is called once per leaf,
// before that leaf's content is decoded, and decides purely from the leaf's
// media type: a leaf no sink asks about is never read.
func (c *capture) factory() message.SinkFactory {
	return func(p *message.Part) message.LeafSinks {
		ls := message.LeafSinks{Identity: c.identity}
		if !strings.HasPrefix(p.Type, "text/") {
			return ls
		}
		// The preview is built from plain text and tag-stripped HTML only
		// (section 4.1.4), so no other text/* subtype needs capturing for it.
		if c.preview && (p.Type == "text/plain" || p.Type == "text/html") {
			s := newPreviewSink(p, &c.previewBudget)
			c.previews[p] = s
			ls.Sinks = append(ls.Sinks, s)
		}
		if c.values {
			s := newValueSink(p, c.maxValueBytes)
			c.valueSinks[p] = s
			ls.Sinks = append(ls.Sinks, s)
		}
		return ls
	}
}

// newCapture builds a capture with its sink maps ready.
func newCapture() *capture {
	return &capture{
		previewBudget: maxPreviewCapture,
		previews:      map[*message.Part]*previewSink{},
		valueSinks:    map[*message.Part]*valueSink{},
	}
}

// parsed is one parsed message as the mail package uses it: the parser's
// structure, the section 4.1.4 body-part views derived from it, and the capture
// holding whatever content was collected.
type parsed struct {
	msg         *message.Message
	textBody    []*message.Part
	htmlBody    []*message.Part
	attachments []*message.Part
	cap         *capture
}

// parseMessage parses r with the given capture and flattens the resulting tree
// into the three body-part views (RFC 8621 section 4.1.4). The parser fails only
// on a read error.
func parseMessage(r io.Reader, c *capture) (*parsed, error) {
	msg, err := message.Parse(r, c.factory())
	if err != nil {
		return nil, err
	}
	tb, hb, at := message.Flatten(msg.Root)
	return &parsed{msg: msg, textBody: tb, htmlBody: hb, attachments: at, cap: c}, nil
}

// hasAttachment is the section 4.1.4 fast field: the attachments view holds a
// part that is not inline.
func (p *parsed) hasAttachment() bool { return message.HasAttachment(p.attachments) }

// preview is the section 4.1.4 preview fast field, built from the text the
// preview sinks captured while walking, in body-view order (plain text first,
// tag-stripped HTML as the fallback). It is empty unless the capture asked for
// the preview.
func (p *parsed) preview() string {
	return message.BuildPreview(p.previewSources(p.textBody), p.previewSources(p.htmlBody))
}

func (p *parsed) previewSources(parts []*message.Part) []message.PreviewSource {
	out := make([]message.PreviewSource, 0, len(parts))
	for _, part := range parts {
		if s, ok := p.cap.previews[part]; ok {
			out = append(out, message.PreviewSource{Type: part.Type, Text: s.text})
		}
	}
	return out
}

// maxPreviewCapture bounds the text captured for the preview across the whole
// message. A part's own budget (message.PreviewTextBytes, PreviewHTMLBytes)
// bounds what one part contributes, but a message can hold as many text parts as
// the tree allows, and the parse cannot know which one the preview will come
// from until the tree is walked - so without a budget for the message, a sender
// could multiply the per-part budget by the part count and make a delivery hold
// what the message itself no longer does.
//
// It is far more than the preview can use (the joined text is cut at
// PreviewTextBytes and the preview itself at 256 characters, section 4.1.4), so
// no message anyone would send is affected by it. A preview is best effort by
// specification: for one made of thousands of text parts, it is built from the
// text at the front of it.
const maxPreviewCapture = 256 << 10

// previewSink captures the leading octets of one text part's decoded content -
// as many as the preview algorithm can use (section 4.1.4 caps the preview at
// 256 characters) - and charset decodes them when the part ends. It retains no
// more than its own budget and no more than what is left of the message's, so
// building a preview holds neither a whole body part nor a whole message of them.
type previewSink struct {
	charset *string
	limit   int
	budget  *int // what the message has left to spend on previews, shared
	raw     []byte
	text    string
}

func newPreviewSink(p *message.Part, budget *int) *previewSink {
	limit := message.PreviewTextBytes
	if p.Type == "text/html" {
		limit = message.PreviewHTMLBytes
	}
	return &previewSink{charset: p.Charset, limit: limit, budget: budget}
}

func (s *previewSink) Write(b []byte) (int, error) {
	room := s.limit - len(s.raw)
	if *s.budget < room {
		room = *s.budget
	}
	if room > 0 {
		if len(b) > room {
			b = b[:room]
		}
		s.raw = append(s.raw, b...)
		*s.budget -= len(b)
	}
	return len(b), nil // octets past the budget are discarded, not an error
}

func (s *previewSink) Close() error {
	s.text, _ = message.DecodeBody(s.raw, s.charset)
	s.raw = nil
	return nil
}

// valueSink captures one text part's decoded content as an EmailBodyValue
// (RFC 8621 section 4.1.4): charset decoded and CRLF normalized as the content
// streams past, and truncated to the request's maxBodyValueBytes on a rune
// boundary (section 4.2 forbids splitting a codepoint). max == 0 is the section
// 4.2 default: no limit, the client asked for the whole value and the response
// carries it.
//
// The content is decoded to its end whether or not the cap is reached, so the
// isEncodingProblem the client is shown describes the whole part rather than
// only the piece of it that fits; what the cap bounds is what is retained.
type valueSink struct {
	w    *message.TextWriter
	keep *cappedText

	value     string
	problem   bool
	truncated bool
}

func newValueSink(p *message.Part, max int64) *valueSink {
	keep := &cappedText{max: max}
	return &valueSink{w: message.NewTextWriter(keep, p.Charset), keep: keep}
}

func (s *valueSink) Write(b []byte) (int, error) { return s.w.Write(b) }

func (s *valueSink) Close() error {
	if err := s.w.Close(); err != nil {
		return err
	}
	s.value, s.problem, s.truncated = string(s.keep.text), s.w.Problem(), s.keep.over
	s.keep = nil
	return nil
}

// cappedText retains decoded text up to a cap in octets, dropping the rest and
// remembering that there was a rest. A cap of 0 is no cap.
//
// The text arrives as valid UTF-8 in whole characters, and it is the cap that
// can fall inside one, so the cut is made at the character boundary before it:
// section 4.2 forbids a value that splits a codepoint, and the value is the
// longest one within the cap that does not.
type cappedText struct {
	max  int64
	text []byte
	over bool // content arrived past the cap
}

func (c *cappedText) Write(b []byte) (int, error) {
	if c.max <= 0 {
		c.text = append(c.text, b...)
		return len(b), nil
	}
	room := int(c.max) - len(c.text)
	switch {
	case room <= 0:
		c.over = true
	case len(b) > room:
		cut := room
		for cut > 0 && !utf8.RuneStart(b[cut]) {
			cut--
		}
		c.text = append(c.text, b[:cut]...)
		c.over = true
	default:
		c.text = append(c.text, b...)
	}
	return len(b), nil // content past the cap is discarded, not an error
}
