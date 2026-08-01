package websocket

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/private/rawjson"
)

// benchPayload builds a valid Request message of roughly size bytes,
// the bulk of it inside one method-call argument string.
func benchPayload(size int) []byte {
	const skeleton = `{"@type":"Request","id":"r1","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"filler":"%s"},"c0"]]}`
	fill := size - len(skeleton)
	if fill < 0 {
		fill = 0
	}
	return []byte(fmt.Sprintf(skeleton, strings.Repeat("a", fill)))
}

var benchSizes = []struct {
	name string
	size int
}{
	{"1KB", 1 << 10},
	{"64KB", 64 << 10},
	{"1MB", 1 << 20},
}

// The envelope peek dispatch performs before handing the payload to
// the pipeline: one scanner pass extracting the two wrapper members.
func BenchmarkDispatchEnvelopePeek(b *testing.B) {
	for _, s := range benchSizes {
		payload := benchPayload(s.size)
		b.Run(s.name, func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				members, err := rawjson.Members(payload, envelopeMembers)
				if err != nil {
					b.Fatal(err)
				}
				if _, ok := rawjson.String(members["@type"]); !ok {
					b.Fatal("payload did not decode as an envelope")
				}
			}
		})
	}
}

// The stdlib decode the peek replaced, kept for comparison.
func BenchmarkDispatchEnvelopePeekStdlib(b *testing.B) {
	for _, s := range benchSizes {
		payload := benchPayload(s.size)
		b.Run(s.name, func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var env struct {
					Type *string `json:"@type"`
					ID   *string `json:"id"`
				}
				if err := json.Unmarshal(payload, &env); err != nil || env.Type == nil {
					b.Fatal("payload did not decode as an envelope")
				}
			}
		})
	}
}

// The two passes the pipeline itself makes over the same bytes, for
// scale: what the peek costs relative to the work that follows it.
func BenchmarkDispatchCheckIJSON(b *testing.B) {
	for _, s := range benchSizes {
		payload := benchPayload(s.size)
		b.Run(s.name, func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err := jmap.CheckIJSON(payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDispatchParseRequest(b *testing.B) {
	for _, s := range benchSizes {
		payload := benchPayload(s.size)
		b.Run(s.name, func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := jmap.ParseRequest(payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
