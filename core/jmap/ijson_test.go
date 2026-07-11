package jmap

import "testing"

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
