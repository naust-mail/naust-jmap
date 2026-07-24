package objectdb

// Tests for the index value encoding (indexValue): bytes.Compare on
// encoded values must match the type's comparison rules (RFC 8620
// section 5.5) across the full range a value can legally take.

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
)

// TestDateIndexEncodingOrder proves the Date encoding sorts
// chronologically over the entire RFC 3339 year range (0000-9999),
// including dates before the Unix epoch and fractional seconds. An
// encoding that overflowed at the extremes would file legal dates in
// the wrong position without any error.
func TestDateIndexEncodingOrder(t *testing.T) {
	p := descriptor.Property{Kind: descriptor.KindDate}
	ordered := []string{
		"0000-01-01T00:00:00Z",
		"1500-06-15T12:00:00Z",
		"1969-12-31T23:59:59.999999Z",
		"1970-01-01T00:00:00Z",
		"1970-01-01T00:00:00.000001Z",
		"2026-07-17T10:00:00Z",
		"2026-07-17T10:00:00.5Z",
		"2262-04-11T23:47:17Z", // above the int64 nanosecond range
		"9999-12-31T23:59:59Z",
	}
	encoded := make([][]byte, len(ordered))
	for i, s := range ordered {
		raw, _ := json.Marshal(s)
		v, err := indexValue(p, raw)
		if err != nil {
			t.Fatalf("indexValue(%q): %v", s, err)
		}
		if len(v) != 8 {
			t.Fatalf("indexValue(%q) = %d bytes, want fixed 8", s, len(v))
		}
		encoded[i] = v
	}
	for i := 1; i < len(encoded); i++ {
		if bytes.Compare(encoded[i-1], encoded[i]) >= 0 {
			t.Errorf("%q does not sort before %q", ordered[i-1], ordered[i])
		}
	}
}

// TestDateIndexEncodingTies proves dates equal at microsecond
// resolution encode identically: the sub-microsecond digits are
// dropped, and ordering between such records falls to the id segment
// that follows the value in every index key.
func TestDateIndexEncodingTies(t *testing.T) {
	p := descriptor.Property{Kind: descriptor.KindDate}
	a, _ := json.Marshal("2026-07-17T10:00:00.000000100Z")
	b, _ := json.Marshal("2026-07-17T10:00:00.000000900Z")
	va, err := indexValue(p, a)
	if err != nil {
		t.Fatal(err)
	}
	vb, err := indexValue(p, b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(va, vb) {
		t.Errorf("sub-microsecond dates encode differently: %x vs %x", va, vb)
	}
}
