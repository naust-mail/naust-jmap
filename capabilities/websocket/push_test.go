package websocket

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/capabilities/websocket/internal/frame"
	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// pushTestServer is a runtime server with two standard types and the
// WebSocket handler push-enabled, so real commits drive StateChange
// notifications end to end.
func pushTestServer(t *testing.T) *testServer {
	t.Helper()
	return pushServerWithTypes(t, map[string]string{"TestNote": "subject", "TestTask": "title"})
}

// pushServerWithTypes builds the same server with caller-chosen type
// names (name -> string property), so spec examples can use their
// verbatim data type names.
func pushServerWithTypes(t *testing.T, types map[string]string) *testServer {
	t.Helper()
	core := runtime.DefaultCoreCapabilities()
	proc := runtime.NewProcessor()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	for name, prop := range types {
		dt := &descriptor.Type{
			Name:       name,
			Capability: "urn:example:testnote",
			Properties: map[string]descriptor.Property{prop: {Kind: descriptor.KindString}},
		}
		if err := runtime.RegisterStandardType(proc, db, dt, core); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := runtime.NewServer(staticAuth{}, proc, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	env := &testServer{srv: srv, gate: make(chan struct{})}
	env.handler = NewHandler(srv, staticAuth{})
	env.handler.EnablePush(db, notify.NewInProcess())
	err = srv.Capability("urn:example:testnote").
		Advertise(struct{}{}, struct{}{}).
		Err()
	if err != nil {
		t.Fatal(err)
	}
	err = srv.Capability(CapabilityURI).
		Advertise(SessionCapability(srv.BaseURL(), "/ws", env.handler.SupportsPush()), struct{}{}).
		Handle("/ws", env.handler).
		Err()
	if err != nil {
		t.Fatal(err)
	}
	env.ts = httptest.NewServer(srv)
	t.Cleanup(env.ts.Close)
	return env
}

// createNote commits one record of the given type over the socket, so
// the change notification pipeline runs exactly as in production.
func createObject(c *wsClient, typeName, prop string) {
	c.t.Helper()
	c.send(frame.OpText, []byte(fmt.Sprintf(
		`{"@type":"Request","id":"create","using":["urn:ietf:params:jmap:core","urn:example:testnote"],`+
			`"methodCalls":[["%s/set",{"accountId":"Atest1","create":{"c":{%q:"x"}}},"c0"]]}`,
		typeName, prop)))
	// With push enabled, the commit's own StateChange may arrive before
	// the Response; recvType sets it aside for the assertions that want it.
	m := c.recvType("Response", 5*time.Second)
	if mr := m["methodResponses"].([]any)[0].([]any); mr[0] == "error" {
		c.t.Fatalf("create failed: %v", mr)
	}
}

// assertNoPush proves silence: nothing set aside and nothing arriving.
func assertNoPush(c *wsClient, wait time.Duration) {
	c.t.Helper()
	if len(c.pending) > 0 {
		c.t.Fatalf("unexpected push traffic: %v", c.pending)
	}
	if _, payload, err := c.recv(wait); err == nil {
		c.t.Fatalf("unexpected frame: %q", payload)
	}
}

// recvStateChange asserts the next frame is a StateChange and returns
// the changed map; it also asserts no pushState member is ever sent
// (this server never issues them; RFC 8887 section 4.3.5.1 sanctions
// that).
func recvStateChange(c *wsClient) map[string]any {
	c.t.Helper()
	m := c.recvType("StateChange", 5*time.Second)
	if _, has := m["pushState"]; has {
		c.t.Fatal("a pushState member was sent; this server must never issue one")
	}
	return m["changed"].(map[string]any)
}

// Enabling push sends one full-state snapshot first (the section
// 4.3.5.2 example flow), then streams changes as commits land.
func TestPushSnapshotThenStream(t *testing.T) {
	env := pushTestServer(t)
	c := dialWS(t, env)
	createObject(c, "TestNote", "subject")

	// The enable message mirrors the section 4.4 example, including a
	// pushState token from "a previous connection" - which this server
	// ignores; the snapshot below covers everything the token could.
	c.send(frame.OpText, []byte(`{"@type":"WebSocketPushEnable","dataTypes":["TestNote","TestTask"],"pushState":"aaa"}`))
	changed := recvStateChange(c)
	acct := changed["Atest1"].(map[string]any)
	if acct["TestNote"] == nil || acct["TestTask"] == nil {
		t.Fatalf("snapshot must carry current state for every requested type: %v", changed)
	}
	noteState := acct["TestNote"]

	createObject(c, "TestNote", "subject")
	changed = recvStateChange(c)
	if got := changed["Atest1"].(map[string]any)["TestNote"]; got == noteState {
		t.Fatalf("streamed state %v did not advance from snapshot state %v", got, noteState)
	}
}

// The RFC 8887 section 4.4 example enable message, verbatim: data
// types "Mailbox" and "Email" plus a pushState carried over from a
// previous connection. This server ignores the pushState (section
// 4.3.5.1 sanctions never issuing one), so the reply is a full-state
// snapshot covering both requested types, followed by streamed
// StateChange objects as commits land - which together deliver
// everything the client's stale token could have asked for.
func TestPushEnableSection44Example(t *testing.T) {
	env := pushServerWithTypes(t, map[string]string{"Mailbox": "name", "Email": "subject"})
	c := dialWS(t, env)
	c.send(frame.OpText, []byte(`{
  "@type": "WebSocketPushEnable",
  "dataTypes": [ "Mailbox", "Email" ],
  "pushState": "aaa"
}`))
	changed := recvStateChange(c)
	acct := changed["Atest1"].(map[string]any)
	if acct["Mailbox"] == nil || acct["Email"] == nil {
		t.Fatalf("snapshot must cover both example data types: %v", changed)
	}
	createObject(c, "Mailbox", "name")
	changed = recvStateChange(c)
	acct = changed["Atest1"].(map[string]any)
	if acct["Mailbox"] == nil {
		t.Fatalf("streamed change missing Mailbox state: %v", changed)
	}
	if _, has := acct["Email"]; has {
		t.Fatalf("a Mailbox commit must not report an Email state: %v", changed)
	}
}

// dataTypes filters what is pushed: other types are omitted and empty
// events are suppressed; null means every type.
func TestPushFilterAndNull(t *testing.T) {
	env := pushTestServer(t)
	c := dialWS(t, env)

	c.send(frame.OpText, []byte(`{"@type":"WebSocketPushEnable","dataTypes":["TestTask"]}`))
	changed := recvStateChange(c)
	acct := changed["Atest1"].(map[string]any)
	if _, has := acct["TestNote"]; has {
		t.Fatalf("snapshot leaked an unrequested type: %v", changed)
	}

	// A TestNote commit must not produce an event...
	createObject(c, "TestNote", "subject")
	assertNoPush(c, 400*time.Millisecond)
	// ...but a TestTask commit must.
	createObject(c, "TestTask", "title")
	changed = recvStateChange(c)
	if changed["Atest1"].(map[string]any)["TestTask"] == nil {
		t.Fatalf("requested type missing: %v", changed)
	}

	// Re-enabling with null reconfigures to all types.
	c.send(frame.OpText, []byte(`{"@type":"WebSocketPushEnable","dataTypes":null}`))
	changed = recvStateChange(c)
	acct = changed["Atest1"].(map[string]any)
	if acct["TestNote"] == nil || acct["TestTask"] == nil {
		t.Fatalf("null dataTypes must cover all types: %v", changed)
	}
	createObject(c, "TestNote", "subject")
	if recvStateChange(c)["Atest1"].(map[string]any)["TestNote"] == nil {
		t.Fatal("reconfigured stream missed a change")
	}
}

func TestPushDisable(t *testing.T) {
	env := pushTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpText, []byte(`{"@type":"WebSocketPushEnable","dataTypes":null}`))
	recvStateChange(c)

	c.send(frame.OpText, []byte(`{"@type":"WebSocketPushDisable"}`))
	// The disable has no reply; give it a moment to land, then prove
	// silence across a commit.
	time.Sleep(100 * time.Millisecond)
	createObject(c, "TestNote", "subject")
	assertNoPush(c, 400*time.Millisecond)

	// Disabling again (with nothing enabled) is a no-op, not an error.
	c.send(frame.OpText, []byte(`{"@type":"WebSocketPushDisable"}`))
	assertNoPush(c, 300*time.Millisecond)
}

func TestPushEnableRejectsUnknownType(t *testing.T) {
	env := pushTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpText, []byte(`{"@type":"WebSocketPushEnable","id":"p1","dataTypes":["Nope"]}`))
	m := c.recvText(5 * time.Second)
	if m["@type"] != "RequestError" || m["requestId"] != "p1" {
		t.Fatalf("unknown data type: %v", m)
	}
	// Malformed enable object.
	c.send(frame.OpText, []byte(`{"@type":"WebSocketPushEnable","dataTypes":"notalist"}`))
	if m := c.recvText(5 * time.Second); m["@type"] != "RequestError" {
		t.Fatalf("malformed enable: %v", m)
	}
}

// expiringAuth authenticates until expired is set - the no-Revoker
// case the re-auth backstop exists for.
type expiringAuth struct {
	expired atomic.Bool
}

func (a *expiringAuth) Authenticate(r *http.Request) (*auth.Identity, error) {
	if a.expired.Load() {
		return nil, auth.ErrUnauthenticated
	}
	return staticAuth{}.Authenticate(r)
}

// With no Revoker, the periodic backstop re-runs Authenticate on the
// stored handshake request and closes with 1008 once the credentials
// die (RFC 8887 section 4.2).
func TestReauthBackstopClosesOnExpiredCredentials(t *testing.T) {
	// Restore via t.Cleanup, not defer: cleanups run after the server's
	// own cleanup has waited out every connection goroutine, so the
	// restore write cannot race a reauth loop still reading the knob.
	oldInterval := ReauthInterval
	t.Cleanup(func() { ReauthInterval = oldInterval })
	ReauthInterval = 30 * time.Millisecond

	a := &expiringAuth{}
	proc := runtime.NewProcessor()
	srv, err := runtime.NewServer(a, proc, "https://jmap.example.com", runtime.DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	env := &testServer{srv: srv, gate: make(chan struct{})}
	env.handler = NewHandler(srv, a)
	if err := srv.Capability(CapabilityURI).Handle("/ws", env.handler).Err(); err != nil {
		t.Fatal(err)
	}
	env.ts = httptest.NewServer(srv)
	t.Cleanup(env.ts.Close)
	// Hijacked handlers outlive ts.Close; wait for the handler's own
	// untrack (a mutex edge) so the knob restore cannot race the reauth
	// loop of a connection still winding down.
	t.Cleanup(func() {
		for i := 0; i < 500; i++ {
			env.handler.mu.Lock()
			n := len(env.handler.conns)
			env.handler.mu.Unlock()
			if n == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("connections never untracked")
	})

	c := dialWS(t, env)
	c.send(frame.OpText, request("R1", "Core/echo", "c0"))
	c.recvText(5 * time.Second)

	a.expired.Store(true)
	op, payload, err := c.recv(5 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("op %d err %v, want a close frame", op, err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1008 {
		t.Fatalf("close code %d, want 1008", code)
	}
}

// The advertised capability object carries the wss URL and the real
// supportsPush value (RFC 8887 section 3).
func TestSessionCapabilityAdvertised(t *testing.T) {
	env := pushTestServer(t)
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/.well-known/jmap", nil)
	req.SetBasicAuth("john@example.com", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var session struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	var cap Capability
	if err := json.Unmarshal(session.Capabilities[CapabilityURI], &cap); err != nil {
		t.Fatal(err)
	}
	if cap.URL != "wss://jmap.example.com/ws" || !cap.SupportsPush {
		t.Fatalf("capability: %+v", cap)
	}
}
