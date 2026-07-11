package jmap

import (
	"strings"
	"testing"
)

func TestIdValid(t *testing.T) {
	valid := []Id{"a", "A-b_9", Id(strings.Repeat("x", 255)), "0", "NIL"}
	for _, id := range valid {
		if !id.Valid() {
			t.Errorf("Valid(%q) = false, want true", id)
		}
	}
	invalid := []Id{"", Id(strings.Repeat("x", 256)), "a b", "a+b", "a/b", "a=", "ø"}
	for _, id := range invalid {
		if id.Valid() {
			t.Errorf("Valid(%q) = true, want false", id)
		}
	}
}

func TestNewIdDefensiveAllocation(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := NewId()
		if !id.Valid() {
			t.Fatalf("NewId produced invalid id %q", id)
		}
		c := id[0]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			t.Fatalf("NewId first char %q is not a letter", c)
		}
	}
}

func TestIntRanges(t *testing.T) {
	if !ValidInt(MaxInt) || !ValidInt(MinInt) || ValidInt(MaxInt+1) || ValidInt(MinInt-1) {
		t.Error("Int range check wrong")
	}
	if !ValidUnsignedInt(0) || ValidUnsignedInt(-1) || ValidUnsignedInt(MaxInt+1) {
		t.Error("UnsignedInt range check wrong")
	}
}

func TestValidDate(t *testing.T) {
	valid := []string{"2014-10-30T14:12:00+08:00", "2014-10-30T06:12:00Z", "2014-10-30T06:12:00.5Z"}
	for _, s := range valid {
		if !ValidDate(s) {
			t.Errorf("ValidDate(%q) = false", s)
		}
	}
	invalid := []string{
		"2014-10-30t06:12:00Z",   // lowercase t
		"2014-10-30T06:12:00z",   // lowercase z
		"2014-10-30T06:12:00.0Z", // zero secfrac must be omitted
		"2014-10-30",
		"",
	}
	for _, s := range invalid {
		if ValidDate(s) {
			t.Errorf("ValidDate(%q) = true", s)
		}
	}
	if !ValidUTCDate("2014-10-30T06:12:00Z") || ValidUTCDate("2014-10-30T14:12:00+08:00") {
		t.Error("UTCDate check wrong")
	}
}
