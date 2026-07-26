package runtime

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/internal/authtest"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// revokingAuth is authtest.Static plus an auth.Revoker stream driven by
// the test.
type revokingAuth struct {
	*authtest.Static
	events chan string
}

func (a *revokingAuth) Revocations(_ context.Context) <-chan string { return a.events }

// A revocation must kill an already-established EventSource stream, not
// just fail the next request: the stream authenticated once and would
// otherwise outlive its credentials.
func TestRevocationClosesEventSource(t *testing.T) {
	core := DefaultCoreCapabilities()
	a := &revokingAuth{Static: authtest.NewStatic(), events: make(chan string)}
	a.AddUser("john@example.com", "secret", "Atest1")
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	if err := RegisterStandardType(p, db, testNoteType(), core); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Capability("urn:example:testnote").Advertise(struct{}{}, struct{}{}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := srv.EnablePush(db, notify.NewInProcess(), nil, nil); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	c := openEventSource(t, ts, "types=*&closeafter=no&ping=0")
	if name, _, err := c.readEvent(); err != nil || name != "state" {
		t.Fatalf("initial event: %q, %v", name, err)
	}

	a.events <- "john@example.com"

	dead := make(chan error, 1)
	go func() {
		_, _, err := c.readEvent()
		dead <- err
	}()
	select {
	case err := <-dead:
		if err == nil {
			t.Fatal("stream produced an event after revocation instead of closing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after revocation")
	}
}

// A revocation for a username with no live connections is a no-op, and
// connections of other users survive it.
func TestRevocationScopedToUsername(t *testing.T) {
	srv := registrarServer(t)
	var johnClosed, janeClosed atomic.Int32
	unregJohn := srv.RegisterConnection("john@example.com", func() { johnClosed.Add(1) })
	defer unregJohn()
	unregJane := srv.RegisterConnection("jane@example.com", func() { janeClosed.Add(1) })
	defer unregJane()

	srv.revokeConnections("ghost@example.com")
	srv.revokeConnections("john@example.com")
	srv.revokeConnections("john@example.com") // second revocation: entries already detached

	if got := johnClosed.Load(); got != 1 {
		t.Errorf("john close calls = %d, want 1", got)
	}
	if got := janeClosed.Load(); got != 0 {
		t.Errorf("jane closed by another user's revocation")
	}
}

// Register/unregister churn racing revocations must keep the index
// balanced: no leaked entries, no double closes, no deadlock.
func TestConnectionRegistryChurn(t *testing.T) {
	srv := registrarServer(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				srv.revokeConnections("churn@example.com")
			}
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				var closed atomic.Int32
				unreg := srv.RegisterConnection("churn@example.com", func() { closed.Add(1) })
				unreg()
				unreg() // idempotent by contract
				if closed.Load() > 1 {
					t.Error("connection closed more than once")
					return
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()

	srv.connMu.Lock()
	defer srv.connMu.Unlock()
	if len(srv.conns) != 0 {
		t.Fatalf("connection index leaked entries: %v", srv.conns)
	}
}
