package report

// Truncating a returned original message to its header block, which is
// what makes the text/rfc822-headers form of the third report component
// (RFC 6522 section 4) a truncation of the original rather than a copy of
// it. The block is everything up to and including the blank line that ends
// the header section (RFC 5322 section 2.1); a message with no blank line
// is all header block.

import "io"

// headerBlockReader streams the leading header block of src and then
// reports EOF, discarding the body without reading it. Bare LF line
// endings are honored as well as CRLF: a stored message is not guaranteed
// to be CRLF-clean, and stopping at the wrong place would leak body
// content into a headers-only report.
type headerBlockReader struct {
	src   io.Reader
	chunk [4096]byte
	buf   []byte // header-block bytes read but not yet returned
	bol   bool   // the next byte begins a line
	cr    bool   // a CR at the start of a line is pending its LF
	found bool   // the blank line has been seen; buf is the last of the block
	eof   bool   // src is exhausted
}

func (h *headerBlockReader) Read(p []byte) (int, error) {
	for len(h.buf) == 0 {
		if h.found || h.eof {
			return 0, io.EOF
		}
		n, err := h.src.Read(h.chunk[:])
		if n > 0 {
			cut, found := h.scan(h.chunk[:n])
			h.buf, h.found = h.chunk[:cut], found
		}
		if err == io.EOF {
			h.eof = true
		} else if err != nil {
			return 0, err
		}
	}
	n := copy(p, h.buf)
	h.buf = h.buf[n:]
	return n, nil
}

// scan advances the line state over b and returns how many of its bytes
// belong to the header block, plus whether the block ended inside b. The
// state fields carry across chunk boundaries, so a CRLF split between two
// reads is still recognized.
func (h *headerBlockReader) scan(b []byte) (int, bool) {
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case h.cr:
			h.cr = false
			if c == '\n' { // CRLF on an empty line: the block ends here
				return i + 1, true
			}
			h.bol = c == '\n'
		case h.bol:
			h.bol = false
			if c == '\n' { // bare LF on an empty line
				return i + 1, true
			}
			if c == '\r' {
				h.cr = true
			}
		default:
			h.bol = c == '\n'
		}
	}
	return len(b), false
}
