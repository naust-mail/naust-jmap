package jsonscan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// encoding/json is the executable specification for every helper: each
// check below compares a helper's verdict and result against the
// stdlib's on the same bytes. One divergence is deliberate and encoded
// in the oracles: json.Unmarshal treats a literal null as a no-op
// success for any Go target, while these helpers report false / make no
// calls, because callers guard null explicitly.

func isNullLit(raw []byte) bool {
	return string(bytes.TrimFunc(raw, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})) == "null"
}

func checkOracles(t *testing.T, raw []byte) {
	t.Helper()
	null := isNullLit(raw)

	var s string
	sErr := json.Unmarshal(raw, &s)
	got, ok := String(raw)
	if want := sErr == nil && !null; ok != want {
		t.Errorf("String(%q) ok = %v, stdlib %v", raw, ok, want)
	} else if ok && got != s {
		t.Errorf("String(%q) = %q, stdlib %q", raw, got, s)
	}
	if v := ValidString(raw); v != (sErr == nil && !null) {
		t.Errorf("ValidString(%q) = %v, stdlib %v", raw, v, sErr == nil && !null)
	}

	var n int64
	nErr := json.Unmarshal(raw, &n)
	gotN, ok := Int(raw)
	if want := nErr == nil && !null; ok != want {
		t.Errorf("Int(%q) ok = %v, stdlib %v", raw, ok, want)
	} else if ok && gotN != n {
		t.Errorf("Int(%q) = %d, stdlib %d", raw, gotN, n)
	}

	var b bool
	bErr := json.Unmarshal(raw, &b)
	gotB, ok := Bool(raw)
	if want := bErr == nil && !null; ok != want {
		t.Errorf("Bool(%q) ok = %v, stdlib %v", raw, ok, want)
	} else if ok && gotB != b {
		t.Errorf("Bool(%q) = %v, stdlib %v", raw, gotB, b)
	}

	var m map[string]json.RawMessage
	mErr := json.Unmarshal(raw, &m)
	if got, want := ValidObject(raw), mErr == nil && !null; got != want {
		t.Errorf("ValidObject(%q) = %v, stdlib %v", raw, got, want)
	}

	var a []json.RawMessage
	aErr := json.Unmarshal(raw, &a)
	if got, want := ValidArray(raw), aErr == nil && !null; got != want {
		t.Errorf("ValidArray(%q) = %v, stdlib %v", raw, got, want)
	}

	// EachKey mirrors decoding into a map (null included: no keys, no
	// error), except keys arrive with duplicates, so compare as sets.
	keys := map[string]bool{}
	kErr := EachKey(raw, func(k string) { keys[k] = true })
	if (kErr == nil) != (mErr == nil) {
		t.Errorf("EachKey(%q) err = %v, stdlib %v", raw, kErr, mErr)
	} else if kErr == nil {
		if len(keys) != len(m) {
			t.Errorf("EachKey(%q) keys %v, stdlib %v", raw, keys, m)
		}
		for k := range m {
			if !keys[k] {
				t.Errorf("EachKey(%q) missing key %q", raw, k)
			}
		}
	}

	// HasKey mirrors map membership under EachKey's acceptance; probes
	// cover hits, misses, names only reachable by unescaping, and names
	// that exist only inside nested values (which must never match).
	for _, probe := range []string{"a", "b", "c", "id", "$seen", "café", "absent"} {
		gotH, hErr := HasKey(raw, probe)
		if (hErr == nil) != (mErr == nil) {
			t.Errorf("HasKey(%q, %q) err = %v, stdlib %v", raw, probe, hErr, mErr)
		} else if hErr == nil {
			_, want := m[probe]
			if gotH != want {
				t.Errorf("HasKey(%q, %q) = %v, stdlib %v", raw, probe, gotH, want)
			}
		}
	}

	// EachString mirrors decoding into []string, in order.
	var ss []string
	ssErr := json.Unmarshal(raw, &ss)
	var elems []string
	eErr := EachString(raw, func(e string) { elems = append(elems, e) })
	if (eErr == nil) != (ssErr == nil) {
		t.Errorf("EachString(%q) err = %v, stdlib %v", raw, eErr, ssErr)
	} else if eErr == nil {
		if len(elems) != len(ss) {
			t.Errorf("EachString(%q) = %v, stdlib %v", raw, elems, ss)
		} else {
			for i := range ss {
				if elems[i] != ss[i] {
					t.Errorf("EachString(%q)[%d] = %q, stdlib %q", raw, i, elems[i], ss[i])
				}
			}
		}
	}
}

// internDict is a fixed vocabulary for the interning checks; "a" and
// "b" appear throughout the case list so both hit and miss paths run.
var internDict = map[string]string{"a": "a", "b": "b", "id": "id"}

// checkInterning verifies DecodeObject with a dictionary is
// byte-for-byte equivalent to DecodeObject without one, and that a name
// found in the dictionary comes back as the dictionary's own string.
func checkInterning(t *testing.T, raw []byte) {
	t.Helper()
	plain, plainErr := DecodeObject(raw, nil)
	interned, internedErr := DecodeObject(raw, internDict)
	if (plainErr == nil) != (internedErr == nil) {
		t.Fatalf("DecodeObject(%q) err = %v with dict, %v without", raw, internedErr, plainErr)
	}
	if plainErr != nil {
		return
	}
	if len(plain) != len(interned) || (plain == nil) != (interned == nil) {
		t.Fatalf("DecodeObject(%q) dict/no-dict shape mismatch", raw)
	}
	for k, v := range plain {
		got, ok := interned[k]
		if !ok || string(got) != string(v) {
			t.Fatalf("DecodeObject(%q) dict/no-dict value mismatch at %q", raw, k)
		}
	}
}

// subsetWants covers a nil set (validate-only), hits, misses, and
// names only reachable by unescaping.
var subsetWants = []map[string]bool{
	nil,
	{"a": true, "café": true, "$seen": true},
	{"a": true, "b": true, "c": true, "id": true, "x": true},
}

// checkSubset verifies DecodeObjectSubset against its executable
// specification: DecodeObject followed by filtering to the wanted
// names, with identical acceptance.
func checkSubset(t *testing.T, raw []byte) {
	t.Helper()
	full, fullErr := DecodeObject(raw, internDict)
	for _, wanted := range subsetWants {
		got, err := DecodeObjectSubset(raw, internDict, wanted)
		if (err == nil) != (fullErr == nil) {
			t.Fatalf("DecodeObjectSubset(%q, %v) err = %v, DecodeObject %v", raw, wanted, err, fullErr)
		}
		if err != nil {
			continue
		}
		if (got == nil) != (full == nil) {
			t.Fatalf("DecodeObjectSubset(%q, %v) nil-ness mismatch", raw, wanted)
		}
		want := 0
		for k, v := range full {
			if !wanted[k] {
				continue
			}
			want++
			if g, ok := got[k]; !ok || !bytes.Equal(g, v) {
				t.Fatalf("DecodeObjectSubset(%q, %v)[%q] = %q, DecodeObject %q", raw, wanted, k, g, v)
			}
		}
		if len(got) != want {
			t.Fatalf("DecodeObjectSubset(%q, %v) has %d members, want %d", raw, wanted, len(got), want)
		}
	}
}

func TestHelpersAgainstStdlib(t *testing.T) {
	cases := []string{
		// Structure and whitespace.
		``, ` `, `null`, ` null `, `nul`, `nulll`, `x`,
		`{}`, ` { } `, `[]`, ` [ ] `, `{`, `[`, `}`, `]`, `{}}`, `[][]`,
		"\xef\xbb\xbf{}", "\x00{}",
		// Strings.
		`""`, `"a"`, ` "a" `, `"a" x`, `"a`, `"\n"`, `"\q"`, `"A"`,
		"\"\x00\"", "\"\U0001F600\"", `"\ud800"`, `"\ude00\ud800"`,
		"\"\x80\"", "\"tab\tend\"", `"a\\"`, `"\/"`,
		// Numbers.
		`0`, `-0`, `1`, `-1`, ` 42 `, `01`, `1.`, `.5`, `1.5`, `-1.5`,
		`1e2`, `1E+2`, `1e`, `--1`, `+1`, `NaN`, `Infinity`, `0x10`,
		`9223372036854775807`, `9223372036854775808`,
		`-9223372036854775808`, `-9223372036854775809`,
		`184467440737095516151844674407370955161518446744073709551615`,
		`1 2`,
		// Booleans.
		`true`, `false`, ` true `, `tru`, `truee`, `TRUE`, `false false`,
		// Objects.
		`{"a":1}`, `{"a":1,"b":2}`, `{"a":1,"a":2}`, `{"a":}`, `{"a"1}`,
		`{"a":1,}`, `{a:1}`, `{"a":{"b":[1,{"c":null}]}}`, `{"a":1}}`,
		`{"\ud800":1}`, `{"a":"\q"}`, `{"a":01}`,
		`{"$seen":true}`, `{"\u0024seen":true}`,
		`{"café":1}`, `{"caf\u00e9":1}`,
		`{"absent":{"a":1}}`, `{"x":1,"a":2,"x":3}`,
		// Arrays.
		`["a"]`, `["a","b"]`, `["a",]`, `[,"a"]`, `["a" "b"]`,
		`["a",null]`, `["a",1]`, `[null]`, `[[]]`, "[\"\U0001F600\"]",
		`[1,2,3]`,
	}
	for _, c := range cases {
		checkOracles(t, []byte(c))
		checkInterning(t, []byte(c))
		checkSubset(t, []byte(c))
	}
	// Depth: the stdlib decides both sides of its own nesting limit.
	checkOracles(t, []byte(strings.Repeat("[", MaxDepth)+strings.Repeat("]", MaxDepth)))
	checkOracles(t, []byte(strings.Repeat("[", MaxDepth+1)+strings.Repeat("]", MaxDepth+1)))
}

func FuzzHelpersVsStdlib(f *testing.F) {
	for _, s := range []string{
		`{"a":1,"a":2}`, `["a","\ud800"]`, `9223372036854775808`,
		` "x" `, `truex`, "{\"\x00 \\ud800\":null}", `[null,"a"]`, `-0`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		checkOracles(t, raw)
		checkInterning(t, raw)
		checkSubset(t, raw)
	})
}

// TestCheckIJSONWideObjects exercises the spilled duplicate table at
// volume: every table size the growth path produces, the duplicate in
// the worst position (last, after the whole table is built), escaped
// spellings colliding across the spill boundary, and a distinct-names
// control at each width. At these widths and load factors hash
// collisions and probe clusters are statistically guaranteed, so the
// probe and resize paths all execute.
func TestCheckIJSONWideObjects(t *testing.T) {
	build := func(n int, dupAt int, esc bool) []byte {
		var b []byte
		b = append(b, '{')
		for i := 0; i < n; i++ {
			if i > 0 {
				b = append(b, ',')
			}
			if i == dupAt {
				if esc {
					// An escaped respelling of member k0.
					b = append(b, `"\u006b0":9`...)
				} else {
					b = append(b, `"k0":9`...)
				}
				continue
			}
			b = append(b, fmt.Sprintf(`"k%d":%d`, i, i)...)
		}
		b = append(b, '}')
		return b
	}
	for _, n := range []int{33, 95, 96, 97, 200, 1000, 5000} {
		if err := CheckIJSON(build(n, -1, false), 1024); err != nil {
			t.Errorf("distinct %d members rejected: %v", n, err)
		}
		if err := CheckIJSON(build(n, n-1, false), 1024); err == nil {
			t.Errorf("duplicate at end of %d members accepted", n)
		}
		if err := CheckIJSON(build(n, n-1, true), 1024); err == nil {
			t.Errorf("escaped duplicate at end of %d members accepted", n)
		}
	}
}
