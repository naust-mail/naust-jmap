package message

// Charset decoding as a stream. DecodeBody turns a text part's octets into its
// EmailBodyValue string (RFC 8621 4.1.4) in one go, which a consumer can afford
// only when it has already bounded what it holds - a preview's leading octets.
// A consumer that wants the whole of a part's text - the body value of an
// Email/get, the text a search term is matched against - must not hold the part
// to get it, so it takes the same decoding as a writer: octets go in as they are
// read, decoded text comes out in pieces, and what is retained is whatever the
// consumer chose to retain.
//
// The two produce the same text. What a whole-buffer decode does by looking at
// all the octets at once - completing a multi-byte character, deciding whether a
// CR begins a CRLF - this does by carrying the few octets it cannot yet decide
// about into the next write.

import (
	"bytes"
	"io"
	"unicode/utf8"

	"golang.org/x/text/transform"
)

// replacement is what a malformed section decodes to (U+FFFD), as in DecodeBody.
var replacement = []byte(string(utf8.RuneError))

// TextWriter decodes a text part's content as it is written: charset decoded
// best effort, malformed input becoming U+FFFD, and CRLF normalized to LF (RFC
// 8621 4.1.4). The decoded text is written on to dst as it is produced; nothing
// is accumulated here. Close must be called: the last characters of the content,
// and a decoding problem in them, are known only at the end.
type TextWriter struct {
	tr   io.WriteCloser // charset decode, writing into norm
	norm *crlfWriter
	// problem is the isEncodingProblem of RFC 8621 4.1.4: the content was not
	// valid in its declared charset, or the charset was not one we know.
	problem bool
}

// NewTextWriter decodes content in the part's charset (nil being the implicit
// us-ascii of RFC 2045 5.2) on to dst.
func NewTextWriter(dst io.Writer, charset *string) *TextWriter {
	cs := "us-ascii"
	if charset != nil {
		cs = *charset
	}
	w := &TextWriter{}
	enc, ascii, known := lookupCharset(cs)
	var t transform.Transformer
	switch {
	case !known:
		// An unknown charset is read as UTF-8 and flagged, as DecodeBody does: the
		// octets are still worth what can be made of them.
		w.problem = true
		t = &sanitizeUTF8{problem: &w.problem}
	case ascii:
		t = &asciiOnly{problem: &w.problem}
	case enc == nil: // UTF-8
		t = &sanitizeUTF8{problem: &w.problem}
	default:
		// The x/text decoders substitute U+FFFD for what they cannot decode rather
		// than reporting it, so a substitution in their output is the problem.
		t = enc.NewDecoder()
		w.norm = &crlfWriter{dst: dst, detect: true, problem: &w.problem}
	}
	if w.norm == nil {
		w.norm = &crlfWriter{dst: dst}
	}
	w.tr = transform.NewWriter(w.norm, t)
	return w
}

func (w *TextWriter) Write(b []byte) (int, error) { return w.tr.Write(b) }

// Close finishes the content: an incomplete character at the end of it, and a CR
// that turned out to end the content rather than begin a CRLF, are decoded now.
func (w *TextWriter) Close() error {
	if err := w.tr.Close(); err != nil {
		return err
	}
	return w.norm.Close()
}

// Problem reports whether the content failed to decode cleanly. It is meaningful
// once Close has returned.
func (w *TextWriter) Problem() bool { return w.problem }

// crlfWriter normalizes CRLF to LF (RFC 8621 4.1.4) as decoded text passes. A CR
// at the end of a write cannot be judged yet - the LF that would pair with it
// may be the first octet of the next - so it is held until the next write, or
// until Close says the content ended on it.
type crlfWriter struct {
	dst io.Writer
	cr  bool // a CR is held back from a previous write
	// detect says a U+FFFD in this text is a decoder's substitution rather than
	// content, which is how a charset decoder that substitutes silently is caught.
	detect  bool
	problem *bool
}

func (w *crlfWriter) Write(b []byte) (int, error) {
	if w.detect && bytes.Contains(b, replacement) {
		*w.problem = true
	}
	n := len(b)
	for len(b) > 0 {
		if w.cr {
			w.cr = false
			if b[0] == '\n' {
				b = b[1:] // the CR began a CRLF: the pair becomes the LF below
				if err := w.emit([]byte{'\n'}); err != nil {
					return 0, err
				}
				continue
			}
			if err := w.emit([]byte{'\r'}); err != nil { // a CR of its own: content
				return 0, err
			}
		}
		i := bytes.IndexByte(b, '\r')
		if i < 0 {
			if err := w.emit(b); err != nil {
				return 0, err
			}
			break
		}
		if err := w.emit(b[:i]); err != nil {
			return 0, err
		}
		w.cr, b = true, b[i+1:]
	}
	return n, nil
}

// Close emits a CR the content ended on.
func (w *crlfWriter) Close() error {
	if !w.cr {
		return nil
	}
	w.cr = false
	return w.emit([]byte{'\r'})
}

func (w *crlfWriter) emit(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, err := w.dst.Write(b)
	return err
}

// asciiOnly is the streaming form of decodeASCII: every octet outside US-ASCII
// (RFC 2045 5.2 makes it the implicit charset of a text part) is a malformed
// one and becomes U+FFFD.
type asciiOnly struct{ problem *bool }

func (t *asciiOnly) Reset() {}

func (t *asciiOnly) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	for nSrc < len(src) {
		c := src[nSrc]
		if c < utf8.RuneSelf {
			if nDst >= len(dst) {
				return nDst, nSrc, transform.ErrShortDst
			}
			dst[nDst] = c
			nDst, nSrc = nDst+1, nSrc+1
			continue
		}
		if nDst+len(replacement) > len(dst) {
			return nDst, nSrc, transform.ErrShortDst
		}
		nDst += copy(dst[nDst:], replacement)
		nSrc++
		*t.problem = true
	}
	return nDst, nSrc, nil
}

// sanitizeUTF8 is the streaming form of strings.ToValidUTF8: each run of octets
// that is not valid UTF-8 becomes one U+FFFD. A character split across two
// writes is not malformed, it is unfinished, and is carried over to the next.
type sanitizeUTF8 struct {
	problem *bool
	inRun   bool // the octets just before were malformed, and are already flagged
}

func (t *sanitizeUTF8) Reset() { t.inRun = false }

func (t *sanitizeUTF8) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	for nSrc < len(src) {
		if c := src[nSrc]; c < utf8.RuneSelf {
			if nDst >= len(dst) {
				return nDst, nSrc, transform.ErrShortDst
			}
			dst[nDst] = c
			nDst, nSrc = nDst+1, nSrc+1
			t.inRun = false
			continue
		}
		r, size := utf8.DecodeRune(src[nSrc:])
		if r == utf8.RuneError && size <= 1 {
			if !atEOF && !utf8.FullRune(src[nSrc:]) {
				return nDst, nSrc, transform.ErrShortSrc // an unfinished character
			}
			if !t.inRun {
				if nDst+len(replacement) > len(dst) {
					return nDst, nSrc, transform.ErrShortDst
				}
				nDst += copy(dst[nDst:], replacement)
				t.inRun = true
				*t.problem = true
			}
			nSrc++
			continue
		}
		if nDst+size > len(dst) {
			return nDst, nSrc, transform.ErrShortDst
		}
		nDst += copy(dst[nDst:], src[nSrc:nSrc+size])
		nSrc += size
		t.inRun = false
	}
	return nDst, nSrc, nil
}
