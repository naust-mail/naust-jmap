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
