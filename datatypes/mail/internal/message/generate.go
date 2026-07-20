package message

// The streaming message writer, the write-twin of Parse: it composes an
// RFC 5322 message from a header list and a tree of body parts into an
// io.Writer one part at a time, so a message is never held whole in
// memory (RFC 8621 section 4.6 creation; RFC 2045/2046 for the MIME
// framing). Leaf content is pulled from a caller-supplied source and
// pushed through the declared Content-Transfer-Encoding as it streams.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime/quotedprintable"
)

// Content-Transfer-Encoding names an OutPart may declare (RFC 2045
// section 6). Identity encodings ("7bit", "8bit") stream the source
// unchanged; the caller is responsible for its lines being CRLF-delimited.
const (
	Enc7Bit   = "7bit"
	Enc8Bit   = "8bit"
	EncQP     = "quoted-printable"
	EncBase64 = "base64"
)

// OutPart is one node of an outbound body-part tree. A multipart node has
// SubParts and a Boundary and no content; a leaf has a Content source and
// an Encoding. Headers is the node's full Content-* header block (a
// multipart node's Content-Type must carry its Boundary as the boundary
// parameter; the writer frames parts, it does not invent headers).
type OutPart struct {
	Headers  []HeaderField
	Boundary string
	SubParts []*OutPart
	// Content opens the leaf's decoded content. It is called exactly once,
	// when the writer reaches the leaf, so a source backed by a blob is
	// opened only for as long as it streams.
	Content  func(ctx context.Context) (io.ReadCloser, error)
	Encoding string
}

// NewBoundary returns a random boundary string (RFC 2046 section 5.1.1).
// 144 bits of randomness make a collision with part content vanishingly
// unlikely, which is what permits streaming content that was never scanned.
func NewBoundary() string {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("message: reading random boundary: " + err.Error())
	}
	return "b" + hex.EncodeToString(b[:])
}

// WriteMessage streams the message to w: the top-level header list, then
// the root part's own headers, then its body (RFC 5322 section 3.5). The
// root's Content-* headers share the message's single header block, which
// is what makes a single-part message come out flat.
func WriteMessage(ctx context.Context, w io.Writer, headers []HeaderField, root *OutPart) error {
	bw := bufio.NewWriter(w)
	for _, h := range headers {
		writeHeader(bw, h)
	}
	if err := writeEntity(ctx, bw, root); err != nil {
		return err
	}
	return bw.Flush()
}

// writeEntity writes one part: its headers, a blank line, then its body -
// either the encoded leaf content or the delimited sub-parts (RFC 2046
// section 5.1.1: each delimiter is CRLF "--" boundary, so a part's
// trailing CRLF belongs to the delimiter that follows it).
func writeEntity(ctx context.Context, bw *bufio.Writer, p *OutPart) error {
	for _, h := range p.Headers {
		writeHeader(bw, h)
	}
	bw.WriteString("\r\n")
	if p.SubParts != nil {
		for _, sp := range p.SubParts {
			bw.WriteString("--" + p.Boundary + "\r\n")
			if err := writeEntity(ctx, bw, sp); err != nil {
				return err
			}
			bw.WriteString("\r\n")
		}
		_, err := bw.WriteString("--" + p.Boundary + "--\r\n")
		return err
	}
	return writeContent(ctx, bw, p)
}

// writeHeader writes one header field line. A value carrying its own
// leading white space (a client-provided Raw value) is written after the
// bare colon; otherwise a single space separates it.
func writeHeader(bw *bufio.Writer, h HeaderField) {
	bw.WriteString(h.Name)
	bw.WriteString(":")
	if h.Value != "" && h.Value[0] != ' ' && h.Value[0] != '\t' {
		bw.WriteString(" ")
	}
	bw.WriteString(h.Value)
	bw.WriteString("\r\n")
}

// writeContent opens the leaf's content and streams it through the
// declared encoding.
func writeContent(ctx context.Context, bw *bufio.Writer, p *OutPart) error {
	rc, err := p.Content(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()
	switch p.Encoding {
	case EncBase64:
		enc := base64.NewEncoder(base64.StdEncoding, &lineWrapper{w: bw, width: 76})
		if _, err := io.Copy(enc, rc); err != nil {
			return err
		}
		return enc.Close()
	case EncQP:
		qp := quotedprintable.NewWriter(bw)
		if _, err := io.Copy(qp, rc); err != nil {
			return err
		}
		return qp.Close()
	default: // identity: 7bit / 8bit
		_, err := io.Copy(bw, rc)
		return err
	}
}

// lineWrapper inserts a CRLF after every width octets written through it
// (the RFC 2045 section 6.8 base64 line limit).
type lineWrapper struct {
	w     io.Writer
	width int
	col   int
}

func (lw *lineWrapper) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		room := lw.width - lw.col
		chunk := p
		if len(chunk) > room {
			chunk = chunk[:room]
		}
		n, err := lw.w.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		lw.col += n
		p = p[n:]
		if lw.col == lw.width {
			if _, err := lw.w.Write([]byte("\r\n")); err != nil {
				return written, err
			}
			lw.col = 0
		}
	}
	return written, nil
}
