package objectdb

// The record codec's contract is exact equivalence with encoding/json
// over map[string]json.RawMessage, and the backend it reads from is not
// trusted. These tests hold both ends: a table of adversarial inputs
// with explicit verdicts (each also cross-checked against the stdlib as
// the executable specification), and differential + round-trip fuzzers.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// oracleCompare runs decodeObject and json.Unmarshal on the same input
// and fails the test on any disagreement: acceptance, nil-ness, keys, or
// value bytes.
func oracleCompare(t *testing.T, input []byte) {
	t.Helper()
	got, gotErr := decodeObject(input)
	var want map[string]json.RawMessage
	wantErr := json.Unmarshal(input, &want)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("decodeObject err = %v, json.Unmarshal err = %v, input %q", gotErr, wantErr, input)
	}
	if gotErr != nil {
		return
	}
	if (got == nil) != (want == nil) {
		t.Fatalf("nil-ness mismatch: decodeObject %v, stdlib %v, input %q", got == nil, want == nil, input)
	}
	if len(got) != len(want) {
		t.Fatalf("key count mismatch: decodeObject %d, stdlib %d, input %q", len(got), len(want), input)
	}
	for k, v := range want {
		gv, ok := got[k]
		if !ok {
			t.Fatalf("missing key %q, input %q", k, input)
		}
		if !bytes.Equal(gv, v) {
			t.Fatalf("value mismatch for %q: decodeObject %q, stdlib %q", k, gv, v)
		}
	}
}

func TestDecodeObjectAdversarial(t *testing.T) {
	cases := []struct {
		name  string
		input string
		ok    bool
	}{
		// Structure.
		{"empty", "", false},
		{"whitespace only", " \t\n\r ", false},
		{"empty object", "{}", true},
		{"spaced empty object", " { } ", true},
		{"unterminated object", "{", false},
		{"bare close", "}", false},
		{"unterminated member", `{"a"`, false},
		{"missing colon", `{"a" 1}`, false},
		{"missing value", `{"a":}`, false},
		{"missing key", `{:1}`, false},
		{"non-string key", `{1:2}`, false},
		{"single-quoted key", `{'a':1}`, false},
		{"trailing comma", `{"a":1,}`, false},
		{"leading comma", `{,"a":1}`, false},
		{"missing comma", `{"a":1 "b":2}`, false},
		{"double comma", `{"a":1,,"b":2}`, false},
		{"top-level array", `["a"]`, false},
		{"top-level string", `"a"`, false},
		{"top-level number", "123", false},
		{"top-level true", "true", false},
		{"top-level null", "null", true}, // nil map, no error - stdlib semantics
		{"null with trailing", "null x", false},
		{"trailing garbage", `{"a":1}x`, false},
		{"trailing brace", `{"a":1}}`, false},
		{"second object", `{"a":1}{"b":2}`, false},
		{"trailing whitespace ok", "{\"a\":1} \t\n", true},
		{"BOM prefix", "\xEF\xBB\xBF{}", false},
		{"NUL before object", "\x00{}", false},
		// Nested containers.
		{"nested ok", `{"a":{"b":[1,{"c":[]}]}}`, true},
		{"array trailing comma", `{"a":[1,2,]}`, false},
		{"nested trailing comma", `{"a":{"b":1,}}`, false},
		{"unterminated array", `{"a":[1,2`, false},
		{"unbalanced close", `{"a":[1]]}`, false},
		{"array missing comma", `{"a":[1 2]}`, false},
		// Literals.
		{"literal soup", `{"a":true,"b":false,"c":null}`, true},
		{"truncated true", `{"a":tru}`, false},
		{"literal with trailing", `{"a":truex}`, false},
		{"uppercase literal", `{"a":TRUE}`, false},
		// Numbers.
		{"numbers ok", `{"a":0,"b":-0,"c":-1.5e+300,"d":1E-10,"e":42}`, true},
		{"huge digits", `{"a":` + strings.Repeat("9", 1000) + `}`, true},
		{"leading zero", `{"a":01}`, false},
		{"bare minus", `{"a":-}`, false},
		{"trailing dot", `{"a":1.}`, false},
		{"leading dot", `{"a":.5}`, false},
		{"plus sign", `{"a":+1}`, false},
		{"bare exponent", `{"a":1e}`, false},
		{"signed bare exponent", `{"a":1e+}`, false},
		{"hex number", `{"a":0x10}`, false},
		{"infinity", `{"a":Infinity}`, false},
		{"nan", `{"a":NaN}`, false},
		// Strings and escapes.
		{"escapes ok", `{"a":"q\" b\\ s\/ \b\f\n\r\t é"}`, true},
		{"bad escape", `{"a":"\q"}`, false},
		{"truncated unicode escape", `{"a":"\u12"}`, false},
		{"non-hex unicode escape", `{"a":"\u12G4"}`, false},
		{"unterminated string", `{"a":"xyz`, false},
		{"unterminated escape", `{"a":"\`, false},
		{"raw newline in string", "{\"a\":\"x\ny\"}", false},
		{"raw NUL in string", "{\"a\":\"x\x00y\"}", false},
		{"raw tab in string", "{\"a\":\"x\ty\"}", false},
		{"DEL byte in string ok", "{\"a\":\"x\x7fy\"}", true},
		{"escaped NUL ok", "{\"a\":\"\\u0000\"}", true},
		{"invalid UTF-8 in string ok", "{\"a\":\"x\xffy\"}", true}, // stdlib scanner passes raw bytes >= 0x20
		{"invalid UTF-8 in key ok", "{\"a\xffb\":1}", true},        // key coerced to U+FFFD, as stdlib does
		{"lone high surrogate key", `{"\ud800":1}`, true},
		{"lone low surrogate key", `{"\udc00":1}`, true},
		{"surrogate pair key", `{"\ud83d\ude00":1}`, true},
		{"reversed surrogates key", `{"\udc00\ud800":1}`, true},
		{"empty key", `{"":1}`, true},
		{"long key", `{"` + strings.Repeat("k", 4096) + `":1}`, true},
		// Member semantics.
		{"duplicate keys", `{"a":1,"a":2}`, true},
		{"escaped duplicate key", `{"A":1,"A":2}`, true},
		{"many members", func() string {
			var b strings.Builder
			b.WriteString("{")
			for i := 0; i < 500; i++ {
				fmt.Fprintf(&b, "%q:%d,", fmt.Sprintf("k%d", i), i)
			}
			b.WriteString(`"z":true}`)
			return b.String()
		}(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeObject([]byte(tc.input))
			if (err == nil) != tc.ok {
				t.Fatalf("decodeObject(%q) err = %v, want ok=%v", tc.input, err, tc.ok)
			}
			oracleCompare(t, []byte(tc.input))
		})
	}
}

// TestDecodeObjectResults pins the decoded content itself for the cases
// where the interesting part is the result, not just acceptance.
func TestDecodeObjectResults(t *testing.T) {
	obj, err := decodeObject([]byte(`{"a":1,"a":2}`))
	if err != nil || string(obj["a"]) != "2" {
		t.Fatalf("duplicate key: got %q, %v, want last value 2", obj["a"], err)
	}
	obj, err = decodeObject([]byte(`{"A":1,"A":2}`))
	if err != nil || len(obj) != 1 || string(obj["A"]) != "2" {
		t.Fatalf("escaped duplicate: got %v, %v, want one key A=2", obj, err)
	}
	obj, err = decodeObject([]byte(`{"\ud800":1}`))
	if err != nil || string(obj["�"]) != "1" {
		t.Fatalf("lone surrogate: got %v, %v, want U+FFFD key", obj, err)
	}
	obj, err = decodeObject([]byte(`{"\ud83d\ude00":1}`))
	if err != nil || string(obj["\U0001F600"]) != "1" {
		t.Fatalf("surrogate pair: got %v, %v, want combined rune key", obj, err)
	}
	// A value keeps its exact bytes, internal whitespace included.
	obj, err = decodeObject([]byte(`{"a": [ 1 , {"b" : "c"} ] }`))
	if err != nil || string(obj["a"]) != `[ 1 , {"b" : "c"} ]` {
		t.Fatalf("raw value: got %q, %v", obj["a"], err)
	}
	obj, err = decodeObject([]byte("null"))
	if err != nil || obj != nil {
		t.Fatalf("null: got %v, %v, want nil map and nil error", obj, err)
	}
}

func TestDecodeObjectDepthLimit(t *testing.T) {
	deep := func(n int) []byte {
		return []byte(`{"a":` + strings.Repeat("[", n) + "1" + strings.Repeat("]", n) + `}`)
	}
	// The record object is one container, so a value may nest
	// maxCodecDepth-1 deep. Both verdicts must agree with the stdlib.
	atLimit, overLimit := deep(maxCodecDepth-1), deep(maxCodecDepth)
	if _, err := decodeObject(atLimit); err != nil {
		t.Fatalf("depth at limit rejected: %v", err)
	}
	if _, err := decodeObject(overLimit); err == nil {
		t.Fatal("depth over limit accepted")
	}
	oracleCompare(t, atLimit)
	oracleCompare(t, overLimit)
}

// TestEncodeObjectRejectsMalformedValues is the injection defense: a
// RawMessage that is not exactly one JSON value must never be embedded
// into a stored record.
func TestEncodeObjectRejectsMalformedValues(t *testing.T) {
	bad := []string{
		``,         // empty
		` `,        // whitespace only
		`1,"x":2`,  // member splice
		`1}`,       // record close splice
		`[1,2]]`,   // unbalanced
		`{`,        // unterminated
		`1 2`,      // two values
		`01`,       // trailing digit after zero
		`"a" "b"`,  // two strings
		"\"\x01\"", // control character
		`{"a":1,}`, // trailing comma
		"nu\x00ll", // NUL inside a literal
	}
	for _, v := range bad {
		if _, err := encodeObject(Object{"p": json.RawMessage(v)}); err == nil {
			t.Errorf("encodeObject accepted malformed value %q", v)
		}
	}
	// The nesting cap counts the record object itself, so a value at the
	// codec's own depth limit is refused - every record the encoder
	// writes must be decodable.
	tooDeep := strings.Repeat("[", maxCodecDepth) + "1" + strings.Repeat("]", maxCodecDepth)
	if _, err := encodeObject(Object{"p": json.RawMessage(tooDeep)}); err == nil {
		t.Error("encodeObject accepted a value nested to the record depth limit")
	}
}

func TestEncodeObjectDeterministicRoundTrip(t *testing.T) {
	obj := Object{
		"id":                json.RawMessage(`"M1"`),
		"subject":           json.RawMessage(`"héllo \"world\""`),
		"keywords":          json.RawMessage(`{"$seen":true}`),
		"n":                 json.RawMessage(`-1.5e+300`),
		"empty":             json.RawMessage(`{}`),
		"spaced":            json.RawMessage(`[ 1 , 2 ]`),
		"weird \n\"key\"\\": json.RawMessage(`null`),
	}
	enc, err := encodeObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(enc) {
		t.Fatalf("encoded record is not valid JSON: %q", enc)
	}
	enc2, err := encodeObject(obj)
	if err != nil || !bytes.Equal(enc, enc2) {
		t.Fatalf("encoding is not deterministic: %q vs %q (%v)", enc, enc2, err)
	}
	back, err := decodeObject(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(obj) {
		t.Fatalf("round trip key count %d, want %d", len(back), len(obj))
	}
	for k, v := range obj {
		if !bytes.Equal(back[k], v) {
			t.Fatalf("round trip changed %q: %q -> %q", k, v, back[k])
		}
	}
	// The stdlib agrees about the encoded bytes.
	oracleCompare(t, enc)
}

func FuzzDecodeObjectVsEncodingJSON(f *testing.F) {
	f.Add([]byte(`{"id":"M1","keywords":{"$seen":true},"size":1024}`))
	f.Add([]byte(`{"a":[1,{"b":"😀"},null,true,-0.5e2]}`))
	f.Add([]byte(`{"a":1,"a":2}`))
	f.Add([]byte("null"))
	f.Add([]byte(`{"\ud800":"x","x":"\\"}`))
	f.Add([]byte("{\"a\xff\":\"\xfe\"}"))
	f.Add([]byte(`{"a":`))
	f.Add([]byte(`{"a":[[[[[[[[1]]]]]]]]}`))
	f.Add([]byte(`{"a":01}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, gotErr := decodeObject(data)
		var want map[string]json.RawMessage
		wantErr := json.Unmarshal(data, &want)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("acceptance mismatch: decodeObject err = %v, stdlib err = %v", gotErr, wantErr)
		}
		if gotErr != nil {
			return
		}
		if (got == nil) != (want == nil) || len(got) != len(want) {
			t.Fatalf("shape mismatch: decodeObject %v, stdlib %v", got, want)
		}
		for k, v := range want {
			if gv, ok := got[k]; !ok || !bytes.Equal(gv, v) {
				t.Fatalf("key %q: decodeObject %q, stdlib %q", k, got[k], v)
			}
		}
	})
}

func FuzzObjectCodecRoundTrip(f *testing.F) {
	f.Add([]byte(`{"id":"M1","keywords":{"$seen":true},"spaced":[ 1 , 2 ]}`))
	f.Add([]byte("{\"\\u0000 \\ud800\":null}"))
	f.Fuzz(func(t *testing.T, data []byte) {
		obj, err := decodeObject(data)
		if err != nil || obj == nil {
			return
		}
		enc, err := encodeObject(obj)
		if err != nil {
			t.Fatalf("decoded object failed to encode: %v", err)
		}
		if !json.Valid(enc) {
			t.Fatalf("encoded record is not valid JSON: %q", enc)
		}
		back, err := decodeObject(enc)
		if err != nil {
			t.Fatalf("encoded record failed to decode: %v", err)
		}
		if len(back) != len(obj) {
			t.Fatalf("round trip key count %d, want %d", len(back), len(obj))
		}
		for k, v := range obj {
			if !bytes.Equal(back[k], v) {
				t.Fatalf("round trip changed %q: %q -> %q", k, v, back[k])
			}
		}
	})
}
