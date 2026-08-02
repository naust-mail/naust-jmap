package websocket

import (
	"context"
	"math/rand"
	"net/http/httptest"
	"os"
	goruntime "runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// TestConnectionChurnSoak collides every connection lifecycle path at
// once: clients dialing, requesting, enabling push, closing cleanly,
// and dropping TCP mid-request, while revocations repeatedly kill
// every live connection - then proves nothing leaked: the connection
// registry drains, the shared request pool returns to full capacity,
// and the process goroutine count settles back to its baseline. The
// pairwise interactions each have their own test; this one exists for
// the collisions between them.
//
// The default duration keeps the normal suite fast; set
// NAUST_SOAK_SECONDS for a longer unattended run.
func TestConnectionChurnSoak(t *testing.T) {
	dur := 3 * time.Second
	if s := os.Getenv("NAUST_SOAK_SECONDS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("NAUST_SOAK_SECONDS: %v", err)
		}
		dur = time.Duration(n) * time.Second
	}

	// Settle, then take the goroutine baseline before any server exists.
	goruntime.GC()
	time.Sleep(50 * time.Millisecond)
	baseline := goruntime.NumGoroutine()

	a := &revokingWSAuth{events: make(chan auth.Revocation)}
	proc := runtime.NewProcessor()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	dt := &descriptor.Type{
		Name:       "TestNote",
		Capability: "urn:example:testnote",
		Properties: map[string]descriptor.Property{"subject": {Kind: descriptor.KindString}},
	}
	core := runtime.DefaultCoreCapabilities()
	if err := runtime.RegisterStandardType(proc, db, dt, core); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(a, proc, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(srv, a, Config{})
	handler.EnablePush(db, notify.NewInProcess())
	err = srv.Capability("urn:example:testnote").Advertise(struct{}{}, struct{}{}).Err()
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Capability(CapabilityURI).Handle("/ws", handler).Err(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)

	deadline := time.Now().Add(dur)
	var wg sync.WaitGroup

	// The revoker fires continuously, killing every live connection of
	// the one soak user over and over while clients keep reconnecting.
	revokerStop := make(chan struct{})
	revokerDone := make(chan struct{})
	go func() {
		defer close(revokerDone)
		for {
			select {
			case <-revokerStop:
				return
			case <-time.After(40 * time.Millisecond):
				select {
				case a.events <- auth.Revocation{Username: "john@example.com", At: time.Now()}:
				case <-revokerStop:
					return
				}
			}
		}
	}()

	// Client messages: valid labeled and unlabeled requests, a commit
	// that drives push, garbage, push enable/disable, and a clean close.
	msgs := []string{
		`{"@type":"Request","id":"s1","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"n":1},"c0"]]}`,
		`{"@type":"Request","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"n":2},"c0"]]}`,
		`{"@type":"Request","id":"s2","using":["urn:ietf:params:jmap:core","urn:example:testnote"],` +
			`"methodCalls":[["TestNote/set",{"accountId":"Atest1","create":{"c":{"subject":"x"}}},"c0"]]}`,
		`the quick brown fox`,
		`{"@type":"WebSocketPushEnable","dataTypes":null}`,
		`{"@type":"WebSocketPushDisable"}`,
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for time.Now().Before(deadline) {
				c, err := dialFuzz(ts.URL)
				if err != nil {
					continue // revocation storms may refuse; redial
				}
				steps := 1 + rng.Intn(4)
				for s := 0; s < steps; s++ {
					if c.sendText([]byte(msgs[rng.Intn(len(msgs))])) != nil {
						break
					}
					// Drain briefly; replies, pushes, and 1008 closes are
					// all fine, this loop only cares about leaks.
					if _, _, err := c.next(time.Now().Add(20 * time.Millisecond)); err != nil {
						if rng.Intn(2) == 0 {
							break
						}
					}
				}
				if rng.Intn(3) == 0 {
					// A third of the connections vanish mid-conversation
					// instead of closing; the rest just drop after use.
					c.sendText([]byte(msgs[rng.Intn(len(msgs))]))
				}
				c.nc.Close()
			}
		}(int64(i))
	}

	wg.Wait()
	close(revokerStop)
	<-revokerDone
	handler.Shutdown()
	ts.Close()

	// The registry must drain: hijacked handlers outlive ts.Close, so
	// poll the handler's own untrack edge.
	drained := false
	for i := 0; i < 500 && !drained; i++ {
		handler.mu.Lock()
		drained = len(handler.conns) == 0
		handler.mu.Unlock()
		if !drained {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !drained {
		t.Fatal("connection registry never drained after shutdown")
	}

	// The shared pool must be whole again: with maxConcurrentRequests 4
	// and a per-user share of 2, two users must be able to fill it.
	ident := func(u string) *auth.Identity {
		return &auth.Identity{
			Username: u,
			Accounts: map[jmap.Id]auth.Access{"Atest1": {Name: u}},
			Primary:  "Atest1",
		}
	}
	acqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var slots []*runtime.RequestSlot
	for _, u := range []string{"john@example.com", "jane@example.com"} {
		for j := 0; j < 2; j++ {
			s, err := srv.AcquireSlot(acqCtx, ident(u))
			if err != nil {
				t.Fatalf("pool slot leaked: %d of 4 reacquirable (%v)", len(slots), err)
			}
			slots = append(slots, s)
		}
	}
	for _, s := range slots {
		s.Release()
	}

	// Goroutines must settle near the baseline. The revocation watcher
	// the server started is permanent by design; a per-connection leak
	// would show up as dozens after this much churn.
	const slack = 8
	for i := 0; i < 100; i++ {
		goruntime.GC()
		if goruntime.NumGoroutine() <= baseline+slack {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	buf := make([]byte, 1<<20)
	t.Fatalf("goroutines: baseline %d, now %d\n%s",
		baseline, goruntime.NumGoroutine(), buf[:goruntime.Stack(buf, true)])
}
