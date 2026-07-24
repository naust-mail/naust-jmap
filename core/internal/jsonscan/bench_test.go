package jsonscan

import (
	"encoding/json"
	"testing"
)

// Benchmarks against the stdlib on the shapes the hot paths actually
// see: a short string value (ids, dates), a small member map (an
// Email's mailboxIds or keywords), and a nested object (a stored
// aggregate).

var (
	benchStr = []byte(`"N06FS5DDKN7BQF30A68SSVB1MH8"`)
	benchMap = []byte(`{"N06inbox0000000000000000001":true,"N06archive000000000000000001":true,"$seen":true}`)
	benchObj = []byte(`{"total":42,"unread":7,"mailboxes":{"a":{"total":42,"unread":7,"onlyUnread":false},"b":{"total":1,"unread":0,"onlyUnread":false}}}`)
)

func BenchmarkStringStdlib(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var s string
		if err := json.Unmarshal(benchStr, &s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStringScan(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if _, ok := String(benchStr); !ok {
			b.Fatal("not a string")
		}
	}
}

func BenchmarkValidStringScan(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if !ValidString(benchStr) {
			b.Fatal("not a string")
		}
	}
}

func BenchmarkIntScan(b *testing.B) {
	raw := []byte(`1721779200000`)
	for i := 0; i < b.N; i++ {
		if _, ok := Int(raw); !ok {
			b.Fatal("not an int")
		}
	}
}

func BenchmarkIntStdlib(b *testing.B) {
	raw := []byte(`1721779200000`)
	for i := 0; i < b.N; i++ {
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEachKeyStdlib(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(benchMap, &m); err != nil {
			b.Fatal(err)
		}
		n := 0
		for range m {
			n++
		}
	}
}

func BenchmarkEachKeyScan(b *testing.B) {
	for i := 0; i < b.N; i++ {
		n := 0
		if err := EachKey(benchMap, func(string) { n++ }); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidObjectStdlib(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(benchObj, &m); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidObjectScan(b *testing.B) {
	for i := 0; i < b.N; i++ {
		if !ValidObject(benchObj) {
			b.Fatal("not an object")
		}
	}
}
