package message

// The body walker: it reads a message's MIME structure straight off the line
// reader, so no part of the message is ever held whole.
//
// A multipart body is a sequence of segments separated by delimiter lines (RFC
// 2046 5.1.1). Each segment is read as a stream that ends where its multipart's
// next delimiter begins, and the entity inside it - its header block and then
// its body - is read through that stream. So an entity nested three multiparts
// deep reads through three segment readers, one per enclosing multipart.
//
// That nesting is what makes the awkward parts of the format fall out rather
// than be special-cased. A delimiter of an outer multipart ends the segments of
// every part open inside it, because the outer segment is the one reading the
// line. The line ending in front of a delimiter belongs to the delimiter and not
// to the content, and where several multiparts end at the same octet each takes
// one - which is exactly one trim per segment. And a segment is trimmed before
// anything inside it is read, so a part whose content is nothing but that line
// ending has an empty body rather than a stray octet.
//
// The line ending is held back rather than emitted, since whether it is content
// is not known until the next line is read. Nothing else is held: the octets of
// a part flow through the segment to whoever asked for them.

import (
	"bytes"
	"io"
	"strconv"
	"strings"
)

// maxDelimLine bounds the prefix of a line the walker inspects for a boundary
// delimiter. RFC 2046 5.1.1 caps a boundary at 70 characters, and RFC 5322 2.1.1
// caps a whole line at 998 octets, so a line longer than this cannot be a
// delimiter in any message worth calling one: it is content, and the walker
// streams it out without holding it.
const maxDelimLine = 4096

// partChunk is how much of a part's stream its line reader holds. A part reads
// through one reader per enclosing multipart, so this - not the message's own
// readChunk - is what the nesting depth multiplies. A delimiter line fits in it,
// which is all a line reader needs to hold.
const partChunk = 4096

// segment is one entity's octets as a stream: everything from where the entity
// begins to the next delimiter of the multipart that encloses it, exclusive of
// that delimiter's line and of the line ending in front of it (RFC 2046 5.1.1).
// The enclosing multipart's parts are read as consecutive segments of its body;
// the parts of a multipart nested inside one of them are read as segments of
// that segment.
type segment struct {
	lr    *lineReader // the enclosing entity's stream
	delim []byte      // "--" + the enclosing multipart's boundary
	out   []byte      // octets read but not yet handed to the caller
	off   int         // how much of out has been handed out already
	// run is the line ending at the end of the content so far, held back: it
	// belongs to the content if the content goes on, and to the delimiter if a
	// delimiter follows. Anything beyond one line ending is content either way and
	// is emitted at once, so this holds at most two octets.
	run []byte
	// atLineStart says the next unread octet of the enclosing stream begins a
	// line - the only place a delimiter can begin (RFC 2046 5.1.1).
	atLineStart bool
	done        bool
	ended       bool // a delimiter ended it, rather than the enclosing stream running out
	close       bool // and that delimiter was the multipart's close delimiter
	err         error
}

func newSegment(lr *lineReader, delim []byte) *segment {
	return &segment{lr: lr, delim: delim, atLineStart: true}
}

func (s *segment) Read(p []byte) (int, error) {
	for s.off == len(s.out) {
		if s.err != nil {
			return 0, s.err
		}
		if s.done {
			return 0, io.EOF
		}
		// Drained: rewind rather than slide, so out's capacity is reused instead
		// of regrowing behind a dead front.
		s.out, s.off = s.out[:0], 0
		s.fill()
	}
	n := copy(p, s.out[s.off:])
	s.off += n
	return n, nil
}

// nlDash is the only shape a delimiter can announce itself in mid-stream: a
// line ending's LF with the "--" of the delimiter line right behind it.
var nlDash = []byte("\n-")

// fill reads more of the enclosing stream into the segment, or ends it. A
// delimiter can only begin with '-' at a line start (RFC 2046 5.1.1), so
// content is taken in bulk up to the next line start followed by '-', without
// being split into lines; only a line that really might be a delimiter is read
// as one and inspected.
func (s *segment) fill() {
	if s.atLineStart {
		b, err := s.lr.peek()
		if err != nil {
			s.endOn(err)
			return
		}
		if b[0] == '-' {
			s.fillLine()
			return
		}
	}
	s.fillBulk()
}

// fillLine reads and decides the one line that might be a delimiter.
func (s *segment) fillLine() {
	line, trunc, err := s.lr.readLine(maxDelimLine)
	if err != nil {
		s.endOn(err)
		return
	}
	// A delimiter line is bounded (RFC 2046 5.1.1 caps a boundary at 70
	// characters), so a line too long to have been read whole is content; its
	// unread remainder cannot begin a delimiter and flows through fillBulk.
	if !trunc {
		if isDelim, isClose := delimiterLine(line, s.delim); isDelim {
			s.ended, s.close = true, isClose
			s.finish()
			return
		}
	}
	s.consumed(line)
}

// fillBulk passes through everything that cannot be a delimiter in one span:
// up to the next LF-then-'-' if there is one, else all that has arrived.
func (s *segment) fillBulk() {
	buf, err := s.lr.peek()
	if err != nil {
		s.endOn(err)
		return
	}
	span := buf
	if i := bytes.Index(buf, nlDash); i >= 0 {
		// Everything through that LF is content or the line ending a delimiter
		// owns; the '-' line itself is decided by fillLine next round.
		span = buf[:i+1]
	}
	s.lr.consume(len(span))
	s.consumed(span)
}

// consumed feeds octets that turned out to be content into out, holding back
// their trailing line ending (splitLineEnding takes only the last one - any
// interior line ending is already known not to precede a delimiter).
func (s *segment) consumed(span []byte) {
	body, ending := splitLineEnding(span)
	if len(body) > 0 {
		s.emit(s.run)
		s.run = s.run[:0]
		s.emit(body)
	}
	s.run = append(s.run, ending...)
	if excess := len(s.run) - 2; excess > 0 {
		// More than one line ending has piled up, so the front of it is content
		// whatever comes next: only the last can be a delimiter's.
		s.emit(s.run[:excess])
		s.run = append(s.run[:0], s.run[excess:]...)
	}
	s.atLineStart = len(span) > 0 && span[len(span)-1] == '\n'
}

// endOn ends the segment on the enclosing stream's error. Its EOF means the
// entity ended without a delimiter for this one; a missing close delimiter is
// tolerated, so the segment ends here, still trimmed of the line ending its
// delimiter would have owned.
func (s *segment) endOn(err error) {
	if err == io.EOF {
		s.finish()
		return
	}
	s.err = err
}

// finish closes the segment, dropping the line ending that belongs to the
// delimiter (RFC 2046 5.1.1) and emitting whatever of the content is left.
func (s *segment) finish() {
	s.emit(trimPartTail(s.run))
	s.run, s.done = nil, true
}

// emit hands octets to the caller of Read.
func (s *segment) emit(b []byte) { s.out = append(s.out, b...) }

// discard consumes the rest of the segment, so the enclosing stream is
// positioned on whatever follows it. It is how the walker reads past content
// nothing asked for - a preamble, a part beyond the tree's budget - without
// decoding it.
func (s *segment) discard() error {
	if _, err := io.Copy(io.Discard, s); err != nil {
		return err
	}
	return s.err
}

// splitLineEnding divides a line into its content and its line ending: CRLF, a
// bare LF, or - at the very end of a stream, or where a line was cut - a lone
// CR, which may yet turn out to be the front half of a CRLF.
func splitLineEnding(line []byte) (body, ending []byte) {
	n := len(line)
	if n == 0 {
		return line, nil
	}
	if line[n-1] == '\n' {
		if n > 1 && line[n-2] == '\r' {
			return line[:n-2], line[n-2:]
		}
		return line[:n-1], line[n-1:]
	}
	if line[n-1] == '\r' {
		return line[:n-1], line[n-1:]
	}
	return line, nil
}

// walkState is the walk in progress: the counters that bound the tree and number
// its parts, and the factory that says which content is wanted.
type walkState struct {
	// counter numbers leaf parts depth-first for partId assignment; budget is the
	// remaining-parts allowance that bounds the whole tree (maxParts).
	counter int
	budget  int
	factory SinkFactory
}

// walkEntity builds the bodyStructure node for one MIME entity whose header
// block has already been read from lr, and reads its body from the rest of lr.
// defaultType applies when the entity has no usable Content-Type ("text/plain",
// or "message/rfc822" inside multipart/digest, RFC 2046).
func walkEntity(st *walkState, lr *lineReader, headers []HeaderField, defaultType string, depth int) (*Part, error) {
	st.budget--
	p := &Part{Headers: headers}
	typ, params, ctPresent := contentType(headers, defaultType)
	boundary := params["boundary"]
	// A multipart declares a boundary to separate its parts (RFC 2046 5.1.1);
	// with none, its body cannot be split, so it is not a multipart but a single
	// part. It is represented as the RFC 2046 default text/plain leaf rather than
	// a childless multipart, so the body is surfaced to the reader instead of
	// being read past as a multipart's preamble.
	if strings.HasPrefix(typ, "multipart/") && boundary == "" {
		typ, params, ctPresent = "text/plain", nil, true
	}
	p.Type = typ
	p.Charset = charsetOf(typ, params, ctPresent)
	p.Disposition, p.Name = dispositionOf(headers, params)
	p.Cid = angleValue(headers, "Content-Id")
	p.Language = languageOf(headers)
	p.Location = locationOf(headers)

	var err error
	if strings.HasPrefix(typ, "multipart/") {
		p.SubParts = []*Part{} // non-nil: partId/blobId are null iff multipart/*
		err = walkMultipart(st, lr, p, typ, boundary, depth)
	} else {
		// Leaf part (message/rfc822 included: bodyStructure does not recurse into
		// embedded messages, RFC 8621 4.1.4). Content is decoded only for the sinks
		// the factory declares for this leaf; a leaf no consumer asked about is read
		// past, never decoded.
		st.counter++
		p.PartID = strconv.Itoa(st.counter)
		err = feedLeafContent(p, lr, cteOf(headers), st.factory)
	}
	if err != nil {
		return p, err
	}
	// Whatever of this entity was not read - a leaf no sink asked for, a
	// multipart's epilogue, a body too deeply nested to split - is read past, so
	// the entity around it goes on from the right octet and a message that stops
	// arriving is an error rather than a short parse.
	if _, err := io.Copy(io.Discard, lr); err != nil {
		return p, err
	}
	return p, nil
}

// walkMultipart reads a multipart entity's body: its preamble, its parts, and -
// left to the caller's read-past - its epilogue (RFC 2046 5.1.1). A multipart
// nested deeper than maxMultipartDepth is left unsplit: it keeps its declared
// type, has no children, and its body is read past. (A multipart with no
// boundary is degraded to a leaf before it reaches here, in walkEntity.)
func walkMultipart(st *walkState, lr *lineReader, p *Part, typ, boundary string, depth int) error {
	if depth >= maxMultipartDepth {
		return nil
	}
	delim := []byte("--" + boundary)

	childDefault := "text/plain"
	if typ == "multipart/digest" {
		childDefault = "message/rfc822"
	}

	// Everything before the first delimiter is the preamble: read past, never a
	// part. If no delimiter follows it, the multipart has no parts at all.
	seg := newSegment(lr, delim)
	if err := seg.discard(); err != nil {
		return err
	}

	for seg.ended && !seg.close {
		seg = newSegment(lr, delim)
		if st.budget <= 0 {
			// The tree's part budget (maxParts) is spent: no further part is built.
			// The octets are still read, so the multiparts around this one go on
			// parsing from the right place.
			if err := seg.discard(); err != nil {
				return err
			}
			continue
		}
		partLR := newLineReaderSize(seg, partChunk)
		headers, err := readHeaderBlock(partLR)
		if err != nil {
			return err
		}
		child, err := walkEntity(st, partLR, headers, childDefault, depth+1)
		if err != nil {
			return err
		}
		p.SubParts = append(p.SubParts, child)
	}
	// The loop ended on this multipart's close delimiter, on a delimiter of a
	// multipart further out (which ends this one too: a missing close delimiter is
	// tolerated), or with the enclosing entity exhausted. Anything left in lr is
	// the epilogue, which the caller reads past.
	return nil
}
