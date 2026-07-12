// Package message parses RFC 5322 messages (and their MIME structure,
// RFC 2045/2046) into the shapes the JMAP Email object needs, per
// RFC 8621 section 4.1. Parsing is best effort and never fails: malformed
// input degrades to the closest sensible representation rather than an
// error, because the mail store must be able to represent every message
// it is handed, valid or not.
package message

import (
	"crypto/sha256"
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
// and no decoded content; every other node is a leaf (message/rfc822 is
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

	// Decoded is the content after Content-Transfer-Encoding removal (the
	// octets a blob for this part contains). Size and SHA256 describe it.
	// EncodingProblem is set when the transfer encoding was unknown or
	// decoding hit malformed input (feeds EmailBodyValue.isEncodingProblem).
	Decoded         []byte
	Size            uint64
	SHA256          [32]byte
	EncodingProblem bool
}

// Message is the parse result for one raw message.
type Message struct {
	Headers     []HeaderField
	Root        *Part   // bodyStructure
	TextBody    []*Part // 4.1.4 flattened views
	HTMLBody    []*Part
	Attachments []*Part

	HasAttachment bool
	Preview       string // plaintext fragment, at most 256 characters
}

// Parse parses a raw message. It always returns a Message; malformed input
// yields a best-effort representation, never an error.
func Parse(raw []byte) *Message {
	headers, body := parseHeaderBlock(raw)
	var counter int
	root := parsePart(headers, body, "text/plain", &counter, 0)
	m := &Message{Headers: headers, Root: root}
	m.TextBody, m.HTMLBody, m.Attachments = flatten(root)
	m.HasAttachment = hasAttachment(m.Attachments)
	m.Preview = preview(m.TextBody, m.HTMLBody)
	return m
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

func sealContent(p *Part, decoded []byte) {
	p.Decoded = decoded
	p.Size = uint64(len(decoded))
	p.SHA256 = sha256.Sum256(decoded)
}
