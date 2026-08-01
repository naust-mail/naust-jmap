package jmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

// checkIJSONReference is the streaming-decoder implementation CheckIJSON
// replaced, kept verbatim as the differential oracle: the scanner-based
// walk must accept and reject exactly the same inputs. Stdlib cannot
// play that role here - json.Valid accepts duplicate member names and
// has no depth cap, which are the two properties under test.
func checkIJSONReference(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("body is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := refCheckValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing content after JSON value")
	}
	return nil
}

func refCheckValue(dec *json.Decoder, depth int) error {
	if depth > maxNestingDepth {
		return fmt.Errorf("JSON nesting exceeds %d levels", maxNestingDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key := keyTok.(string)
			if _, dup := seen[key]; dup {
				return fmt.Errorf("duplicate object member %q", key)
			}
			seen[key] = struct{}{}
			if err := refCheckValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token() // consume '}'
		return err
	case '[':
		for dec.More() {
			if err := refCheckValue(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token() // consume ']'
		return err
	}
	return nil
}

// The awkward corners where the two implementations could plausibly
// disagree: escape spellings of one name, duplicates past the linear
// window, the exact depth boundary, and every reject class.
func TestCheckIJSONVsReference(t *testing.T) {
	wide := func(n int, dup bool) string {
		var b strings.Builder
		b.WriteByte('{')
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"k%d":%d`, i, i)
		}
		if dup {
			b.WriteString(`,"k0":9`)
		}
		b.WriteByte('}')
		return b.String()
	}
	cases := []string{
		`{}`, `[]`, `""`, `0`, `-1.5e10`, `true`, `false`, `null`,
		`{"a":1,"b":2}`,
		`{"a":1,"a":2}`,
		`{"a":1,"\u0061":2}`,                 // escaped duplicate of a literal name
		`{"\u0041":1,"A":2}`,                 // literal duplicate of an escaped name
		`{"\ud83d\ude00":1,"\u1f600":2}`,     // distinct decoded names, both escaped
		"{\"caf\u00e9\":1,\"caf\\u00e9\":2}", // non-ASCII literal vs escape
		`{"a\u0000b":1}`,                     // escaped NUL in a name
		wide(31, false), wide(31, true),
		wide(32, false), wide(32, true),
		wide(33, false), wide(33, true),
		wide(200, false), wide(200, true),
		wide(96, false), wide(97, true),
		wide(1000, false), wide(1000, true),
		wide(5000, false), wide(5000, true),
		strings.Repeat("[", 1024) + "1" + strings.Repeat("]", 1024),
		strings.Repeat("[", 1025) + "1" + strings.Repeat("]", 1025),
		strings.Repeat("[", 1025) + strings.Repeat("]", 1025),
		strings.Repeat("[", 1026) + strings.Repeat("]", 1026),
		strings.Repeat(`{"a":`, 1024) + "1" + strings.Repeat("}", 1024),
		strings.Repeat(`{"a":`, 1025) + "1" + strings.Repeat("}", 1025),
		`{} `, ` {}`, "\t{}\n", `{} x`, `{}{}`, `[1] 2`,
		`{"a":}`, `{"a" 1}`, `{123:1}`, `[1,]`, `{,}`, `[`, `{`, `"`,
		`01`, `1.`, `1e`, `+1`, `nul`, `tru`, `falsey`,
		"", " ", "\xff", "{\"\xff\":1}", "\"\xed\xa0\x80\"",
		`{"":1,"":2}`, `{"":1}`,
		"[\"\\ud800\",1]", `{"\ud800":1,"\udc00":2}`, // lone surrogates in names
	}
	for _, c := range cases {
		got := CheckIJSON([]byte(c))
		want := checkIJSONReference([]byte(c))
		if (got == nil) != (want == nil) {
			t.Errorf("verdicts differ on %.60q: scanner=%v reference=%v", c, got, want)
		}
	}
}

// FuzzCheckIJSONVsReference holds the scanner-based CheckIJSON to the
// replaced implementation's verdicts on arbitrary bytes: any input one
// accepts and the other rejects is a bug.
func FuzzCheckIJSONVsReference(f *testing.F) {
	f.Add([]byte(`{"a":1,"a":2}`))
	f.Add([]byte(`{"a":1,"\u0061":2}`))
	f.Add([]byte(`[{"k":[1,"two",null,true]},-1.5e-3]`))
	f.Add([]byte(`{"a":{"b":{"a":1}}}`))
	f.Add([]byte(`{} `))
	f.Fuzz(func(t *testing.T, data []byte) {
		got := CheckIJSON(data)
		want := checkIJSONReference(data)
		if (got == nil) != (want == nil) {
			t.Fatalf("verdicts differ on %q: scanner=%v reference=%v", data, got, want)
		}
	})
}
