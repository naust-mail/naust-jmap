package jmap

import (
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
