package frame

import (
	"bytes"
	"errors"
	"testing"
)

// FuzzReader drives arbitrary bytes through the reader. The wire is
// fully attacker-controlled, so the property is total: every input
// either yields validated messages or a clean error - never a panic -
// and no message breaches the configured caps regardless of what the
// frame headers claim.
func FuzzReader(f *testing.F) {
	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	f.Add([]byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58})
	f.Add(clientFrame(true, OpText, []byte("hello"), key))
	f.Add(clientFrame(true, OpClose, []byte{0x03, 0xe8}, key))
	f.Add(append(clientFrame(false, OpText, []byte("He"), key), clientFrame(true, OpContinuation, []byte("llo"), key)...))
	f.Add([]byte{0x81, 0x80 | 127, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0x00, 0x00})
	f.Add([]byte{})

	const maxMessage, maxFragments = 1 << 16, 8
	f.Fuzz(func(t *testing.T, data []byte) {
		rd := NewReader(bytes.NewReader(data), maxMessage, maxFragments)
		for i := 0; i < 64; i++ {
			msg, err := rd.Next()
			if err != nil {
				var pe *ProtocolError
				if errors.As(err, &pe) && pe.Code == 0 {
					t.Fatal("protocol error with no close code")
				}
				return
			}
			if len(msg.Payload) > maxMessage {
				t.Fatalf("message of %d bytes breached the %d cap", len(msg.Payload), maxMessage)
			}
			if msg.Opcode&0x8 != 0 && len(msg.Payload) > 125 {
				t.Fatal("control frame payload over 125 bytes surfaced")
			}
		}
	})
}
