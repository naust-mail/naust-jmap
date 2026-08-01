package jmap

import (
	"fmt"
	"strings"
	"testing"
)

// The validator's cost tracks token count, not byte count, so each
// shape below stresses a different token density at the same size:
// one big string is the cheapest input per byte, an object of many
// small members the most expensive. escapedNames is manyMembers with
// every name spelled through a \u escape, forcing the decode-and-copy
// name path that literal ASCII names skip.
func ijsonShapes(size int) []struct {
	name    string
	payload []byte
} {
	var b strings.Builder

	oneString := fmt.Sprintf(`{"filler":%q}`, strings.Repeat("a", size-14))

	b.Reset()
	b.WriteByte('{')
	for i := 0; b.Len() < size-16; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%07d":%d`, i, i%10)
	}
	b.WriteByte('}')
	members := b.String()

	b.Reset()
	b.WriteByte('{')
	for i := 0; b.Len() < size-28; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"\u006b%07d":%d`, i, i%10)
	}
	b.WriteByte('}')
	escaped := b.String()

	b.Reset()
	b.WriteByte('[')
	for i := 0; b.Len() < size-12; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d.%d", i%1000, i%97)
	}
	b.WriteByte(']')
	numbers := b.String()

	b.Reset()
	b.WriteString(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[`)
	for i := 0; b.Len() < size-64; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `["Core/echo",{"a":%d,"b":"x","c":true},"c%d"]`, i, i)
	}
	b.WriteString(`]}`)
	calls := b.String()

	return []struct {
		name    string
		payload []byte
	}{
		{"oneString", []byte(oneString)},
		{"manyMembers", []byte(members)},
		{"escapedNames", []byte(escaped)},
		{"numbers", []byte(numbers)},
		{"methodCalls", []byte(calls)},
	}
}

func benchCheckIJSON(b *testing.B, check func([]byte) error) {
	for _, sz := range []struct {
		name string
		size int
	}{{"64KB", 64 << 10}, {"1MB", 1 << 20}} {
		for _, s := range ijsonShapes(sz.size) {
			b.Run(sz.name+"/"+s.name, func(b *testing.B) {
				b.SetBytes(int64(len(s.payload)))
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if err := check(s.payload); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkCheckIJSONShapes(b *testing.B) { benchCheckIJSON(b, CheckIJSON) }

// The replaced streaming-decoder implementation over the same shapes,
// so before and after run interleaved in one binary.
func BenchmarkCheckIJSONShapesReference(b *testing.B) { benchCheckIJSON(b, checkIJSONReference) }

// Maximum accepted nesting: the recursion itself is the cost, every
// other shape stays shallow.
func benchCheckIJSONDeep(b *testing.B, check func([]byte) error) {
	payload := []byte(strings.Repeat("[", maxNestingDepth) + "1" + strings.Repeat("]", maxNestingDepth))
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := check(payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCheckIJSONDeep(b *testing.B)          { benchCheckIJSONDeep(b, CheckIJSON) }
func BenchmarkCheckIJSONDeepReference(b *testing.B) { benchCheckIJSONDeep(b, checkIJSONReference) }
