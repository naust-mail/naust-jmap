package message

import (
	"crypto/sha256"
	"hash"
	"io"
)

// A Sink consumes the decoded octets of one leaf body part as a stream. The
// content pipeline feeds every attached sink the same Content-Transfer-Encoding
// -decoded bytes (RFC 2045 section 6); a sink retains only its own bounded
// result - a capped preview fragment, a running match - and never the whole
// part, so no decoded body part is ever held in full above this layer. Sinks
// are supplied by a SinkFactory, which is how a consumer declares the derived
// content it needs without the parser knowing any request semantics.
type Sink interface {
	io.Writer
	// Close is called once, after the final octet, so the sink can finalize
	// (flush a match, record truncation). It is not called when the part's
	// content is never processed.
	Close() error
}

// LeafSinks is a SinkFactory's answer for one leaf part. Identity requests the
// part's content identity - the SHA-256 digest that IdFromDigest turns into the
// blobId (RFC 8620 section 6.1) and the decoded octet size (RFC 8621 section
// 4.1.4) - computed by the pipeline and assigned to the Part. Sinks are
// consumer writers that receive the same decoded octets. An empty LeafSinks
// means the part's content is not processed at all: no decode, no hashing,
// nothing read - so a caller wanting only structure never decodes hostile
// attachment bytes.
type LeafSinks struct {
	Identity bool
	Sinks    []Sink
}

// SinkFactory maps a leaf Part's metadata to the sinks its content should feed.
// It is called once per leaf, before that leaf's content is decoded, and must
// decide only from the Part's metadata (type, disposition, name, ...). Request
// -level rules belong in the closure that builds the factory, not in per-part
// logic here. It never runs for multipart/* parts, which carry no content of
// their own.
type SinkFactory func(p *Part) LeafSinks

// identityWriter computes a leaf's content identity as its decoded octets pass:
// the SHA-256 digest IdFromDigest turns into the blobId (RFC 8620 6.1) and the
// octet count that is the part's size (RFC 8621 4.1.4). It holds no Part
// reference; the parser reads the finished values and assigns them, so a sink
// never reaches into parser-owned state.
type identityWriter struct {
	h hash.Hash
	n int64
}

func newIdentityWriter() *identityWriter { return &identityWriter{h: sha256.New()} }

func (w *identityWriter) Write(b []byte) (int, error) {
	w.n += int64(len(b))
	return w.h.Write(b)
}

func (w *identityWriter) result() (digest [32]byte, size uint64) {
	w.h.Sum(digest[:0])
	return digest, uint64(w.n)
}

// feedLeafContent processes one leaf part's content exactly as factory declares,
// and no more. When neither identity nor any sink is requested it does not read
// body at all - the walker reads past those octets without them being decoded -
// which is the core guarantee of this parser: content is never processed without
// a declared consumer, so an unauthenticated delivery path never decodes
// attacker-supplied attachment bytes it will not use. When content is requested
// it is decoded once (RFC 2045 section 6) and the decoded octets are fanned to
// the identity writer and every sink together; only a decoded part has a known
// EncodingProblem. The only error is a failure reading body.
func feedLeafContent(p *Part, body io.Reader, cte string, factory SinkFactory) error {
	if factory == nil {
		return nil
	}
	ls := factory(p)
	if !ls.Identity && len(ls.Sinks) == 0 {
		return nil
	}
	var idw *identityWriter
	writers := make([]io.Writer, 0, len(ls.Sinks)+1)
	if ls.Identity {
		idw = newIdentityWriter()
		writers = append(writers, idw)
	}
	for _, s := range ls.Sinks {
		writers = append(writers, s)
	}

	// The content flows from the message straight through the decoder and out to
	// every sink together, in pieces: neither its encoded nor its decoded form is
	// ever assembled. What is kept is what a sink chose to keep.
	dec := newCTEDecoder(body, cte)
	if _, err := io.Copy(fanOut(writers), dec); err != nil {
		return err
	}
	p.EncodingProblem = dec.problem
	for _, s := range ls.Sinks {
		_ = s.Close()
	}
	if idw != nil {
		p.Digest, p.Size = idw.result()
	}
	return nil
}

// fanOut hands each piece of decoded content to every sink, independently. A
// sink keeps bounded state and has no meaningful way to refuse content, so what
// one sink makes of a write says nothing about the message and must not decide
// anything for the sinks beside it: a preview being built is not a reason for a
// search not to match, and neither is a reason to stop parsing. (io.MultiWriter
// is the opposite contract - it stops at the first writer that errors or writes
// short - which is why this is not one.)
type fanOut []io.Writer

func (f fanOut) Write(b []byte) (int, error) {
	for _, w := range f {
		_, _ = w.Write(b)
	}
	return len(b), nil
}
