package backend

import (
	"bytes"
	"testing"
)

func TestBatchSet(t *testing.T) {
	var b Batch
	b.Set([]byte("k"), []byte("v"))
	if len(b.Ops) != 1 {
		t.Fatalf("len(Ops) = %d, want 1", len(b.Ops))
	}
	op := b.Ops[0]
	if op.Kind != OpSet || !bytes.Equal(op.Key, []byte("k")) || !bytes.Equal(op.Value, []byte("v")) {
		t.Fatalf("op = %+v, want Kind=OpSet Key=k Value=v", op)
	}
}

func TestBatchDelete(t *testing.T) {
	var b Batch
	b.Delete([]byte("k"))
	op := b.Ops[0]
	if op.Kind != OpDelete || !bytes.Equal(op.Key, []byte("k")) {
		t.Fatalf("op = %+v, want Kind=OpDelete Key=k", op)
	}
}

func TestBatchAdd(t *testing.T) {
	var b Batch
	b.Add([]byte("k"), -5)
	op := b.Ops[0]
	if op.Kind != OpAdd || !bytes.Equal(op.Key, []byte("k")) || op.Delta != -5 {
		t.Fatalf("op = %+v, want Kind=OpAdd Key=k Delta=-5", op)
	}
}

func TestBatchAssert(t *testing.T) {
	var b Batch
	b.Assert([]byte("k"), []byte("expected"))
	op := b.Ops[0]
	if op.Kind != OpAssert || !bytes.Equal(op.Key, []byte("k")) || !bytes.Equal(op.Value, []byte("expected")) {
		t.Fatalf("op = %+v, want Kind=OpAssert Key=k Value=expected", op)
	}

	// nil expect means "key must be absent" - must be preserved, not
	// normalized to an empty non-nil slice.
	var b2 Batch
	b2.Assert([]byte("k"), nil)
	if b2.Ops[0].Value != nil {
		t.Fatalf("Assert(k, nil) stored Value = %v, want nil", b2.Ops[0].Value)
	}
}

// TestBatchAccumulatesInOrder checks that a batch built from multiple calls
// preserves op order, since WriteBatch applies them as one ordered atomic
// sequence.
func TestBatchAccumulatesInOrder(t *testing.T) {
	var b Batch
	b.Set([]byte("a"), []byte("1"))
	b.Delete([]byte("b"))
	b.Add([]byte("c"), 3)
	b.Assert([]byte("d"), []byte("2"))

	wantKinds := []OpKind{OpSet, OpDelete, OpAdd, OpAssert}
	if len(b.Ops) != len(wantKinds) {
		t.Fatalf("len(Ops) = %d, want %d", len(b.Ops), len(wantKinds))
	}
	for i, k := range wantKinds {
		if b.Ops[i].Kind != k {
			t.Fatalf("Ops[%d].Kind = %v, want %v", i, b.Ops[i].Kind, k)
		}
	}
}

func TestEncodeDecodeInt64RoundTrip(t *testing.T) {
	values := []int64{0, 1, -1, 42, -42, 1 << 62, -(1 << 62), 1<<63 - 1, -(1 << 63)}
	for _, v := range values {
		enc := EncodeInt64(v)
		if len(enc) != 8 {
			t.Fatalf("EncodeInt64(%d): len = %d, want 8", v, len(enc))
		}
		got, err := DecodeInt64(enc)
		if err != nil {
			t.Fatalf("DecodeInt64(EncodeInt64(%d)): err = %v", v, err)
		}
		if got != v {
			t.Fatalf("DecodeInt64(EncodeInt64(%d)) = %d", v, got)
		}
	}
}

// TestEncodeInt64ByteOrderMatchesNumericOrder is the property the encoding
// exists for: a backend that stores counters as raw sorted bytes (a range
// scan, a min/max index) must see byte order agree with numeric order
// across the zero crossing, which is exactly what the sign-bit offset in
// EncodeInt64 buys.
func TestEncodeInt64ByteOrderMatchesNumericOrder(t *testing.T) {
	values := []int64{-(1 << 62), -100, -1, 0, 1, 100, 1 << 62}
	for i := 0; i < len(values); i++ {
		for j := i + 1; j < len(values); j++ {
			a, b := EncodeInt64(values[i]), EncodeInt64(values[j])
			if bytes.Compare(a, b) >= 0 {
				t.Fatalf("EncodeInt64(%d) not < EncodeInt64(%d): %v vs %v", values[i], values[j], a, b)
			}
		}
	}
}

func TestDecodeInt64WrongLength(t *testing.T) {
	cases := [][]byte{nil, {}, {1, 2, 3}, {1, 2, 3, 4, 5, 6, 7, 8, 9}}
	for _, v := range cases {
		if _, err := DecodeInt64(v); err == nil {
			t.Fatalf("DecodeInt64(%v): want error, got nil", v)
		}
	}
}
