package websocket

import (
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/capabilities/websocket/internal/frame"
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
// the shutdown that triggered it).
func requireNoNewModuleGoroutines(t *testing.T, before map[string]string) {
	t.Helper()
	const pkg = "naust-jmap/capabilities/websocket"
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

// Shutdown's contract is total: it returns only after every tracked
// connection's serving goroutine has finished, so a caller may release
// anything those goroutines touch the moment it returns.
func TestShutdownJoinsConnGoroutines(t *testing.T) {
	before := goroutineRecords()
	env := newTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpText, request("R1", "Core/echo", "c0"))
	c.recvType("Response", 5*time.Second)

	env.handler.mu.Lock()
	var tracked []*conn
	for tc := range env.handler.conns {
		tracked = append(tracked, tc)
	}
	env.handler.mu.Unlock()
	if len(tracked) != 1 {
		t.Fatalf("tracked %d connections, want 1", len(tracked))
	}

	env.handler.Shutdown()
	select {
	case <-tracked[0].done:
	default:
		t.Fatal("Shutdown returned before the connection goroutine finished")
	}
	requireNoNewModuleGoroutines(t, before)
}

// Shutdown must not return while a connection's request goroutine is
// still running, even after the drain and close-reply deadlines have
// forced the socket down: the join is on the goroutine, not the socket.
func TestShutdownWaitsHungRequestOut(t *testing.T) {
	oldDrain, oldReply := DrainDeadline, CloseReplyDeadline
	DrainDeadline, CloseReplyDeadline = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { DrainDeadline, CloseReplyDeadline = oldDrain, oldReply })

	env := newTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpText, []byte(`{"@type":"Request","id":"R1","using":["urn:ietf:params:jmap:core","urn:example:test"],"methodCalls":[["Test/hang",{},"c0"]]}`))
	select {
	case <-env.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Test/hang never started")
	}

	done := make(chan struct{})
	go func() { env.handler.Shutdown(); close(done) }()
	select {
	case <-done:
		t.Fatal("Shutdown returned while a request goroutine was still running")
	case <-time.After(300 * time.Millisecond):
	}

	close(env.gate)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return after the hung request finished")
	}
}
