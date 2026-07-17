package message

// Content-Transfer-Encoding as a stream (RFC 2045 section 6). A part's content
// is decoded on its way from the reader to the sinks, so neither the encoded nor
// the decoded form of it is ever held: an attachment of any size costs the
// parser a buffer, not its length.
//
// Decoding is best effort, as the rest of the parser is. An encoding that is not
// understood, or content that does not decode cleanly, does not fail the parse:
// it is flagged, and that flag becomes EmailBodyValue.isEncodingProblem (RFC
// 8621 section 4.1.4). A failure to READ the message, on the other hand, is a
// real error and is reported as one - a truncated delivery must not be mistaken
// for a message with a broken attachment.

import (
	"encoding/base64"
	"io"
	"mime/quotedprintable"
)

// cteDecoder is a leaf's content, decoded. problem is meaningful once the stream
// has been read to the end.
type cteDecoder struct {
	dec     io.Reader
	src     *readErrors
	problem bool
	done    bool
}

// newCTEDecoder wraps a part's raw content in the decoder its encoding calls
// for. An unknown encoding is passed through as-is and flagged (RFC 8621 4.1.4
// treats it as identity).
func newCTEDecoder(raw io.Reader, cte string) *cteDecoder {
	src := &readErrors{r: raw}
	d := &cteDecoder{src: src}
	switch cte {
	case "", "7bit", "8bit", "binary":
		d.dec = src
	case "base64":
		d.dec = base64.NewDecoder(base64.RawStdEncoding, &base64Filter{src: src, problem: &d.problem})
	case "quoted-printable":
		d.dec = quotedprintable.NewReader(src)
	default:
		d.dec, d.problem = src, true
	}
	return d
}

func (d *cteDecoder) Read(p []byte) (int, error) {
	if d.done {
		return 0, io.EOF
	}
	n, err := d.dec.Read(p)
	switch {
	case err == nil, err == io.EOF:
		if err == io.EOF {
			d.done = true
		}
		return n, err
	case d.src.err != nil:
		return n, d.src.err // the message could not be read: a real failure
	default:
		// The content did not decode. Keep what did, flag the part, and end the
		// stream: a malformed body part is not a broken message.
		d.problem, d.done = true, true
		return n, io.EOF
	}
}

// readErrors passes a reader through, remembering any failure to read it, so a
// decoder's error can be told apart from the underlying stream's.
type readErrors struct {
	r   io.Reader
	err error
}

func (r *readErrors) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

// base64Filter feeds a base64 decoder only what it can accept. A body carries
// its base64 in lines (RFC 2045 6.8), and may carry padding; both are dropped
// here rather than confusing the decoder. Any other octet is not base64 at all:
// it is dropped too, and the part is flagged.
//
// Only whole four-character groups are passed on, with the last partial group
// held back until the end of the content is known - a final group of one
// character is an impossible base64 length, and is dropped with the part flagged
// rather than failing the decode.
type base64Filter struct {
	src     io.Reader
	problem *bool
	group   []byte // filtered characters not yet a complete group (at most 3)
	out     []byte // complete groups waiting to be read
	off     int
	chunk   [readChunk]byte
	done    bool
	padded  bool // a padding character has been seen: the data has ended
}

// flushFinal emits the last, possibly partial, group. A group of one character
// is not a valid base64 length (RFC 2045 6.8 encodes in 24-bit units, so the
// shortest tail is two characters), so it is dropped and the part flagged rather
// than handed to the decoder as an error.
func (f *base64Filter) flushFinal() {
	if len(f.group) == 1 {
		*f.problem = true
	} else {
		f.out = append(f.out, f.group...)
	}
	f.group = f.group[:0]
}

func (f *base64Filter) Read(p []byte) (int, error) {
	for f.off == len(f.out) {
		if f.done {
			return 0, io.EOF
		}
		f.out, f.off = f.out[:0], 0
		n, err := f.src.Read(f.chunk[:])
		for _, c := range f.chunk[:n] {
			switch {
			case c == '=':
				// Padding marks the end of the encoded data (RFC 2045 6.8). The
				// group in hand is the last one; anything after it is trailing
				// content, handled below.
				if !f.padded {
					f.padded = true
					f.flushFinal()
				}
			case c == ' ', c == '\t', c == '\r', c == '\n':
				// The white space that folds the lines: not content.
			case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
				if f.padded {
					*f.problem = true // base64 data after the padding: malformed
					continue
				}
				f.group = append(f.group, c)
				if len(f.group) == 4 {
					f.out = append(f.out, f.group...)
					f.group = f.group[:0]
				}
			default:
				*f.problem = true // a foreign octet in a base64 body
			}
		}
		if err == io.EOF {
			f.done = true
			if !f.padded {
				f.flushFinal()
			}
		} else if err != nil {
			return 0, err
		}
	}
	n := copy(p, f.out[f.off:])
	f.off += n
	return n, nil
}
