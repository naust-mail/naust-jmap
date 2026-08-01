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
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// revokingAuth is authtest.Static plus an auth.Revoker stream driven by
// the test.
type revokingAuth struct {
	*authtest.Static
	events chan auth.Revocation
}

func (a *revokingAuth) Revocations(_ context.Context) <-chan auth.Revocation { return a.events }

// A revocation must kill an already-established EventSource stream, not
// just fail the next request: the stream authenticated once and would
// otherwise outlive its credentials.
func TestRevocationClosesEventSource(t *testing.T) {
	core := DefaultCoreCapabilities()
	a := &revokingAuth{Static: authtest.NewStatic(), events: make(chan auth.Revocation)}
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

	a.events <- auth.Revocation{Username: "john@example.com", At: time.Now()}

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

	srv.revokeConnections("ghost@example.com", time.Now())
	srv.revokeConnections("john@example.com", time.Now())
	srv.revokeConnections("john@example.com", time.Now()) // second revocation: entries already detached

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
				srv.revokeConnections("churn@example.com", time.Now())
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

// Revocation delivery is at-least-once (see auth.Revoker), so applying
// an event must be idempotent: only connections established before the
// revocation (plus clock slack) die; a connection established after
// that survives any number of redeliveries of the same event.
func TestRevocationIdempotentByTime(t *testing.T) {
	srv := registrarServer(t)
	var oldClosed, newClosed atomic.Int32

	// Revocation instant in the past; the pre-revocation connection is
	// backdated behind it, the post-revocation one registers at real
	// now, safely beyond at+slack.
	at := time.Now().Add(-2 * revocationClockSlack)

	unregOld := srv.RegisterConnection("kim@example.com", func() { oldClosed.Add(1) })
	defer unregOld()
	srv.connMu.Lock()
	for e := range srv.conns["kim@example.com"] {
		e.at = at.Add(-time.Minute)
	}
	srv.connMu.Unlock()

	unregNew := srv.RegisterConnection("kim@example.com", func() { newClosed.Add(1) })
	defer unregNew()

	srv.revokeConnections("kim@example.com", at)
	srv.revokeConnections("kim@example.com", at) // redelivery of the same event

	if got := oldClosed.Load(); got != 1 {
		t.Errorf("pre-revocation connection close calls = %d, want 1", got)
	}
	if got := newClosed.Load(); got != 0 {
		t.Errorf("post-revocation connection closed %d times, want to survive", got)
	}
}

// A connection registered inside the slack window after the revocation
// instant is closed: the comparison is biased toward closing because
// the revocation's clock and this host's clock may disagree.
func TestRevocationSlackBiasesTowardClosing(t *testing.T) {
	srv := registrarServer(t)
	var closed atomic.Int32
	unreg := srv.RegisterConnection("lee@example.com", func() { closed.Add(1) })
	defer unreg()

	// Revocation stamped before the registration but within slack.
	at := time.Now().Add(-revocationClockSlack / 2)
	srv.revokeConnections("lee@example.com", at)

	if got := closed.Load(); got != 1 {
		t.Errorf("in-slack connection close calls = %d, want 1", got)
	}
}
