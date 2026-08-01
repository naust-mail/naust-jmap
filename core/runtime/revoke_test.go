package runtime

import (
	"context"
	"errors"
	"net/http"
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
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// revokingAuth is authtest.Static plus an auth.Revoker stream driven by
// the test.
type revokingAuth struct {
	*authtest.Static
	events chan auth.Revocation
}

func (a *revokingAuth) Revocations(_ context.Context) <-chan auth.Revocation { return a.events }

// mustRegister registers a connection in tests that are not about
// admission; under the default per-user cap it cannot fail.
func mustRegister(t *testing.T, srv *Server, username string, close func()) func() {
	t.Helper()
	unregister, err := srv.RegisterConnection(username, close)
	if err != nil {
		t.Fatal(err)
	}
	return unregister
}

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
	unregJohn := mustRegister(t, srv, "john@example.com", func() { johnClosed.Add(1) })
	defer unregJohn()
	unregJane := mustRegister(t, srv, "jane@example.com", func() { janeClosed.Add(1) })
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
				unreg, err := srv.RegisterConnection("churn@example.com", func() { closed.Add(1) })
				if err != nil {
					t.Error(err)
					return
				}
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

	unregOld := mustRegister(t, srv, "kim@example.com", func() { oldClosed.Add(1) })
	defer unregOld()
	srv.connMu.Lock()
	for e := range srv.conns["kim@example.com"] {
		e.at = at.Add(-time.Minute)
	}
	srv.connMu.Unlock()

	unregNew := mustRegister(t, srv, "kim@example.com", func() { newClosed.Add(1) })
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
	unreg := mustRegister(t, srv, "lee@example.com", func() { closed.Add(1) })
	defer unreg()

	// Revocation stamped before the registration but within slack.
	at := time.Now().Add(-revocationClockSlack / 2)
	srv.revokeConnections("lee@example.com", at)

	if got := closed.Load(); got != 1 {
		t.Errorf("in-slack connection close calls = %d, want 1", got)
	}
}

// Registration is the per-user admission point (RFC 8620 section 8.5):
// a username at tuning.MaxConnectionsPerUser is refused, the count is
// per username, unregistering frees a slot, and a non-positive cap
// disables the bound.
func TestConnectionCapPerUser(t *testing.T) {
	old := tuning.MaxConnectionsPerUser
	tuning.MaxConnectionsPerUser = 2
	t.Cleanup(func() { tuning.MaxConnectionsPerUser = old })
	srv := registrarServer(t)

	unreg1 := mustRegister(t, srv, "max@example.com", func() {})
	defer unreg1()
	unreg2 := mustRegister(t, srv, "max@example.com", func() {})
	defer unreg2()

	if _, err := srv.RegisterConnection("max@example.com", func() {}); !errors.Is(err, ErrTooManyConnections) {
		t.Fatalf("third registration err = %v, want ErrTooManyConnections", err)
	}

	// The bound is per username, not global.
	unregOther := mustRegister(t, srv, "other@example.com", func() {})
	defer unregOther()

	// Unregistering releases the slot.
	unreg2()
	unreg3 := mustRegister(t, srv, "max@example.com", func() {})
	defer unreg3()

	// A non-positive cap disables admission entirely.
	tuning.MaxConnectionsPerUser = 0
	unreg4 := mustRegister(t, srv, "max@example.com", func() {})
	defer unreg4()
}

// An EventSource request from a user already holding their full
// connection quota is refused with 429 before the stream starts.
func TestConnectionCapRefusesEventSource(t *testing.T) {
	old := tuning.MaxConnectionsPerUser
	tuning.MaxConnectionsPerUser = 1
	t.Cleanup(func() { tuning.MaxConnectionsPerUser = old })
	ts := pushServer(t)

	c := openEventSource(t, ts, "types=*&closeafter=no&ping=0")
	if name, _, err := c.readEvent(); err != nil || name != "state" {
		t.Fatalf("initial event: %q, %v", name, err)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/eventsource?types=*&closeafter=no&ping=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("john@example.com", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second stream status = %d, want 429", resp.StatusCode)
	}
}
