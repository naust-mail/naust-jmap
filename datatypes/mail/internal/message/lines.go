package message

import (
	"bufio"
	"bytes"
	"io"
)

// readChunk is how much of the message the line reader holds at once. A message
// is parsed line by line (RFC 5322 is a line protocol, and so are the MIME
// boundary delimiters of RFC 2046 5.1.1), so this is the whole of the message
// that is ever resident: everything past it has either been consumed or not yet
// been read.
const readChunk = 32 * 1024

// lineReader reads a message as lines without ever holding it whole. It is the
// one place the parser touches the input: the header block reads lines from it,
// the body walker reads lines from it, and what remains unread stays in the
// underlying reader.
//
// A line longer than the caller's limit is not an error and is not buffered
// whole: readLine hands back the first limit octets and says so, and the caller
// either discards the rest (a header field, whose value is capped anyway) or
// pushes its prefix back and lets the body consume the line in full.
type lineReader struct {
	br *bufio.Reader
	// pending is what has been read from br but not yet consumed: the tail of a
	// chunk (a view into br's buffer), or octets a caller pushed back. It is
	// always drained before br is read again, so nothing that points into br's
	// buffer outlives it.
	pending []byte
	// scratch backs a line that straddles fills or exceeds its limit, reused
	// across calls so those lines do not allocate either.
	scratch []byte
}

func newLineReader(r io.Reader) *lineReader {
	return newLineReaderSize(r, readChunk)
}

// newLineReaderSize is newLineReader with the buffer named. A part of a message
// is read through one reader per multipart enclosing it, so those hold less than
// the message's own reader: what the nesting depth multiplies must stay small.
func newLineReaderSize(r io.Reader, size int) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(r, size)}
}

// fill makes pending non-empty, or returns the reader's error (io.EOF at the
// end of the message). What it hands over is a view into br's buffer, not a
// copy: pending is always drained before br is read again, so the view never
// outlives the octets behind it.
func (lr *lineReader) fill() error {
	if len(lr.pending) > 0 {
		return nil
	}
	if lr.br.Buffered() == 0 {
		if _, err := lr.br.Peek(1); err != nil {
			return err
		}
	}
	b, _ := lr.br.Peek(lr.br.Buffered())
	lr.br.Discard(len(b))
	lr.pending = b
	return nil
}

// peek returns the unconsumed front of the stream, filling it if empty, and
// consume takes octets off that front. The slice is valid only until the next
// call on the reader.
func (lr *lineReader) peek() ([]byte, error) {
	if err := lr.fill(); err != nil {
		return nil, err
	}
	return lr.pending, nil
}

func (lr *lineReader) consume(n int) { lr.pending = lr.pending[n:] }

// readLine returns the next line including its line ending, or io.EOF when the
// message is exhausted. A line of more than limit octets is cut: the first limit
// octets come back with trunc set, and the rest of the line is still unread.
// The returned line is valid only until the next call on the reader.
func (lr *lineReader) readLine(limit int) ([]byte, bool, error) {
	if err := lr.fill(); err != nil {
		return nil, false, err
	}
	// The common case: the whole line is already pending, and comes back as a
	// subslice of it - no copy, no allocation.
	scan := lr.pending
	if len(scan) > limit {
		scan = scan[:limit]
	}
	if i := bytes.IndexByte(scan, '\n'); i >= 0 {
		line := lr.pending[:i+1]
		lr.pending = lr.pending[i+1:]
		return line, false, nil
	}
	// The line straddles fills or exceeds the limit: accumulate it in scratch,
	// which is reused across calls under the same validity contract.
	line := lr.scratch[:0]
	for len(line) < limit {
		if err := lr.fill(); err != nil {
			if len(line) > 0 {
				lr.scratch = line
				return line, false, nil // a final line with no line ending
			}
			return nil, false, err
		}
		take := lr.pending
		if room := limit - len(line); len(take) > room {
			take = take[:room]
		}
		if i := bytes.IndexByte(take, '\n'); i >= 0 {
			take = take[:i+1]
		}
		line = append(line, take...)
		lr.pending = lr.pending[len(take):]
		if line[len(line)-1] == '\n' {
			lr.scratch = line
			return line, false, nil
		}
	}
	// At the limit: the line is over-long only if it really does continue. Its
	// line ending may come next (CRLF or a bare LF), or the message may simply
	// end here - in either case the line ended at the limit, not past it.
	lr.scratch = line
	if err := lr.fill(); err != nil {
		return line, false, nil
	}
	if lr.pending[0] == '\r' {
		lr.pending = lr.pending[1:]
		line = append(line, '\r')
		lr.scratch = line
		if err := lr.fill(); err != nil {
			return line, false, nil
		}
	}
	if lr.pending[0] == '\n' {
		lr.pending = lr.pending[1:]
		line = append(line, '\n')
		lr.scratch = line
		return line, false, nil
	}
	return line, true, nil
}

// discardLine consumes whatever remains of the current line, including its line
// ending. It is how a caller drops the tail of an over-long line it has decided
// about from the prefix alone.
func (lr *lineReader) discardLine() error {
	for {
		if err := lr.fill(); err != nil {
			if err == io.EOF {
				return nil // the line simply ended with the message
			}
			return err
		}
		if i := bytes.IndexByte(lr.pending, '\n'); i >= 0 {
			lr.pending = lr.pending[i+1:]
			return nil
		}
		lr.pending = nil
	}
}

// unread puts octets back at the front of the stream, to be read again. The
// header block uses it to hand the body the line that ended it.
func (lr *lineReader) unread(b []byte) {
	if len(b) == 0 {
		return
	}
	back := make([]byte, 0, len(b)+len(lr.pending))
	back = append(back, b...)
	back = append(back, lr.pending...)
	lr.pending = back
}

// Read makes the unconsumed remainder of the message an io.Reader, so a caller
// that wants the rest as a stream (the body, once the headers are read) gets it
// without the reader having buffered any of it.
func (lr *lineReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := lr.fill(); err != nil {
		return 0, err
	}
	n := copy(p, lr.pending)
	lr.pending = lr.pending[n:]
	return n, nil
}
