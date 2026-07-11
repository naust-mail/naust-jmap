// Package keyenc is the shared order-preserving key encoding used by
// every component that stores data in a backend.Backend (objectdb and
// the KV blob store). Keeping it in one place guarantees the packages
// agree on segment boundaries and therefore can never forge or collide
// with each other's keys as long as their tag segments differ.
package keyenc

// Segment escapes 0x00 as 0x00 0xFF and terminates with 0x00 0x01,
// appending to dst. The encoding preserves bytes.Compare order and a
// prefix segment sorts before any longer segment.
func Segment(dst, seg []byte) []byte {
	for _, b := range seg {
		if b == 0x00 {
			dst = append(dst, 0x00, 0xFF)
		} else {
			dst = append(dst, b)
		}
	}
	return append(dst, 0x00, 0x01)
}

// Key concatenates encoded segments.
func Key(segs ...[]byte) []byte {
	var out []byte
	for _, s := range segs {
		out = Segment(out, s)
	}
	return out
}

// PrefixSuccessor returns the smallest key greater than every key with
// the given prefix, or nil if there is none (all 0xFF).
func PrefixSuccessor(prefix []byte) []byte {
	out := make([]byte, len(prefix))
	copy(out, prefix)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < 0xFF {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}

// PrefixRange returns the [start, end) scan bounds covering every key
// that starts with the given segments.
func PrefixRange(segs ...[]byte) (start, end []byte) {
	start = Key(segs...)
	return start, PrefixSuccessor(start)
}
