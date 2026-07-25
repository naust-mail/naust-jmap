package keyenc

import (
	"bytes"
	"testing"
)

func TestSegment(t *testing.T) {
	cases := []struct {
		name string
		seg  []byte
		want []byte
	}{
		{"empty", []byte{}, []byte{0x00, 0x01}},
		{"no zero bytes", []byte("abc"), []byte{'a', 'b', 'c', 0x00, 0x01}},
		{"single zero byte", []byte{0x00}, []byte{0x00, 0xFF, 0x00, 0x01}},
		{"zero at start", []byte{0x00, 'a'}, []byte{0x00, 0xFF, 'a', 0x00, 0x01}},
		{"zero at end", []byte{'a', 0x00}, []byte{'a', 0x00, 0xFF, 0x00, 0x01}},
		{"consecutive zeros", []byte{0x00, 0x00}, []byte{0x00, 0xFF, 0x00, 0xFF, 0x00, 0x01}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Segment(nil, c.seg)
			if !bytes.Equal(got, c.want) {
				t.Fatalf("Segment(nil, %v) = %v, want %v", c.seg, got, c.want)
			}
		})
	}
}

// TestSegmentAppends confirms Segment appends to an existing prefix rather
// than discarding it.
func TestSegmentAppends(t *testing.T) {
	dst := []byte("prefix:")
	got := Segment(dst, []byte("abc"))
	want := []byte("prefix:abc\x00\x01")
	if !bytes.Equal(got, want) {
		t.Fatalf("Segment(dst, abc) = %v, want %v", got, want)
	}
}

// TestSegmentOrderPreserving is the encoding's core guarantee: encoding
// must preserve bytes.Compare order, including the "prefix sorts before a
// longer segment" case that the 0x00 0x01 terminator exists to make true
// (without a terminator, "ab" would sort after "a\x00" the same way it
// already sorts after "ac").
func TestSegmentOrderPreserving(t *testing.T) {
	segs := [][]byte{
		{},
		{0x00},
		{0x00, 0x00},
		[]byte("a"),
		[]byte("a\x00"),
		[]byte("aa"),
		[]byte("ab"),
		[]byte("ab\x00"),
		[]byte("ac"),
		[]byte("b"),
		{0xFF},
		{0xFF, 0x00},
		{0xFF, 0xFF},
	}
	for i := range segs {
		for j := range segs {
			wantCmp := bytes.Compare(segs[i], segs[j])
			gotCmp := bytes.Compare(Segment(nil, segs[i]), Segment(nil, segs[j]))
			// Both sides only need to agree on sign, not exact value.
			if sign(wantCmp) != sign(gotCmp) {
				t.Fatalf("order mismatch: bytes.Compare(%v,%v)=%d but bytes.Compare(Segment(%v),Segment(%v))=%d",
					segs[i], segs[j], wantCmp, segs[i], segs[j], gotCmp)
			}
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// TestSegmentBodyNeverContainsTerminator is the anti-forgery property the
// package doc promises: only 0x00 is escaped (to 0x00 0xFF), so if a
// hostile segment value's own bytes could produce a literal 0x00 0x01 pair
// ahead of its real terminator, a caller could smuggle a fake segment
// boundary into a key built from attacker-influenced input (an account id,
// a record id). It must be structurally impossible: every 0x00 in the
// input is rewritten to 0x00 0xFF, so 0x00 0x01 can only ever appear as
// the genuine terminator this function appends.
func TestSegmentBodyNeverContainsTerminator(t *testing.T) {
	segs := [][]byte{
		{},
		{0x00, 0x01},                   // the raw terminator bytes themselves
		{0x00, 0x01, 0x00, 0x01},       // repeated
		{'a', 0x00, 0x01, 'b'},         // embedded mid-segment
		{0x00, 0x00, 0x01},             // adjacent escape and forged terminator
		{0x01, 0x00},                   // reversed order
		bytes.Repeat([]byte{0x00}, 20), // long run of zeros
	}
	for _, seg := range segs {
		enc := Segment(nil, seg)
		if len(enc) < 2 {
			t.Fatalf("Segment(%v) too short: %v", seg, enc)
		}
		body, term := enc[:len(enc)-2], enc[len(enc)-2:]
		if !bytes.Equal(term, []byte{0x00, 0x01}) {
			t.Fatalf("Segment(%v): missing terminator, got %v", seg, enc)
		}
		if idx := bytes.Index(body, []byte{0x00, 0x01}); idx != -1 {
			t.Fatalf("Segment(%v): forged terminator at body offset %d: %v", seg, idx, enc)
		}
	}
}

// TestKeyNoCollisionAcrossSegmentations checks the practical form of "can
// never collide": encoding the same total logical bytes split at different
// segment boundaries must produce different keys, since callers (objectdb
// building a key from account id, type name, record id) rely on the
// segment structure itself being part of the key's meaning, not just the
// concatenated bytes.
func TestKeyNoCollisionAcrossSegmentations(t *testing.T) {
	splits := [][][]byte{
		{[]byte("ab"), []byte("c")},
		{[]byte("a"), []byte("bc")},
		{[]byte("a"), []byte("b"), []byte("c")},
		{[]byte("abc")},
		{[]byte(""), []byte("abc")},
		{[]byte("abc"), []byte("")},
	}
	seen := map[string][][]byte{}
	for _, segs := range splits {
		got := string(Key(segs...))
		if prior, ok := seen[got]; ok {
			t.Fatalf("Key(%v) collides with Key(%v): both = %v", segs, prior, Key(segs...))
		}
		seen[got] = segs
	}
}

func TestKey(t *testing.T) {
	got := Key([]byte("a"), []byte("bc"))
	want := append(Segment(nil, []byte("a")), Segment(nil, []byte("bc"))...)
	if !bytes.Equal(got, want) {
		t.Fatalf("Key(a,bc) = %v, want %v", got, want)
	}
}

func TestKeyNoSegments(t *testing.T) {
	got := Key()
	if len(got) != 0 {
		t.Fatalf("Key() = %v, want empty", got)
	}
}

// TestKeyExactCapacity checks the documented "one exactly-sized
// allocation" property: the size pre-computation (including the +2 per
// segment for the terminator, and one extra +1 per embedded zero byte for
// its escape) must match what Segment actually writes, or the pre-sized
// buffer would need to grow past its capacity.
func TestKeyExactCapacity(t *testing.T) {
	cases := [][]byte{
		{},
		[]byte("hello"),
		{0x00},
		{0x00, 0x00, 0x00},
		[]byte("a\x00b\x00c"),
	}
	for _, seg := range cases {
		got := Key(seg)
		if cap(got) != len(got) {
			t.Fatalf("Key(%v): cap=%d len=%d, want exact allocation", seg, cap(got), len(got))
		}
	}
}

// TestKeyOrdering exercises the reason keys are built from multiple
// segments instead of one concatenated byte slice: each segment is a
// distinct comparison unit, most-significant first, and a record's key
// sorts before any key that extends it with more segments.
func TestKeyOrdering(t *testing.T) {
	cases := []struct {
		name       string
		a, b       [][]byte
		wantALessB bool
	}{
		{"first segment dominates", [][]byte{[]byte("a"), []byte("z")}, [][]byte{[]byte("b"), []byte("a")}, true},
		{"prefix sorts before extension", [][]byte{[]byte("a")}, [][]byte{[]byte("a"), []byte("b")}, true},
		{"equal segments compare equal", [][]byte{[]byte("a"), []byte("b")}, [][]byte{[]byte("a"), []byte("b")}, false},
		{"second segment breaks tie", [][]byte{[]byte("a"), []byte("x")}, [][]byte{[]byte("a"), []byte("y")}, true},
		{"embedded zero does not break segmentation", [][]byte{{0x00}, []byte("a")}, [][]byte{[]byte("a")}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := Key(c.a...), Key(c.b...)
			cmp := bytes.Compare(a, b)
			if c.wantALessB && cmp >= 0 {
				t.Fatalf("Key(%v)=%v not < Key(%v)=%v", c.a, a, c.b, b)
			}
			if !c.wantALessB && cmp != 0 {
				t.Fatalf("Key(%v)=%v want == Key(%v)=%v", c.a, a, c.b, b)
			}
		})
	}
}

func TestPrefixSuccessor(t *testing.T) {
	cases := []struct {
		name   string
		prefix []byte
		want   []byte
	}{
		{"empty", []byte{}, nil},
		{"simple increment", []byte{0x01}, []byte{0x02}},
		{"increments last byte", []byte("ab"), []byte("ac")},
		{"trailing 0xFF truncated", []byte{0x01, 0xFF}, []byte{0x02}},
		{"multiple trailing 0xFF truncated", []byte{0x01, 0xFF, 0xFF}, []byte{0x02}},
		{"all 0xFF has no successor", []byte{0xFF, 0xFF, 0xFF}, nil},
		{"single 0xFF has no successor", []byte{0xFF}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PrefixSuccessor(c.prefix)
			if !bytes.Equal(got, c.want) {
				t.Fatalf("PrefixSuccessor(%v) = %v, want %v", c.prefix, got, c.want)
			}
		})
	}
}

// TestPrefixSuccessorDoesNotMutateInput guards the copy in PrefixSuccessor:
// callers commonly pass a slice they still hold onto (e.g. a stored start
// key), and PrefixSuccessor incrementing it in place would corrupt it.
func TestPrefixSuccessorDoesNotMutateInput(t *testing.T) {
	prefix := []byte{0x01, 0x02}
	orig := append([]byte(nil), prefix...)
	_ = PrefixSuccessor(prefix)
	if !bytes.Equal(prefix, orig) {
		t.Fatalf("PrefixSuccessor mutated its input: got %v, want %v", prefix, orig)
	}
}

// TestPrefixRange checks the actual contract callers rely on: every key
// built by extending the given segments with more segments falls inside
// [start, end), and a key that diverges from the prefix does not.
func TestPrefixRange(t *testing.T) {
	start, end := PrefixRange([]byte("acct1"), []byte("Email"))

	withinPrefix := [][]byte{
		Key([]byte("acct1"), []byte("Email")),
		Key([]byte("acct1"), []byte("Email"), []byte("rec1")),
		Key([]byte("acct1"), []byte("Email"), []byte{0xFF, 0xFF}),
	}
	for _, k := range withinPrefix {
		if bytes.Compare(k, start) < 0 || bytes.Compare(k, end) >= 0 {
			t.Fatalf("key %v not within [%v, %v)", k, start, end)
		}
	}

	outsidePrefix := [][]byte{
		Key([]byte("acct1"), []byte("Ea")),
		Key([]byte("acct1"), []byte("Emailz")),
		Key([]byte("acct2"), []byte("Email")),
	}
	for _, k := range outsidePrefix {
		if bytes.Compare(k, start) >= 0 && bytes.Compare(k, end) < 0 {
			t.Fatalf("key %v unexpectedly within [%v, %v)", k, start, end)
		}
	}
}

// TestPrefixRangeUnboundedWhenNoSegments checks the degenerate case where
// the encoded start key is empty (no segments, the whole keyspace): its
// successor does not exist, and the range must still be a valid half-open
// interval - end=nil meaning "no upper bound" rather than an empty range.
func TestPrefixRangeUnboundedWhenNoSegments(t *testing.T) {
	start, end := PrefixRange()
	if len(start) != 0 {
		t.Fatalf("start = %v, want empty", start)
	}
	if end != nil {
		t.Fatalf("end = %v, want nil (unbounded)", end)
	}
}
