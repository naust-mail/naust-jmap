// Package testsupport holds fixtures shared by more than one of this
// module's test packages. It exists only for _test.go files: nothing in the
// module's non-test build may import it (see internal/depsguard).
package testsupport

import (
	"io"
	"runtime"
	"strings"
)

// TextMessage wraps body as a single text part of the given charset, headers
// and all - the shape both the delivery pipeline and the search matcher tests
// feed a real message parse.
func TextMessage(charset string, body io.Reader) io.Reader {
	return io.MultiReader(
		strings.NewReader("Content-Type: text/plain; charset="+charset+"\r\n\r\n"),
		body,
	)
}

// Filler is size octets of plain text, produced without ever holding them: a
// large body for a bounded-memory test to stream through.
func Filler(size int) io.Reader {
	return io.LimitReader(&repeat{s: "the quick brown fox jumps over the lazy dog "}, int64(size))
}

type repeat struct {
	s   string
	off int
}

func (r *repeat) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.s[r.off:])
		r.off = (r.off + c) % len(r.s)
		n += c
	}
	return n, nil
}

// HeapWatcher passes a message through and measures the live heap as it goes.
// What must stay small is what a parse HOLDS at any moment, so the heap is
// collected and read as the octets flow past; a sink that is buffering the
// part cannot hide from it.
type HeapWatcher struct {
	r     io.Reader
	base  uint64
	peak  uint64
	reads int
}

// WatchHeap starts watching the live heap while r is read.
func WatchHeap(r io.Reader) *HeapWatcher {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return &HeapWatcher{r: r, base: ms.HeapAlloc}
}

func (w *HeapWatcher) Read(p []byte) (int, error) {
	if w.reads%8 == 0 { // sampling: a full GC per read would take all day
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > w.peak {
			w.peak = ms.HeapAlloc
		}
	}
	w.reads++
	return w.r.Read(p)
}

// Held is the live heap the parse added, at its worst moment.
func (w *HeapWatcher) Held() uint64 {
	if w.peak < w.base {
		return 0
	}
	return w.peak - w.base
}
