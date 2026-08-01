package runtime

import (
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/internal/authtest"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
)

// goroutineRecords snapshots every live goroutine's stack, keyed by its
// "goroutine N" header, so a test can diff what appeared across a
// create/use/close cycle.
func goroutineRecords() map[string]string {
	buf := make([]byte, 1<<20)
	n := goruntime.Stack(buf, true)
	recs := map[string]string{}
	for _, rec := range strings.Split(string(buf[:n]), "\n\n") {
		if id, _, ok := strings.Cut(rec, " ["); ok && strings.HasPrefix(id, "goroutine ") {
			recs[id] = rec
		}
	}
	return recs
}

// requireNoNewModuleGoroutines fails if a goroutine that did not exist
// in before is still running this module's code once a short grace
// period runs out (a goroutine's exit is asynchronous with respect to
// the close that triggered it).
func requireNoNewModuleGoroutines(t *testing.T, before map[string]string) {
	t.Helper()
	const pkg = "naust-jmap/core/runtime"
	deadline := time.Now().Add(2 * time.Second)
	for {
		var leaked []string
		for id, rec := range goroutineRecords() {
			if _, ok := before[id]; !ok && strings.Contains(rec, pkg) {
				leaked = append(leaked, rec)
			}
		}
		if len(leaked) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines still running %s code:\n%s", pkg, strings.Join(leaked, "\n\n"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Close owns the server's background goroutines: it must return even
// against a Revoker that ignores its context (the watcher's own ctx
// arm covers that), and once it returns none of the server's
// goroutines may remain.
func TestCloseJoinsRevocationWatcher(t *testing.T) {
	before := goroutineRecords()
	a := &revokingAuth{Static: authtest.NewStatic(), events: make(chan auth.Revocation)}
	srv, err := NewServer(a, NewProcessor(), "https://jmap.example.com", DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}

	closed := make(chan struct{})
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung: it must not depend on the Revoker closing its channel")
	}
	select {
	case <-srv.watchDone:
	default:
		t.Fatal("Close returned before the revocation watcher exited")
	}
	requireNoNewModuleGoroutines(t, before)
}

// Without a Revoker there is no watcher; Close must not wait for one,
// and calling it again must be a no-op.
func TestCloseIdempotentWithoutRevoker(t *testing.T) {
	srv, err := NewServer(authtest.NewStatic(), NewProcessor(), "https://jmap.example.com", DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	srv.Close()
}
