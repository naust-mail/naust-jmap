package frame

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	testMaxMessage   = 1 << 20
	testMaxFragments = 64
)

func newTestReader(stream []byte) *Reader {
	return NewReader(bytes.NewReader(stream), testMaxMessage, testMaxFragments)
}

// clientFrame builds one masked client frame, the only kind a server
// may accept (RFC 6455 section 5.1).
func clientFrame(fin bool, op byte, payload []byte, key [4]byte) []byte {
	var b []byte
	b0 := op
	if fin {
		b0 |= 0x80
	}
	b = append(b, b0)
	switch {
	case len(payload) <= 125:
		b = append(b, 0x80|byte(len(payload)))
	case len(payload) <= 0xffff:
		b = append(b, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
	default:
		b = append(b, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(len(payload)))
		b = append(b, ext[:]...)
	}
	b = append(b, key[:]...)
	masked := make([]byte, len(payload))
	copy(masked, payload)
	mask(masked, key)
	return append(b, masked...)
}

func wantProtocolError(t *testing.T, stream []byte, code uint16) {
	t.Helper()
	rd := newTestReader(stream)
	for {
		_, err := rd.Next()
		if err == nil {
			continue
		}
		var pe *ProtocolError
		if !errors.As(err, &pe) {
			t.Fatalf("got %v, want a ProtocolError with code %d", err, code)
		}
		if pe.Code != code {
			t.Fatalf("close code = %d (%s), want %d", pe.Code, pe.Reason, code)
		}
		return
	}
}

// --- RFC 6455 section 5.7 examples, verbatim ---

// "A single-frame masked text message": 0x81 0x85 0x37 0xfa 0x21 0x3d
// 0x7f 0x9f 0x4d 0x51 0x58 contains "Hello".
func TestExampleSingleFrameMaskedText(t *testing.T) {
	stream := []byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58}
	msg, err := newTestReader(stream).Next()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Opcode != OpText || string(msg.Payload) != "Hello" {
		t.Fatalf("got op %d payload %q", msg.Opcode, msg.Payload)
	}
}

// "A single-frame unmasked text message": 0x81 0x05 0x48 0x65 0x6c
// 0x6c 0x6f contains "Hello". That is the server-to-client direction,
// so it is what WriteMessage must produce - and, fed back to the
// server, an unmasked frame that MUST be rejected (5.1).
func TestExampleSingleFrameUnmaskedText(t *testing.T) {
	want := []byte{0x81, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, OpText, []byte("Hello")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("wrote % x, want % x", buf.Bytes(), want)
	}
	wantProtocolError(t, want, CloseProtocolError)
}

// "A fragmented unmasked text message": 0x01 0x03 "Hel" then 0x80 0x02
// "lo". Unmasked frames are rejected by a server, so the reassembly is
// exercised through the masked equivalent of the same split.
func TestExampleFragmentedText(t *testing.T) {
	unmasked := []byte{0x01, 0x03, 0x48, 0x65, 0x6c, 0x80, 0x02, 0x6c, 0x6f}
	wantProtocolError(t, unmasked, CloseProtocolError)

	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	stream := append(
		clientFrame(false, OpText, []byte("Hel"), key),
		clientFrame(true, OpContinuation, []byte("lo"), key)...)
	msg, err := newTestReader(stream).Next()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Opcode != OpText || string(msg.Payload) != "Hello" {
		t.Fatalf("got op %d payload %q", msg.Opcode, msg.Payload)
	}
}

// "Unmasked Ping request and masked Ping response": the unmasked ping
// 0x89 0x05 "Hello" is what the server sends; the masked pong 0x8a
// 0x85 0x37 0xfa 0x21 0x3d 0x7f 0x9f 0x4d 0x51 0x58 is what it reads.
func TestExamplePingPong(t *testing.T) {
	wantPing := []byte{0x89, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, OpPing, []byte("Hello")); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), wantPing) {
		t.Fatalf("ping: wrote % x, want % x", buf.Bytes(), wantPing)
	}

	maskedPong := []byte{0x8a, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58}
	msg, err := newTestReader(maskedPong).Next()
	if err != nil {
		t.Fatal(err)
	}
	if msg.Opcode != OpPong || string(msg.Payload) != "Hello" {
		t.Fatalf("got op %d payload %q", msg.Opcode, msg.Payload)
	}
}

// "256 bytes binary message in a single unmasked frame": 0x82 0x7E
// 0x0100 followed by the data; "64KiB binary message in a single
// unmasked frame": 0x82 0x7F 0x0000000000010000 followed by the data.
func TestExampleExtendedLengths(t *testing.T) {
	data256 := bytes.Repeat([]byte{0xAB}, 256)
	var buf bytes.Buffer
	if err := WriteMessage(&buf, OpBinary, data256); err != nil {
		t.Fatal(err)
	}
	if want := append([]byte{0x82, 0x7E, 0x01, 0x00}, data256...); !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("256-byte frame header = % x", buf.Bytes()[:4])
	}

	data64k := bytes.Repeat([]byte{0xCD}, 65536)
	buf.Reset()
	if err := WriteMessage(&buf, OpBinary, data64k); err != nil {
		t.Fatal(err)
	}
	if want := append([]byte{0x82, 0x7F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00}, data64k...); !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("64KiB frame header = % x", buf.Bytes()[:10])
	}

	// The same lengths read back through the masked client direction.
	key := [4]byte{1, 2, 3, 4}
	for _, data := range [][]byte{data256, data64k} {
		msg, err := newTestReader(clientFrame(true, OpBinary, data, key)).Next()
		if err != nil {
			t.Fatal(err)
		}
		if msg.Opcode != OpBinary || !bytes.Equal(msg.Payload, data) {
			t.Fatalf("%d-byte round trip failed", len(data))
		}
	}
}

// --- Validation and hostile input ---

func TestRejectReservedBits(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	f := clientFrame(true, OpText, []byte("hi"), key)
	f[0] |= 0x40 // RSV1
	wantProtocolError(t, f, CloseProtocolError)
}

func TestRejectReservedOpcodes(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	for _, op := range []byte{0x3, 0x7, 0xB, 0xF} {
		wantProtocolError(t, clientFrame(true, op, nil, key), CloseProtocolError)
	}
}

// The spec's own non-minimal example: "the length of a 124-byte-long
// string can't be encoded as the sequence 126, 0, 124" (5.2).
func TestRejectNonMinimalLengths(t *testing.T) {
	f16 := []byte{0x81, 0x80 | 126, 0x00, 124, 1, 2, 3, 4}
	wantProtocolError(t, f16, CloseProtocolError)

	f64 := []byte{0x81, 0x80 | 127, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 1, 2, 3, 4}
	wantProtocolError(t, f64, CloseProtocolError)
}

func TestRejectHighBit64Length(t *testing.T) {
	f := []byte{0x81, 0x80 | 127, 0x80, 0, 0, 0, 0, 0, 0, 1, 1, 2, 3, 4}
	wantProtocolError(t, f, CloseProtocolError)
}

// A hostile length must be refused before any allocation or read: the
// stream ends right after the header, and the failure must still be
// 1009, not an IO error from trying to read the claimed payload.
func TestOversizeRefusedBeforeRead(t *testing.T) {
	var ext [8]byte
	binary.BigEndian.PutUint64(ext[:], 1<<62)
	f := append([]byte{0x81, 0x80 | 127}, ext[:]...)
	wantProtocolError(t, f, CloseTooBig)
}

// The reassembled size is what the cap bounds: fragments that are each
// small but together exceed maxMessage fail with 1009.
func TestOversizeAcrossFragments(t *testing.T) {
	key := [4]byte{9, 9, 9, 9}
	half := bytes.Repeat([]byte{'a'}, testMaxMessage/2+1)
	stream := append(
		clientFrame(false, OpText, half, key),
		clientFrame(true, OpContinuation, half, key)...)
	wantProtocolError(t, stream, CloseTooBig)
}

func TestFragmentCountCap(t *testing.T) {
	key := [4]byte{1, 1, 1, 1}
	var stream []byte
	stream = append(stream, clientFrame(false, OpText, []byte("a"), key)...)
	for i := 0; i < testMaxFragments; i++ {
		stream = append(stream, clientFrame(false, OpContinuation, []byte("a"), key)...)
	}
	wantProtocolError(t, stream, CloseTooBig)
}

func TestRejectFragmentedControl(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	wantProtocolError(t, clientFrame(false, OpPing, []byte("x"), key), CloseProtocolError)
}

func TestRejectOversizeControl(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	wantProtocolError(t, clientFrame(true, OpPing, bytes.Repeat([]byte{'x'}, 126), key), CloseProtocolError)
}

func TestRejectLoneContinuation(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	wantProtocolError(t, clientFrame(true, OpContinuation, []byte("x"), key), CloseProtocolError)
}

func TestRejectInterleavedDataFrames(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	stream := append(
		clientFrame(false, OpText, []byte("a"), key),
		clientFrame(true, OpText, []byte("b"), key)...)
	wantProtocolError(t, stream, CloseProtocolError)
}

// Control frames interleave inside a fragmented message and the
// reassembly survives them (5.4, 5.5).
func TestControlInterleavedInFragments(t *testing.T) {
	key := [4]byte{5, 6, 7, 8}
	var stream []byte
	stream = append(stream, clientFrame(false, OpText, []byte("Hel"), key)...)
	stream = append(stream, clientFrame(true, OpPing, []byte("keepalive"), key)...)
	stream = append(stream, clientFrame(true, OpContinuation, []byte("lo"), key)...)

	rd := newTestReader(stream)
	msg, err := rd.Next()
	if err != nil || msg.Opcode != OpPing || string(msg.Payload) != "keepalive" {
		t.Fatalf("first message: %+v, %v (want the interleaved ping)", msg, err)
	}
	msg, err = rd.Next()
	if err != nil || msg.Opcode != OpText || string(msg.Payload) != "Hello" {
		t.Fatalf("second message: %+v, %v (want the reassembled text)", msg, err)
	}
}

func TestUTF8Validation(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}

	// Invalid in a single frame.
	wantProtocolError(t, clientFrame(true, OpText, []byte{0xff, 0xfe}, key), CloseInvalidPayload)

	// A code point split across a fragment boundary is fine: the check
	// applies to the whole message (5.6). "é" is 0xc3 0xa9.
	stream := append(
		clientFrame(false, OpText, []byte{0xc3}, key),
		clientFrame(true, OpContinuation, []byte{0xa9}, key)...)
	msg, err := newTestReader(stream).Next()
	if err != nil || string(msg.Payload) != "é" {
		t.Fatalf("split code point: %q, %v", msg.Payload, err)
	}

	// A sequence that never completes fails once the message ends.
	stream = append(
		clientFrame(false, OpText, []byte{0xc3}, key),
		clientFrame(true, OpContinuation, nil, key)...)
	wantProtocolError(t, stream, CloseInvalidPayload)

	// Binary messages are never UTF-8 checked.
	msg, err = newTestReader(clientFrame(true, OpBinary, []byte{0xff, 0xfe}, key)).Next()
	if err != nil || msg.Opcode != OpBinary {
		t.Fatalf("binary: %+v, %v", msg, err)
	}
}

func TestCloseFrameValidation(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}

	// Empty body is legal (5.5.1).
	msg, err := newTestReader(clientFrame(true, OpClose, nil, key)).Next()
	if err != nil || msg.Opcode != OpClose {
		t.Fatalf("empty close: %+v, %v", msg, err)
	}

	// One byte cannot hold the 2-byte status code.
	wantProtocolError(t, clientFrame(true, OpClose, []byte{0x03}, key), CloseProtocolError)

	body := func(code uint16, reason string) []byte {
		b := make([]byte, 2+len(reason))
		binary.BigEndian.PutUint16(b, code)
		copy(b[2:], reason)
		return b
	}
	for _, code := range []uint16{1000, 1001, 1003, 1007, 1011, 3000, 4999} {
		if _, err := newTestReader(clientFrame(true, OpClose, body(code, "bye"), key)).Next(); err != nil {
			t.Errorf("legal close code %d rejected: %v", code, err)
		}
	}
	// 0-999 unused; 1004-1006 and 1015 reserved for absence, never on
	// the wire; 1016-2999 undefined; >4999 outside every range (7.4).
	for _, code := range []uint16{0, 999, 1004, 1005, 1006, 1015, 1016, 2999, 5000, 65535} {
		wantProtocolError(t, clientFrame(true, OpClose, body(code, ""), key), CloseProtocolError)
	}

	// A close reason must be UTF-8.
	wantProtocolError(t, clientFrame(true, OpClose, body(1000, "\xff\xfe"), key), CloseInvalidPayload)
}

func TestEmptyMessages(t *testing.T) {
	key := [4]byte{0, 0, 0, 0}
	rd := newTestReader(append(
		clientFrame(true, OpText, nil, key),
		clientFrame(true, OpPing, nil, key)...))
	msg, err := rd.Next()
	if err != nil || msg.Opcode != OpText || len(msg.Payload) != 0 {
		t.Fatalf("empty text: %+v, %v", msg, err)
	}
	msg, err = rd.Next()
	if err != nil || msg.Opcode != OpPing {
		t.Fatalf("empty ping: %+v, %v", msg, err)
	}
}

func TestWriteCloseTruncatesAtRuneBoundary(t *testing.T) {
	var buf bytes.Buffer
	reason := strings.Repeat("é", 100) // 200 bytes of two-byte runes
	if err := WriteClose(&buf, CloseNormal, reason); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if b[0] != 0x80|OpClose {
		t.Fatalf("header %x", b[0])
	}
	if n := int(b[1]); n > 125 {
		t.Fatalf("close payload %d bytes exceeds the control cap", n)
	}
	if !utf8.Valid(b[4:]) {
		t.Fatal("truncation split a rune")
	}
	if code := binary.BigEndian.Uint16(b[2:4]); code != CloseNormal {
		t.Fatalf("code %d", code)
	}
}

// Server frames round-trip: what WriteMessage emits, a client-side
// mask plus our Reader reproduces byte for byte.
func TestRoundTrip(t *testing.T) {
	key := [4]byte{0xde, 0xad, 0xbe, 0xef}
	for _, payload := range []string{"", "x", strings.Repeat("data", 100), strings.Repeat("y", 70000)} {
		f := clientFrame(true, OpText, []byte(payload), key)
		msg, err := newTestReader(f).Next()
		if err != nil {
			t.Fatal(err)
		}
		if string(msg.Payload) != payload {
			t.Fatalf("round trip lost data at %d bytes", len(payload))
		}
	}
}

// A truncated stream is an IO error, never a panic and never a
// ProtocolError that would blame the client for the network.
func TestTruncatedStreams(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	whole := clientFrame(true, OpText, []byte("Hello"), key)
	for cut := 0; cut < len(whole); cut++ {
		rd := newTestReader(whole[:cut])
		_, err := rd.Next()
		if err == nil {
			t.Fatalf("truncation at %d returned a message", cut)
		}
		var pe *ProtocolError
		if errors.As(err, &pe) {
			t.Fatalf("truncation at %d misread as protocol error: %v", cut, err)
		}
	}
}

// --- Boundary values ---

// Exact limits are legal; one past them is not.
func TestBoundaryLengths(t *testing.T) {
	key := [4]byte{9, 8, 7, 6}

	// A control frame at exactly the 125-byte cap (5.5).
	msg, err := newTestReader(clientFrame(true, OpPing, bytes.Repeat([]byte{'p'}, 125), key)).Next()
	if err != nil || len(msg.Payload) != 125 {
		t.Fatalf("125-byte control frame: %v", err)
	}

	// A data message at exactly maxMessage.
	exact := bytes.Repeat([]byte{'a'}, testMaxMessage)
	msg, err = newTestReader(clientFrame(true, OpText, exact, key)).Next()
	if err != nil || len(msg.Payload) != testMaxMessage {
		t.Fatalf("exact-cap message: %v", err)
	}
	// And one byte past it.
	over := bytes.Repeat([]byte{'a'}, testMaxMessage+1)
	wantProtocolError(t, clientFrame(true, OpText, over, key), CloseTooBig)

	// The minimal-encoding boundaries themselves: 126 and 65536 are the
	// smallest lengths of the 16- and 64-bit forms (5.2).
	for _, n := range []int{125, 126, 65535, 65536} {
		payload := bytes.Repeat([]byte{'x'}, n)
		msg, err := newTestReader(clientFrame(true, OpBinary, payload, key)).Next()
		if err != nil || len(msg.Payload) != n {
			t.Fatalf("length %d: %v", n, err)
		}
	}
}

// Exactly maxFragments fragments are legal; the count cap only trips
// beyond it.
func TestFragmentCountExactCap(t *testing.T) {
	key := [4]byte{1, 1, 1, 1}
	var stream []byte
	stream = append(stream, clientFrame(false, OpText, []byte("a"), key)...)
	for i := 0; i < testMaxFragments-2; i++ {
		stream = append(stream, clientFrame(false, OpContinuation, []byte("a"), key)...)
	}
	stream = append(stream, clientFrame(true, OpContinuation, []byte("a"), key)...)
	msg, err := newTestReader(stream).Next()
	if err != nil || len(msg.Payload) != testMaxFragments {
		t.Fatalf("exact-cap fragmentation: %v (%d bytes)", err, len(msg.Payload))
	}
}

// An all-zero masking key is legal (5.3 puts no constraint the server
// may enforce on the key's value; unpredictability is the client's
// obligation).
func TestZeroMaskKey(t *testing.T) {
	msg, err := newTestReader(clientFrame(true, OpText, []byte("Hello"), [4]byte{})).Next()
	if err != nil || string(msg.Payload) != "Hello" {
		t.Fatalf("zero mask key: %v", err)
	}
}

// --- Adversarial UTF-8 (5.6, 8.1: the whole message MUST be valid) ---

func TestUTF8Pathologies(t *testing.T) {
	key := [4]byte{2, 3, 4, 5}
	bad := map[string][]byte{
		"overlong slash":       {0xC0, 0xAF},
		"surrogate half":       {0xED, 0xA0, 0x80},
		"truncated multibyte":  {0xE2, 0x82},
		"lone continuation":    {0x80},
		"out of range (5byte)": {0xF8, 0x88, 0x80, 0x80, 0x80},
	}
	for name, payload := range bad {
		rd := newTestReader(clientFrame(true, OpText, payload, key))
		if _, err := rd.Next(); err == nil {
			t.Errorf("%s accepted as text", name)
		} else {
			var pe *ProtocolError
			if !errors.As(err, &pe) || pe.Code != CloseInvalidPayload {
				t.Errorf("%s: %v, want close 1007", name, err)
			}
		}
	}

	// A 4-byte code point split across three fragments is still one
	// valid message.
	emoji := []byte("\U0001F600")
	stream := append(clientFrame(false, OpText, emoji[:1], key),
		clientFrame(false, OpContinuation, emoji[1:3], key)...)
	stream = append(stream, clientFrame(true, OpContinuation, emoji[3:], key)...)
	msg, err := newTestReader(stream).Next()
	if err != nil || !bytes.Equal(msg.Payload, emoji) {
		t.Fatalf("split code point: %q, %v", msg.Payload, err)
	}

	// A BOM is just a character; it must pass.
	if _, err := newTestReader(clientFrame(true, OpText, []byte("\uFEFFhi"), key)).Next(); err != nil {
		t.Fatalf("BOM rejected: %v", err)
	}
}

// WriteClose at the exact reason cap keeps the body at 125 bytes.
func TestWriteCloseExactCap(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteClose(&buf, CloseNormal, strings.Repeat("r", 123)); err != nil {
		t.Fatal(err)
	}
	if got := buf.Bytes()[1]; got != 125 {
		t.Fatalf("close payload %d bytes, want exactly 125", got)
	}
}
