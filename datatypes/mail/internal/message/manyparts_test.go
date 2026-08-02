package message

// Cost guards for the sibling-breadth worst case. maxParts bounds how
// many parts one message may yield, so the interesting number is the
// cost AT that bound: content sinks run per leaf, and any per-leaf
// rental of a fixed-size resource (a copy buffer) multiplies by the
// full cap. The walk therefore shares one copy buffer across its
// leaves (walkState.copyBuf); TestManyPartsAllocCeiling pins that
// behavior with an allocation ceiling - per-leaf buffer rental
// allocates one 32KB buffer per part and trips it regardless of
// machine speed.

import (
	"runtime"
	"strings"
	"testing"
)

// manyPartsBomb is the at-cap hostile shape: maxParts minimal sibling
// text parts under one multipart, each with one line of content.
func manyPartsBomb() string {
	var b strings.Builder
	b.WriteString("MIME-Version: 1.0\r\nFrom: a@example.com\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 0; i < maxParts; i++ {
		b.WriteString("--B\r\nContent-Type: text/plain\r\n\r\nx\r\n")
	}
	b.WriteString("--B--\r\n")
	return b.String()
}

func BenchmarkManyPartsBomb(b *testing.B) {
	raw := manyPartsBomb()
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		msg, err := Parse(strings.NewReader(raw), digestFactory)
		if err != nil {
			b.Fatal(err)
		}
		if len(msg.Root.SubParts) < maxParts-1 {
			b.Fatalf("parts = %d", len(msg.Root.SubParts))
		}
	}
}

// TestManyPartsAllocCeiling pins the shared-copy-buffer behavior in
// bytes, not allocation count: a per-leaf buffer rental adds only one
// allocation per part but 32KB each - ~67MB across the at-cap
// message, against ~2MB for the shared buffer. The ceiling sits well
// above the latter and well below the former, so it is stable across
// Go versions but trips on any per-leaf rental creeping back in.
func TestManyPartsAllocCeiling(t *testing.T) {
	raw := manyPartsBomb()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := Parse(strings.NewReader(raw), digestFactory); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	total := after.TotalAlloc - before.TotalAlloc
	if total > 16<<20 {
		t.Fatalf("parse of the at-cap message allocated %d MB; the shared-buffer walk needs ~2MB - a per-leaf copy buffer has crept back in", total>>20)
	}
}
