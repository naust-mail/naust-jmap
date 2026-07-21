// Package message parses RFC 5322 messages (and their MIME structure,
// RFC 2045/2046) into the shapes the JMAP Email object needs, per
// RFC 8621 section 4.1. Parsing is best effort and never fails on malformed
// input: it degrades to the closest sensible representation rather than an
// error, because the mail store must be able to represent every message it is
// handed, valid or not. The only error Parse reports is a failure reading the
// input.
//
// The parse result is metadata only: the header list and a tree of body-part
// nodes carrying each part's MIME metadata. A part's decoded content is never
// held on the tree. Content is processed only for the sinks a caller declares
// through a SinkFactory (see sinks.go), so a caller that needs only structure
// (delivery computing hasAttachment) touches no body octets, and the only
// content-derived values ever stored on a Part - its Digest and Size - appear
// only when the caller asked the pipeline to produce them.
package message

import (
	"io"
	"strings"
)

// HeaderField is one header field instance in Raw form (RFC 8621 4.1.2.1):
// original capitalization, raw value octets after the colon with NULs
// dropped and invalid UTF-8 replaced by U+FFFD, internal folding kept,
// terminating CRLF excluded.
type HeaderField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Address is the EmailAddress object (RFC 8621 4.1.2.3). Name is nil when
// the mailbox has no display-name (and no usable trailing comment).
type Address struct {
	Name  *string `json:"name"`
	Email string  `json:"email"`
}

// AddressGroup is the EmailAddressGroup object (RFC 8621 4.1.2.4). Name is
// nil for mailboxes that are not part of a group.
type AddressGroup struct {
	Name      *string   `json:"name"`
	Addresses []Address `json:"addresses"`
}

// Part is one node of the bodyStructure tree (EmailBodyPart, RFC 8621
// 4.1.4). Exactly the multipart/* nodes have SubParts != nil, a "" PartID,
// and no content identity; every other node is a leaf (message/rfc822 is
// not recursed into).
type Part struct {
	PartID      string  // "" iff multipart/*
	Type        string  // lowercase type/subtype, CFWS removed, parameters stripped
	Charset     *string // charset rules of 4.1.4; nil when spec says null
	Disposition *string // lowercase Content-Disposition value, parameters stripped
	Cid         *string // Content-Id without CFWS or angle brackets
	Language    []string
	Location    *string
	Name        *string // decoded filename (RFC 2231) or Content-Type name
	Headers     []HeaderField
	SubParts    []*Part // non-nil iff multipart/*

	// Size and Digest are the leaf's content identity, produced by the content
	// pipeline (sinks.go) only when a SinkFactory requests Identity for this
	// leaf. Size is the decoded octet count (the "size" property, RFC 8621
	// 4.1.4); Digest is the SHA-256 of the decoded content that IdFromDigest
	// turns into the blobId (the content address, RFC 8620 6.1). Both are zero
	// on multipart/* nodes (no content) and on leaves whose content was never
	// processed. EncodingProblem reports a Content-Transfer-Encoding that was
	// unknown or failed to decode (feeds EmailBodyValue.isEncodingProblem,
	// 4.1.4); it is meaningful only when the content was decoded.
	Size            uint64
	Digest          [32]byte
	EncodingProblem bool
}

// Message is the parse result for one raw message: its header list and the
// root of the bodyStructure tree. The body-part views (textBody, htmlBody,
// attachments), hasAttachment, and preview are not stored here; they are
// derived from the tree by the caller (Flatten, HasAttachment, BuildPreview),
// which keeps the parser free of any consumer's request semantics.
type Message struct {
	Headers []HeaderField
	Root    *Part
}

// Parse reads a raw RFC 5322 message and returns its header list and MIME
// structure tree. Structure and metadata are always produced; a leaf's content
// is decoded only for the sinks factory declares for it (see SinkFactory), so a
// caller needing only structure touches no body octets. Parsing is best effort
// and never fails on malformed input; the only error is a failure reading r.
func Parse(r io.Reader, factory SinkFactory) (*Message, error) {
	lr := newLineReader(r)
	headers, err := readHeaderBlock(lr)
	if err != nil {
		return nil, err
	}
	st := &walkState{budget: maxParts, factory: factory}
	root, err := walkEntity(st, lr, headers, "text/plain", 0)
	lr.release()
	if err != nil {
		return nil, err
	}
	return &Message{Headers: headers, Root: root}, nil
}

// HeaderInstances returns the raw values of every instance of the named
// header field, in message order. Names match case-insensitively.
func (m *Message) HeaderInstances(name string) []string {
	return headerInstances(m.Headers, name)
}

// HeaderLast returns the raw value of the last instance of the named
// header field, or ok=false if the field is absent.
func (m *Message) HeaderLast(name string) (string, bool) {
	return headerLast(m.Headers, name)
}

// HeaderInstances is the per-part variant of Message.HeaderInstances.
func (p *Part) HeaderInstances(name string) []string {
	return headerInstances(p.Headers, name)
}

// HeaderLast is the per-part variant of Message.HeaderLast.
func (p *Part) HeaderLast(name string) (string, bool) {
	return headerLast(p.Headers, name)
}

func headerInstances(headers []HeaderField, name string) []string {
	var vals []string
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			vals = append(vals, h.Value)
		}
	}
	return vals
}

func headerLast(headers []HeaderField, name string) (string, bool) {
	for i := len(headers) - 1; i >= 0; i-- {
		if strings.EqualFold(headers[i].Name, name) {
			return headers[i].Value, true
		}
	}
	return "", false
}
