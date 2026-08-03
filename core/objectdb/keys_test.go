package objectdb

// Tests for the index value encoding (indexValue): bytes.Compare on
// encoded values must match the type's comparison rules (RFC 8620
// section 5.5) across the full range a value can legally take.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

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

// TestUTCDateMicrosAgreesWithTimeParse pins the in-place reader for the
// canonical UTC form to the fallback it exists to skip: whenever it
// accepts a value, time.Parse must accept it too and agree to the
// microsecond. Refusing is always allowed - the caller falls back - so
// only acceptance is checked, over shapes that include leap days, days
// past the end of a month (which time.Date would silently roll forward
// where time.Parse refuses them), a leap second, offsets, fractional
// seconds, and malformed input.
func TestUTCDateMicrosAgreesWithTimeParse(t *testing.T) {
	var values []string
	for _, year := range []int{1, 1969, 1970, 2000, 2024, 2026, 9999} {
		for _, md := range [][2]int{{1, 1}, {2, 28}, {2, 29}, {4, 30}, {4, 31}, {6, 0}, {12, 31}, {13, 1}, {0, 5}} {
			values = append(values,
				fmt.Sprintf("%04d-%02d-%02dT00:00:00Z", year, md[0], md[1]),
				fmt.Sprintf("%04d-%02d-%02dT23:59:60Z", year, md[0], md[1]))
		}
	}
	values = append(values,
		"2026-08-04T12:30:00+01:00",
		"2026-08-04T12:30:00.5Z",
		"2026-08-04t12:30:00Z",
		"2026-08-04T12:30:00",
		"not a date",
		"")
	accepted := 0
	for _, v := range values {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := utcDateMicros(raw)
		if !ok {
			continue
		}
		accepted++
		want, err := time.Parse(time.RFC3339, v)
		if err != nil {
			t.Fatalf("%s: read in place but rejected by time.Parse", v)
		}
		if got != want.UnixMicro() {
			t.Fatalf("%s: read %d microseconds, time.Parse gives %d", v, got, want.UnixMicro())
		}
	}
	if accepted == 0 {
		t.Fatal("no value took the in-place path, so nothing was compared")
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
