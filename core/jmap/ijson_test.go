package jmap

import (
	"fmt"
	"strings"
	"testing"
)

func TestCheckIJSON(t *testing.T) {
	valid := []string{
		`{}`,
		`{"a": 1, "b": {"a": 1}}`, // same key at different levels is fine
		`[1, "two", null, {"x": [true]}]`,
		`{"a": [{"k": 1}, {"k": 2}]}`, // same key in sibling objects is fine
	}
	for _, s := range valid {
		if err := CheckIJSON([]byte(s)); err != nil {
			t.Errorf("CheckIJSON(%s) = %v, want nil", s, err)
		}
	}
	invalid := []string{
		`{"a": 1, "a": 2}`,
		`{"outer": {"a": 1, "a": 2}}`,
		`{"a": [{"k": 1, "k": 2}]}`,
		`{} trailing`,
		`{"a": }`,
		"{\"a\": \"\xff\"}", // invalid UTF-8
		``,
	}
	for _, s := range invalid {
		if err := CheckIJSON([]byte(s)); err == nil {
			t.Errorf("CheckIJSON(%q) = nil, want error", s)
		}
	}
}

// TestCheckIJSONAllocBounds pins the allocation profile of the accept
// path so it cannot silently regress: a body with no objects completes
// with zero allocations, and an object's cost is the growth of one
// shared name stack (doubling appends), never an allocation per member.
// A per-member regression on the 32-member case would read as 32+.
func TestCheckIJSONAllocBounds(t *testing.T) {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < 32; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%d":%d`, i, i)
	}
	b.WriteByte('}')
	cases := []struct {
		name string
		body []byte
		max  float64
	}{
		{"noObjects", []byte(`[1,-2.5e3,"x",true,null,["y"]]`), 0},
		{"members32", []byte(b.String()), 8},
	}
	for _, c := range cases {
		got := testing.AllocsPerRun(200, func() {
			if err := CheckIJSON(c.body); err != nil {
				t.Fatal(err)
			}
		})
		if got > c.max {
			t.Errorf("%s: %v allocs per run, want <= %v", c.name, got, c.max)
		}
	}
}

// TestCheckIJSONDepthLimit guards the nesting cap: the streaming json.Decoder
// enforces no depth limit of its own, so a deeply nested body must be rejected
// by checkValue's own guard rather than recursing until the goroutine stack
// overflows (a fatal, unrecoverable crash). The deep inputs here are far past
// the ~2.5M-level stack-overflow point, so a missing guard would crash this
// test instead of failing it.
func TestCheckIJSONDepthLimit(t *testing.T) {
	// Ordinary nesting is accepted.
	if err := CheckIJSON([]byte(strings.Repeat("[", 100) + strings.Repeat("]", 100))); err != nil {
		t.Errorf("depth 100 array: %v", err)
	}
	if err := CheckIJSON([]byte(strings.Repeat(`{"a":`, 100) + "1" + strings.Repeat("}", 100))); err != nil {
		t.Errorf("depth 100 object: %v", err)
	}
	// Stack-exhausting nesting is rejected, not crashed.
	deepArr := strings.Repeat("[", 5_000_000) + strings.Repeat("]", 5_000_000)
	if err := CheckIJSON([]byte(deepArr)); err == nil {
		t.Error("deeply nested array accepted, want rejection")
	}
	deepObj := strings.Repeat(`{"a":`, 5_000_000) + "1" + strings.Repeat("}", 5_000_000)
	if err := CheckIJSON([]byte(deepObj)); err == nil {
		t.Error("deeply nested object accepted, want rejection")
	}
}
