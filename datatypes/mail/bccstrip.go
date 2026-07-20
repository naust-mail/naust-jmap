package mail

// stripBcc removes Bcc header fields from an RFC 5322 message stream.
// RFC 8621 section 7.5: "The server MUST remove any Bcc header field
// present on the message during delivery" - a server duty performed by
// the sending worker before the message reaches a Submitter. The filter
// is streaming: header lines pass through (or are dropped) as they are
// read, and once the blank line ends the header section the body is
// copied verbatim, so a large message never accumulates in memory.

import (
	"bufio"
	"io"
)

// stripBcc wraps src, dropping every Bcc header field (including folded
// continuation lines, RFC 5322 section 2.2.3) from the header section.
// Both CRLF and bare-LF line endings pass through unchanged.
func stripBcc(src io.Reader) io.Reader {
	return &bccStripReader{src: bufio.NewReader(src)}
}

type bccStripReader struct {
	src      *bufio.Reader
	pending  []byte
	inBody   bool
	dropping bool // inside a Bcc field's folded continuation lines
	err      error
}

func (r *bccStripReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.inBody {
			// Past the headers: hand the buffered reader's stream
			// through without line processing.
			return r.src.Read(p)
		}
		line, err := r.src.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			r.err = err
			continue
		}
		if err != nil {
			// Final unterminated line: emit it and end after.
			r.err = err
		}
		switch {
		case isBlankLine(line):
			r.inBody = true
			r.pending = line
		case r.dropping && isFoldContinuation(line):
			// dropped
		case isBccField(line):
			r.dropping = true
		default:
			r.dropping = false
			r.pending = line
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// isBlankLine reports a header-terminating empty line (CRLF or LF).
func isBlankLine(line []byte) bool {
	return string(line) == "\r\n" || string(line) == "\n"
}

// isFoldContinuation reports a folded header continuation line (starts
// with WSP, RFC 5322 section 2.2.3).
func isFoldContinuation(line []byte) bool {
	return len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
}

// isBccField reports a line opening a Bcc header field: the field name
// "Bcc" (case-insensitive) followed by optional WSP and a colon.
func isBccField(line []byte) bool {
	if len(line) < 4 {
		return false
	}
	if !(line[0] == 'b' || line[0] == 'B') ||
		!(line[1] == 'c' || line[1] == 'C') ||
		!(line[2] == 'c' || line[2] == 'C') {
		return false
	}
	for _, c := range line[3:] {
		switch c {
		case ':':
			return true
		case ' ', '\t':
			// RFC 5322 obsolete syntax allows WSP before the colon.
		default:
			return false
		}
	}
	return false
}
