// Package frame implements the server side of the RFC 6455 base
// framing protocol (section 5): reading masked client frames with
// fragment reassembly and strict validation, and writing unmasked
// server frames. It knows nothing about JMAP; the connection layer
// above decides what messages mean.
//
// Every input is hostile until proven otherwise: lengths are checked
// against caps before any allocation, reserved bits and opcodes fail
// the connection, and validation failures carry the RFC 7.4.1 status
// code the connection should close with.
package frame

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"unicode/utf8"
)

// Frame opcodes (RFC 6455 section 5.2).
const (
	OpContinuation byte = 0x0
	OpText         byte = 0x1
	OpBinary       byte = 0x2
	OpClose        byte = 0x8
	OpPing         byte = 0x9
	OpPong         byte = 0xA
)

// Close status codes this server sends (RFC 6455 section 7.4.1).
const (
	CloseNormal          uint16 = 1000
	CloseGoingAway       uint16 = 1001
	CloseProtocolError   uint16 = 1002
	CloseUnsupportedData uint16 = 1003
	CloseInvalidPayload  uint16 = 1007
	ClosePolicyViolation uint16 = 1008
	CloseTooBig          uint16 = 1009
	CloseInternalError   uint16 = 1011
)

// ProtocolError reports input that fails RFC 6455 validation. Code is
// the section 7.4.1 status the connection must be failed with.
type ProtocolError struct {
	Code   uint16
	Reason string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("websocket: %s (close %d)", e.Reason, e.Code)
}

// Message is one complete message: for OpText and OpBinary the
// fragments are already reassembled (section 5.4); OpClose, OpPing,
// and OpPong arrive as-is (control frames cannot fragment, 5.5).
type Message struct {
	Opcode  byte
	Payload []byte
}

// Reader reads and validates client frames from a stream. It is not
// safe for concurrent use; the connection owns one reading goroutine.
type Reader struct {
	r io.Reader

	// OnFrameHeader, when set, is called on the reading goroutine each
	// time a frame header has arrived, before the frame is processed.
	// The connection layer uses it to bound how long a peer that has
	// started sending may take to finish (section 10.4).
	OnFrameHeader func()

	// maxMessage caps a message's total reassembled payload; a message
	// that would exceed it fails with 1009 before the excess is read.
	maxMessage int64
	// maxFragments caps how many frames one message may span, so a
	// stream of tiny fragments cannot buy unbounded per-frame overhead.
	maxFragments int

	// Reassembly state for the message in progress (section 5.4).
	// utfOK is how much of partial has already passed text validation,
	// so invalid UTF-8 fails as it arrives, not at message end.
	partial   []byte
	partialOp byte
	frags     int
	utfOK     int

	// dirty is set when a read error cut the stream inside a frame:
	// some of its bytes were consumed and some were not, so the next
	// byte's meaning is unknown and no further parse can be trusted.
	dirty bool
}

// MessageInProgress reports whether a fragmented message is mid
// assembly (section 5.4), so the connection layer can keep a deadline
// armed across the control frames Next returns in between.
func (rd *Reader) MessageInProgress() bool {
	return rd.partial != nil
}

// Dirty reports whether an earlier read error (typically a forced
// deadline) cut the stream inside a frame, leaving the parse position
// unknown. A dirty reader must not be resumed; one whose reads only
// ever failed on frame boundaries can keep parsing.
func (rd *Reader) Dirty() bool { return rd.dirty }

// NewReader wraps a stream (typically the buffered reader left over
// from the HTTP hijack). maxMessage and maxFragments must be positive.
func NewReader(r io.Reader, maxMessage int64, maxFragments int) *Reader {
	if maxMessage <= 0 || maxFragments <= 0 {
		// Programmer error, not peer input: a non-positive cap would
		// refuse every data frame or every fragment.
		panic("websocket: NewReader requires positive maxMessage and maxFragments")
	}
	return &Reader{r: r, maxMessage: maxMessage, maxFragments: maxFragments}
}

func (rd *Reader) full(buf []byte) error {
	n, err := io.ReadFull(rd.r, buf)
	if err != nil && n > 0 {
		rd.dirty = true
	}
	return err
}

// midFull reads bytes that belong to a frame whose header has already
// been consumed: any failure here, even before a byte arrives, strands
// the parse position inside that frame.
func (rd *Reader) midFull(buf []byte) error {
	if err := rd.full(buf); err != nil {
		rd.dirty = true
		return err
	}
	return nil
}

// Next returns the next complete message. Control frames interleaved
// inside a fragmented message are returned as they arrive, with the
// reassembly state kept (section 5.4). A *ProtocolError means the
// connection must be failed with the error's close code; any other
// error is the underlying stream failing.
func (rd *Reader) Next() (Message, error) {
	for {
		var hdr [2]byte
		if err := rd.full(hdr[:]); err != nil {
			return Message{}, err
		}
		if rd.OnFrameHeader != nil {
			rd.OnFrameHeader()
		}
		fin := hdr[0]&0x80 != 0
		if hdr[0]&0x70 != 0 {
			// RSV1-3 MUST be 0 with no negotiated extension (5.2); this
			// server negotiates none.
			return Message{}, &ProtocolError{CloseProtocolError, "reserved bits set"}
		}
		op := hdr[0] & 0x0f
		masked := hdr[1]&0x80 != 0
		if !masked {
			// Every client frame MUST be masked (5.1).
			return Message{}, &ProtocolError{CloseProtocolError, "client frame not masked"}
		}

		length := int64(hdr[1] & 0x7f)
		switch length {
		case 126:
			var ext [2]byte
			if err := rd.midFull(ext[:]); err != nil {
				return Message{}, err
			}
			length = int64(binary.BigEndian.Uint16(ext[:]))
			if length <= 125 {
				// The minimal number of bytes MUST encode the length (5.2).
				return Message{}, &ProtocolError{CloseProtocolError, "non-minimal 16-bit length"}
			}
		case 127:
			var ext [8]byte
			if err := rd.midFull(ext[:]); err != nil {
				return Message{}, err
			}
			v := binary.BigEndian.Uint64(ext[:])
			if v&(1<<63) != 0 {
				// The most significant bit MUST be 0 (5.2).
				return Message{}, &ProtocolError{CloseProtocolError, "64-bit length with high bit set"}
			}
			if v <= 0xffff {
				return Message{}, &ProtocolError{CloseProtocolError, "non-minimal 64-bit length"}
			}
			length = int64(v)
		}

		control := op&0x8 != 0
		if control {
			if !fin {
				// Control frames MUST NOT be fragmented (5.5).
				return Message{}, &ProtocolError{CloseProtocolError, "fragmented control frame"}
			}
			if length > 125 {
				// Control frames MUST have a payload of 125 bytes or less (5.5).
				return Message{}, &ProtocolError{CloseProtocolError, "control frame payload over 125 bytes"}
			}
			switch op {
			case OpClose, OpPing, OpPong:
			default:
				// 0xB-F are reserved; an unknown opcode fails the connection (5.2).
				return Message{}, &ProtocolError{CloseProtocolError, "reserved control opcode"}
			}
		} else {
			switch op {
			case OpContinuation:
				if rd.partial == nil {
					// A continuation needs a message in progress (5.4).
					return Message{}, &ProtocolError{CloseProtocolError, "continuation with no message in progress"}
				}
			case OpText, OpBinary:
				if rd.partial != nil {
					// Fragments of two messages cannot interleave (5.4).
					return Message{}, &ProtocolError{CloseProtocolError, "new data frame inside a fragmented message"}
				}
			default:
				// 0x3-7 are reserved; an unknown opcode fails the connection (5.2).
				return Message{}, &ProtocolError{CloseProtocolError, "reserved data opcode"}
			}
			// Check the size budget BEFORE reading: a hostile length never
			// allocates. Reassembled size counts (5.4: a fragmented message
			// equals the concatenation of its fragments).
			if length > rd.maxMessage-int64(len(rd.partial)) {
				return Message{}, &ProtocolError{CloseTooBig, "message exceeds the maximum size"}
			}
			rd.frags++
			if rd.frags > rd.maxFragments {
				return Message{}, &ProtocolError{CloseTooBig, "message split into too many fragments"}
			}
		}

		var key [4]byte
		if err := rd.midFull(key[:]); err != nil {
			return Message{}, err
		}

		if control {
			// Control payloads are at most 125 bytes (checked above), so
			// an exact allocation is already bounded.
			payload := make([]byte, length)
			if err := rd.midFull(payload); err != nil {
				return Message{}, err
			}
			mask(payload, key, 0)
			if op == OpClose {
				if err := validateClose(payload); err != nil {
					return Message{}, err
				}
			}
			return Message{Opcode: op, Payload: payload}, nil
		}

		if op != OpContinuation {
			rd.partialOp = op
			// Non-nil marks a message in progress even when this first
			// fragment carries no payload (section 5.4).
			rd.partial = []byte{}
			rd.utfOK = 0
		}
		if err := rd.appendPayload(length, key); err != nil {
			return Message{}, err
		}
		if !fin {
			continue
		}
		msg := Message{Opcode: rd.partialOp, Payload: rd.partial}
		tail := rd.utfOK
		rd.partial, rd.partialOp, rd.frags, rd.utfOK = nil, 0, 0, 0
		if msg.Opcode == OpText && !utf8.Valid(msg.Payload[tail:]) {
			// Everything before tail was validated as it arrived; the
			// message may not end mid code point (5.6, 8.1).
			return Message{}, &ProtocolError{CloseInvalidPayload, "text message is not valid UTF-8"}
		}
		return msg, nil
	}
}

// allocChunk bounds how far payload allocation may run ahead of the
// bytes actually received. A frame header alone commits at most this
// much memory; claiming a huge length costs the sender the bandwidth
// to fill it (section 10.4 resource limits).
const allocChunk = 32 << 10

// appendPayload reads a data frame's payload directly onto the
// reassembly buffer in allocChunk-bounded steps, unmasking each chunk
// as it arrives. The buffer grows with received bytes, never with a
// merely claimed length, and a fragment is never buffered separately
// from the message it extends.
func (rd *Reader) appendPayload(length int64, key [4]byte) error {
	for done := int64(0); done < length; {
		n := int(min(length-done, allocChunk))
		off := len(rd.partial)
		rd.partial = slices.Grow(rd.partial, n)[:off+n]
		if err := rd.midFull(rd.partial[off:]); err != nil {
			return err
		}
		// The mask key index continues across chunk boundaries: byte i
		// of the frame's payload is masked with key[i%4] (section 5.3).
		mask(rd.partial[off:], key, done)
		done += int64(n)
		if rd.partialOp == OpText {
			if err := rd.validateChunk(); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateChunk checks the not-yet-validated span of a text message,
// leaving at most utf8.UTFMax-1 trailing bytes of a possibly still
// incomplete code point for the next chunk. Invalid text MUST fail
// the connection (5.6, 8.1); checking as bytes arrive means it does
// so before a full message's worth of buffering, not after.
func (rd *Reader) validateChunk() error {
	p, cut := rd.partial, len(rd.partial)
	for i := 1; i < utf8.UTFMax && cut-i >= rd.utfOK; i++ {
		if utf8.RuneStart(p[cut-i]) {
			if !utf8.FullRune(p[cut-i:]) {
				cut -= i
			}
			break
		}
	}
	if !utf8.Valid(p[rd.utfOK:cut]) {
		return &ProtocolError{CloseInvalidPayload, "text message is not valid UTF-8"}
	}
	rd.utfOK = cut
	return nil
}

// mask applies the section 5.3 transform in place (it is its own
// inverse), starting at the given byte offset of the frame's payload
// so a payload can be unmasked in chunks. The 4-byte key rotated to
// the offset's phase and repeated fills a 64-bit word, so eight bytes
// transform per XOR; a byte loop finishes the sub-word tail.
func mask(p []byte, key [4]byte, offset int64) {
	var k [4]byte
	for i := range k {
		k[i] = key[(offset+int64(i))%4]
	}
	k32 := binary.LittleEndian.Uint32(k[:])
	k64 := uint64(k32) | uint64(k32)<<32
	for len(p) >= 8 {
		binary.LittleEndian.PutUint64(p, binary.LittleEndian.Uint64(p)^k64)
		p = p[8:]
	}
	for i, b := range p {
		p[i] = b ^ k[i%4]
	}
}

// validateClose checks a close frame body (5.5.1): empty is allowed; a
// single byte is malformed; otherwise the first two bytes are a status
// code that must be legal to receive, and the rest a UTF-8 reason.
func validateClose(payload []byte) error {
	switch {
	case len(payload) == 0:
		return nil
	case len(payload) == 1:
		return &ProtocolError{CloseProtocolError, "close frame with a 1-byte body"}
	}
	code := binary.BigEndian.Uint16(payload[:2])
	if !validReceivedCloseCode(code) {
		return &ProtocolError{CloseProtocolError, "close frame with an invalid status code"}
	}
	if !utf8.Valid(payload[2:]) {
		return &ProtocolError{CloseInvalidPayload, "close reason is not valid UTF-8"}
	}
	return nil
}

// validReceivedCloseCode reports whether a peer may put code on the
// wire (7.4.1, 7.4.2): the defined 1000-1011 codes minus the
// reserved-for-absence values 1004-1006, plus 1012-1014 (registered by
// later specifications in the 1000-2999 protocol range), plus the
// 3000-4999 application ranges. 1015 and everything else is never
// legal in a received frame.
func validReceivedCloseCode(code uint16) bool {
	switch {
	case code >= 1000 && code <= 1003:
		return true
	case code >= 1007 && code <= 1014:
		return true
	case code >= 3000 && code <= 4999:
		return true
	}
	return false
}

// WriteMessage writes one complete unmasked server frame (FIN set):
// the server never masks (5.1) and never needs to fragment. The caller
// serializes writers and sets any write deadline on w's connection.
func WriteMessage(w io.Writer, op byte, payload []byte) error {
	// The writer enforces what the wire format cannot express: opcodes
	// are 4 bits (5.2) and a control payload is at most 125 bytes and
	// unfragmented (5.5) - FIN is always set here.
	if op > 0x0f {
		return fmt.Errorf("websocket: invalid opcode %#x", op)
	}
	if op&0x8 != 0 && len(payload) > 125 {
		return fmt.Errorf("websocket: control frame payload of %d bytes exceeds 125", len(payload))
	}
	var hdr [10]byte
	hdr[0] = 0x80 | op
	n := 2
	switch {
	case len(payload) <= 125:
		hdr[1] = byte(len(payload))
	case len(payload) <= 0xffff:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(len(payload)))
		n = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(len(payload)))
		n = 10
	}
	// Header and payload go out as one gathered write: on a TCP
	// connection net.Buffers is a single writev, so a frame never costs
	// two syscalls, and on any other writer it degrades to the same
	// sequential writes. The bytes on the wire are identical either way.
	bufs := net.Buffers{hdr[:n], payload}
	_, err := bufs.WriteTo(w)
	return err
}

// WriteClose writes a close frame carrying code and reason (5.5.1).
// The reason is truncated to fit the 125-byte control frame cap at a
// UTF-8 boundary, so the body stays valid.
func WriteClose(w io.Writer, code uint16, reason string) error {
	const maxReason = 125 - 2
	for len(reason) > maxReason {
		// Drop the trailing rune, never a fraction of one.
		_, size := utf8.DecodeLastRuneInString(reason)
		reason = reason[:len(reason)-size]
	}
	body := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(body[:2], code)
	copy(body[2:], reason)
	return WriteMessage(w, OpClose, body)
}
