package fsstore

import (
	"time"

	"github.com/naust-mail/naust-jmap/core/providers/blob"
)

// AgeThrottle backdates w's flush throttle, as a trickle that has held
// buffered bytes that long would see it, so a test can prove the interval
// flush keeps a slow upload's liveness fresh without waiting out the real
// interval.
func AgeThrottle(w blob.BlobWriter, d time.Duration) {
	w.(*writer).lastFlush = time.Now().Add(-d)
}

// CloseUnderlyingFile closes w's temporary file out from under it, so a test
// can force the next Write or flush to fail exactly like a disk-level I/O
// error would, without needing a real fault-injecting filesystem.
func CloseUnderlyingFile(w blob.BlobWriter) error {
	return w.(*writer).f.Close()
}

// SyncDir exposes the package-private syncDir for a direct unit test: it is
// a small enough helper that driving it through Commit's happy path never
// reaches its own error branch.
func SyncDir(dir string) error {
	return syncDir(dir)
}
