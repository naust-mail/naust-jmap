package websocket

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/capabilities/websocket/internal/frame"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// staticAuth authenticates HTTP Basic john@example.com/secret; the
// module cannot reach the core's internal test authenticator.
type staticAuth struct{}

func (staticAuth) Authenticate(r *http.Request) (*auth.Identity, error) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != "john@example.com" || pass != "secret" {
		return nil, auth.ErrUnauthenticated
	}
	return &auth.Identity{
		Username: "john@example.com",
		Accounts: map[jmap.Id]auth.Access{"Atest1": {Name: user, Personal: true}},
		Primary:  "Atest1",
	}, nil
}

// testServer mounts a runtime server with the WebSocket handler on
// /ws plus a gated "Test/slow" method for concurrency tests.
type testServer struct {
	ts      *httptest.Server
	srv     *runtime.Server
	handler *Handler
	gate    chan struct{} // closing it releases every Test/slow call
	started chan struct{} // Test/hang signals here when it begins
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	proc := runtime.NewProcessor()
	srv, err := runtime.NewServer(staticAuth{}, proc, "https://jmap.example.com", runtime.DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	env := &testServer{srv: srv, gate: make(chan struct{}), started: make(chan struct{}, 8)}
	env.handler = NewHandler(srv, staticAuth{}, Config{})
	err = srv.Capability(CapabilityURI).
		Advertise(SessionCapability(srv.BaseURL(), "/ws", false), struct{}{}).
		Handle("/ws", env.handler).
		Err()
	if err != nil {
		t.Fatal(err)
	}
	err = srv.Capability("urn:example:test").
		Advertise(struct{}{}, struct{}{}).
		Method("Test/slow", func(ctx context.Context, call *runtime.Call) []jmap.Invocation {
			select {
			case <-env.gate:
			case <-ctx.Done():
			}
			return []jmap.Invocation{{Name: "Test/slow", Args: json.RawMessage(`{"done":true}`), CallID: call.CallID}}
		}).
		Method("Test/hang", func(ctx context.Context, call *runtime.Call) []jmap.Invocation {
			// Deliberately ignores ctx: models a method that keeps
			// working after cancellation, for teardown-ordering tests.
			select {
			case env.started <- struct{}{}:
			default:
			}
			<-env.gate
			return []jmap.Invocation{{Name: "Test/hang", Args: json.RawMessage(`{}`), CallID: call.CallID}}
		}).
		Err()
	if err != nil {
		t.Fatal(err)
	}
	env.ts = httptest.NewServer(srv)
	t.Cleanup(env.ts.Close)
	// Shutdown synchronizes with every tracked connection's goroutine;
	// without it a cleanup restoring a package tuning var races the
	// hijacked connection's setup-time read of that var (the httptest
	// server does not wait for hijacked connections).
	t.Cleanup(env.handler.Shutdown)
	return env
}

// wsClient is a minimal RFC 6455 client for tests: masked writes,
// unmasked reads.
type wsClient struct {
	t  *testing.T
	nc net.Conn
	br *bufio.Reader
	// pending holds decoded text messages set aside while waiting for a
	// specific @type: with push enabled, StateChange frames interleave
	// freely with Response frames.
	pending []map[string]any
}

func dialWS(t *testing.T, env *testServer) *wsClient {
	t.Helper()
	return dialURL(t, env.ts.URL)
}

// dialURL performs the handshake against any test server URL, so tests
// can wrap the handler in their own http server when they need to
// observe ServeHTTP itself.
func dialURL(t *testing.T, url string) *wsClient {
	t.Helper()
	nc, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	fmt.Fprintf(nc, "GET /ws HTTP/1.1\r\n"+
		"Host: server.example.com\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Authorization: Basic am9obkBleGFtcGxlLmNvbTpzZWNyZXQ=\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Protocol: jmap\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("handshake status %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "jmap" {
		t.Fatalf("subprotocol %q", got)
	}
	return &wsClient{t: t, nc: nc, br: br}
}

// sendFrame writes one masked client frame.
func (c *wsClient) sendFrame(fin bool, op byte, payload []byte) {
	c.t.Helper()
	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	var b []byte
	b0 := op
	if fin {
		b0 |= 0x80
	}
	b = append(b, b0)
	switch {
	case len(payload) <= 125:
		b = append(b, 0x80|byte(len(payload)))
	case len(payload) <= 0xffff:
		b = append(b, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
	default:
		b = append(b, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(len(payload)))
		b = append(b, ext[:]...)
	}
	b = append(b, key[:]...)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ key[i%4]
	}
	if _, err := c.nc.Write(append(b, masked...)); err != nil {
		c.t.Fatal(err)
	}
}

func (c *wsClient) send(op byte, payload []byte) { c.sendFrame(true, op, payload) }

// recv reads one unmasked server frame within the deadline.
func (c *wsClient) recv(timeout time.Duration) (op byte, payload []byte, err error) {
	c.nc.SetReadDeadline(time.Now().Add(timeout))
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	if hdr[1]&0x80 != 0 {
		c.t.Fatal("server frame is masked (RFC 6455 section 5.1 forbids it)")
	}
	length := int64(hdr[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0] & 0x0f, payload, nil
}

// recvText asserts the next frame is text and decodes it. Messages set
// aside by recvType are returned first.
func (c *wsClient) recvText(timeout time.Duration) map[string]any {
	c.t.Helper()
	if len(c.pending) > 0 {
		m := c.pending[0]
		c.pending = c.pending[1:]
		return m
	}
	op, payload, err := c.recv(timeout)
	if err != nil {
		c.t.Fatal(err)
	}
	if op != frame.OpText {
		c.t.Fatalf("frame opcode %d, want text (payload %q)", op, payload)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		c.t.Fatalf("server sent invalid JSON: %v (%q)", err, payload)
	}
	return m
}

// recvType returns the next message of the wanted @type, setting other
// text messages aside for later recvText/recvType calls.
func (c *wsClient) recvType(want string, timeout time.Duration) map[string]any {
	c.t.Helper()
	for i, m := range c.pending {
		if m["@type"] == want {
			c.pending = append(c.pending[:i:i], c.pending[i+1:]...)
			return m
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			c.t.Fatalf("no %s message arrived", want)
		}
		op, payload, err := c.recv(remaining)
		if err != nil {
			c.t.Fatalf("waiting for %s: %v", want, err)
		}
		if op != frame.OpText {
			c.t.Fatalf("frame opcode %d while waiting for %s", op, want)
		}
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil {
			c.t.Fatalf("server sent invalid JSON: %v (%q)", err, payload)
		}
		if m["@type"] == want {
			return m
		}
		c.pending = append(c.pending, m)
	}
}

// --- RFC 8887 section 4.4 examples, verbatim ---

// The Core/echo exchange: request R1 must come back as a Response
// carrying @type "Response", requestId "R1", and the echoed call.
func TestExampleEchoRequest(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpText, []byte(`{
	  "@type": "Request",
	  "id": "R1",
	  "using": [ "urn:ietf:params:jmap:core" ],
	  "methodCalls": [
	    [
	      "Core/echo", {
	        "hello": true,
	        "high": 5
	      },
	      "b3ff"
	    ]
	  ]
	}`))
	m := c.recvText(5 * time.Second)
	if m["@type"] != "Response" || m["requestId"] != "R1" {
		t.Fatalf("envelope: %v", m)
	}
	mr := m["methodResponses"].([]any)[0].([]any)
	if mr[0] != "Core/echo" || mr[2] != "b3ff" {
		t.Fatalf("methodResponses: %v", mr)
	}
	args := mr[1].(map[string]any)
	if args["hello"] != true || args["high"] != float64(5) {
		t.Fatalf("echo args: %v", args)
	}
}

// The non-JSON exchange: "The quick brown fox jumps over the lazy
// dog." must be answered with a RequestError whose requestId is null,
// type notJSON, status 400, and the example's detail text - and the
// connection survives to serve the next request.
func TestExampleNotJSON(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpText, []byte("The quick brown fox jumps\n over the lazy dog."))
	m := c.recvText(5 * time.Second)
	if m["@type"] != "RequestError" {
		t.Fatalf("@type: %v", m)
	}
	if v, present := m["requestId"]; !present || v != nil {
		t.Fatalf("requestId must be present and null: %v", m)
	}
	if m["type"] != "urn:ietf:params:jmap:error:notJSON" || m["status"] != float64(400) {
		t.Fatalf("problem: %v", m)
	}
	if m["detail"] != "The request did not parse as I-JSON." {
		t.Fatalf("detail: %v", m["detail"])
	}

	c.send(frame.OpText, []byte(`{"@type":"Request","id":"R2","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c0"]]}`))
	if m := c.recvText(5 * time.Second); m["requestId"] != "R2" {
		t.Fatal("connection did not survive the request-level error")
	}
}

// --- Concurrency (B2) ---

func request(id, method, callID string) []byte {
	idProp := ""
	if id != "" {
		idProp = fmt.Sprintf("%q:%q,", "id", id)
	}
	return []byte(fmt.Sprintf(`{"@type":"Request",%s"using":["urn:ietf:params:jmap:core","urn:example:test"],"methodCalls":[[%q,{},%q]]}`,
		idProp, method, callID))
}

// Labeled requests run concurrently and responses may return out of
// order (RFC 8887 section 4.3.2): a fast request overtakes a slow one.
func TestOutOfOrderResponses(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpText, request("slow", "Test/slow", "c1"))
	c.send(frame.OpText, request("fast", "Core/echo", "c2"))

	if m := c.recvText(5 * time.Second); m["requestId"] != "fast" {
		t.Fatalf("first response was %v, want the fast overtaker", m["requestId"])
	}
	close(env.gate)
	if m := c.recvText(5 * time.Second); m["requestId"] != "slow" {
		t.Fatal("slow response never arrived")
	}
}

// Requests without an id run strictly in order on the serial lane:
// the second cannot overtake the first even when the first is slow.
func TestIdlessRequestsStayOrdered(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpText, request("", "Test/slow", "c1"))
	c.send(frame.OpText, request("", "Core/echo", "c2"))

	// The fast echo must NOT arrive while the slow one holds the lane.
	if _, _, err := c.recv(300 * time.Millisecond); err == nil {
		t.Fatal("id-less response arrived out of order")
	}
	close(env.gate)
	m1 := c.recvText(5 * time.Second)
	m2 := c.recvText(5 * time.Second)
	name1 := m1["methodResponses"].([]any)[0].([]any)[0]
	name2 := m2["methodResponses"].([]any)[0].([]any)[0]
	if name1 != "Test/slow" || name2 != "Core/echo" {
		t.Fatalf("order: %v then %v", name1, name2)
	}
}

// A full shared pool gates the socket instead of erroring (B2: TCP is
// the waiting room): the request completes once slots free up, and no
// limit error is ever sent.
func TestPoolGatingWithoutErrors(t *testing.T) {
	env := newTestServer(t)
	other := &auth.Identity{Username: "jane@example.com", Accounts: map[jmap.Id]auth.Access{"A2": {}}, Primary: "A2"}
	third := &auth.Identity{Username: "kate@example.com", Accounts: map[jmap.Id]auth.Access{"A3": {}}, Primary: "A3"}
	var held []*runtime.RequestSlot
	for _, id := range []*auth.Identity{other, third} {
		for i := 0; i < 2; i++ {
			s := env.srv.TryAcquireSlot(id)
			if s == nil {
				t.Fatal("could not fill the pool")
			}
			held = append(held, s)
		}
	}

	c := dialWS(t, env)
	c.send(frame.OpText, request("R1", "Core/echo", "c0"))
	if _, _, err := c.recv(300 * time.Millisecond); err == nil {
		t.Fatal("got a response while the pool was full")
	}
	for _, s := range held {
		s.Release()
	}
	if m := c.recvText(5 * time.Second); m["@type"] != "Response" {
		t.Fatalf("after release: %v (a limit error would violate the no-spurious-errors rule)", m)
	}
}

// The lane cap bounds one connection's concurrency but never loses
// requests: with LaneCap labeled slow requests in flight, more just
// wait their turn and all complete.
func TestLaneCapBackpressure(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	for i := 0; i < LaneCap+1; i++ {
		c.send(frame.OpText, request(fmt.Sprintf("R%d", i), "Test/slow", "c"))
	}
	if _, _, err := c.recv(300 * time.Millisecond); err == nil {
		t.Fatal("slow response arrived before the gate opened")
	}
	close(env.gate)
	seen := map[any]bool{}
	for i := 0; i < LaneCap+1; i++ {
		seen[c.recvText(5 * time.Second)["requestId"]] = true
	}
	if len(seen) != LaneCap+1 {
		t.Fatalf("lost responses: %v", seen)
	}
}

// --- Protocol behavior ---

func TestPingPongAndClientClose(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)

	c.send(frame.OpPing, []byte("Hello"))
	op, payload, err := c.recv(5 * time.Second)
	if err != nil || op != frame.OpPong || string(payload) != "Hello" {
		t.Fatalf("pong: op %d payload %q err %v", op, payload, err)
	}

	// Unsolicited pong is a heartbeat; the connection must survive it.
	c.send(frame.OpPong, []byte("beat"))

	// Client-initiated close: the server echoes the code and closes TCP.
	c.send(frame.OpClose, []byte{0x03, 0xe8}) // 1000
	op, payload, err = c.recv(5 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("close reply: op %d err %v", op, err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1000 {
		t.Fatalf("echoed close code %d", code)
	}
	if _, _, err := c.recv(2 * time.Second); err == nil {
		t.Fatal("connection still open after close handshake")
	}
}

// A binary data frame is not part of the jmap subprotocol; this server
// closes with 1003 (RFC 8887 section 4.3.1).
func TestBinaryFrameCloses(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpBinary, []byte{0x01, 0x02})
	op, payload, err := c.recv(5 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("op %d err %v", op, err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1003 {
		t.Fatalf("close code %d, want 1003", code)
	}
}

// A protocol violation (unmasked client frame) fails the connection
// with 1002.
func TestProtocolViolationCloses(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	if _, err := c.nc.Write([]byte{0x81, 0x05, 'H', 'e', 'l', 'l', 'o'}); err != nil {
		t.Fatal(err)
	}
	op, payload, err := c.recv(5 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("op %d err %v", op, err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1002 {
		t.Fatalf("close code %d, want 1002", code)
	}
}

// A fragmented text message is coalesced before parsing (RFC 8887
// section 4.3).
func TestFragmentedRequest(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	body := request("frag", "Core/echo", "c0")
	half := len(body) / 2
	c.sendFrame(false, frame.OpText, body[:half])
	c.sendFrame(true, frame.OpContinuation, body[half:])
	if m := c.recvText(5 * time.Second); m["requestId"] != "frag" {
		t.Fatalf("fragmented request: %v", m)
	}
}

// --- Request-level errors keep the connection alive ---

func TestRequestLevelErrors(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)

	// Unknown @type: notRequest with the id echoed.
	c.send(frame.OpText, []byte(`{"@type":"Bogus","id":"x1"}`))
	m := c.recvText(5 * time.Second)
	if m["@type"] != "RequestError" || m["requestId"] != "x1" || m["type"] != jmap.ProblemNotRequest {
		t.Fatalf("unknown @type: %v", m)
	}

	// A JSON object with no @type at all.
	c.send(frame.OpText, []byte(`{"using":[],"methodCalls":[]}`))
	if m := c.recvText(5 * time.Second); m["type"] != jmap.ProblemNotRequest {
		t.Fatalf("missing @type: %v", m)
	}

	// An oversized request id is refused without being echoed.
	long := strings.Repeat("a", MaxRequestIDLength+1)
	c.send(frame.OpText, []byte(`{"@type":"Request","id":"`+long+`","using":[],"methodCalls":[]}`))
	m = c.recvText(5 * time.Second)
	if m["type"] != jmap.ProblemNotRequest || m["requestId"] != nil {
		t.Fatalf("oversized id: %v", m)
	}

	// Too many method calls: the pipeline's limit error rides back with
	// the id echoed (RFC 8620 section 3.6.1 via RFC 8887 section 4.3.4).
	calls := strings.TrimSuffix(strings.Repeat(`["Core/echo",{},"c"],`, 17), ",")
	c.send(frame.OpText, []byte(`{"@type":"Request","id":"big","using":["urn:ietf:params:jmap:core"],"methodCalls":[`+calls+`]}`))
	m = c.recvText(5 * time.Second)
	if m["type"] != jmap.ProblemLimit || m["limit"] != "maxCallsInRequest" || m["requestId"] != "big" {
		t.Fatalf("limit error: %v", m)
	}

	// Unknown capability in using.
	c.send(frame.OpText, []byte(`{"@type":"Request","id":"cap","using":["urn:example:absent"],"methodCalls":[]}`))
	if m := c.recvText(5 * time.Second); m["type"] != jmap.ProblemUnknownCapability {
		t.Fatalf("unknown capability: %v", m)
	}

	// Push enable without push wiring.
	c.send(frame.OpText, []byte(`{"@type":"WebSocketPushEnable","dataTypes":null}`))
	if m := c.recvText(5 * time.Second); m["@type"] != "RequestError" {
		t.Fatalf("push enable without support: %v", m)
	}

	// After all of that, the connection still works.
	c.send(frame.OpText, request("alive", "Core/echo", "c9"))
	if m := c.recvText(5 * time.Second); m["requestId"] != "alive" {
		t.Fatal("connection died from request-level errors")
	}
}

// --- Revocation and shutdown ---

type revokingWSAuth struct {
	staticAuth
	events chan auth.Revocation
}

func (a *revokingWSAuth) Revocations(_ context.Context) <-chan auth.Revocation { return a.events }

// Revoking the identity closes the socket with 1008 (RFC 8887 section
// 4.2) and cancels its in-flight work.
func TestRevocationClosesSocket(t *testing.T) {
	proc := runtime.NewProcessor()
	a := &revokingWSAuth{events: make(chan auth.Revocation)}
	srv, err := runtime.NewServer(a, proc, "https://jmap.example.com", runtime.DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	env := &testServer{srv: srv, gate: make(chan struct{})}
	env.handler = NewHandler(srv, a, Config{})
	if err := srv.Capability(CapabilityURI).Handle("/ws", env.handler).Err(); err != nil {
		t.Fatal(err)
	}
	env.ts = httptest.NewServer(srv)
	t.Cleanup(env.ts.Close)
	// Hijacked handlers outlive ts.Close; Shutdown waits for them, so a
	// later test's write to a package tuning var cannot race this
	// connection's registration-time reads.
	t.Cleanup(env.handler.Shutdown)

	c := dialWS(t, env)
	// Prove liveness, then revoke.
	c.send(frame.OpText, request("R1", "Core/echo", "c0"))
	c.recvText(5 * time.Second)

	a.events <- auth.Revocation{Username: "john@example.com", At: time.Now()}
	op, payload, err := c.recv(5 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("op %d err %v, want a close frame", op, err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1008 {
		t.Fatalf("close code %d, want 1008", code)
	}
}

// A user already holding their full per-user connection quota (RFC 8620
// section 8.5) is turned away with 1013 right after the handshake; the
// first connection stays usable.
func TestConnectionCapClosesSocket(t *testing.T) {
	old := tuning.MaxConnectionsPerUser
	tuning.MaxConnectionsPerUser = 1
	t.Cleanup(func() { tuning.MaxConnectionsPerUser = old })
	env := newTestServer(t)

	c1 := dialWS(t, env)
	// Prove the first connection is admitted and live.
	c1.send(frame.OpText, request("R1", "Core/echo", "c0"))
	c1.recvText(5 * time.Second)

	c2 := dialWS(t, env)
	op, payload, err := c2.recv(5 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("op %d err %v, want a close frame", op, err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != frame.CloseTryAgainLater {
		t.Fatalf("close code %d, want %d", code, frame.CloseTryAgainLater)
	}

	// The admitted connection is unaffected by the refusal.
	c1.send(frame.OpText, request("R2", "Core/echo", "c1"))
	c1.recvText(5 * time.Second)
}

// Graceful shutdown: in-flight work drains and flushes, then 1001.
func TestGracefulShutdown(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpText, request("R1", "Test/slow", "c0"))

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond) // let the request start
		close(env.gate)
		env.handler.Shutdown()
	}()

	// The in-flight response arrives BEFORE the close frame: drain
	// first, then 1001 (RFC 6455 section 5.5.1 forbids data after Close).
	m := c.recvText(5 * time.Second)
	if m["requestId"] != "R1" {
		t.Fatalf("expected the drained response first, got %v", m)
	}
	op, payload, err := c.recv(5 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("op %d err %v", op, err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1001 {
		t.Fatalf("close code %d, want 1001", code)
	}
	wg.Wait()
}

// --- Adversarial and edge-case messages ---

// Valid JSON that is not a JMAP message object gets notRequest; only
// bytes that fail to parse at all get notJSON (RFC 8620 section 3.6.1
// via RFC 8887 section 4.3.1). The connection survives every one.
func TestNonObjectAndWrongTypedMessages(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	for _, tc := range []struct{ payload, wantType string }{
		{`[1,2,3]`, jmap.ProblemNotRequest},
		{`"just a string"`, jmap.ProblemNotRequest},
		{`7`, jmap.ProblemNotRequest},
		{`true`, jmap.ProblemNotRequest},
		{`null`, jmap.ProblemNotRequest},
		{`{"@type":5}`, jmap.ProblemNotRequest},
		{`{"@type":["Request"]}`, jmap.ProblemNotRequest},
		{`{"@type":"Request","id":7}`, jmap.ProblemNotRequest},
		{`{"@type":null}`, jmap.ProblemNotRequest},
		{`{"@type":"Request","@type":5}`, jmap.ProblemNotRequest},
		{`{{not json`, jmap.ProblemNotJSON},
		{`{"@type":"Request"} trailing`, jmap.ProblemNotJSON},
		{``, jmap.ProblemNotJSON},
	} {
		c.send(frame.OpText, []byte(tc.payload))
		m := c.recvText(5 * time.Second)
		if m["@type"] != "RequestError" || m["type"] != tc.wantType {
			t.Errorf("%q: got %v/%v, want RequestError/%s", tc.payload, m["@type"], m["type"], tc.wantType)
		}
	}
	c.send(frame.OpText, request("alive", "Core/echo", "c9"))
	if m := c.recvText(5 * time.Second); m["requestId"] != "alive" {
		t.Fatal("connection did not survive the garbage parade")
	}
}

// Envelope member decoding follows JSON member semantics, not byte
// matching: an escaped name is the same member, a duplicated member
// keeps its last occurrence like a map decode, and an explicit null id
// is an absent id (a response to an id-less request carries no
// requestId, RFC 8887 section 4.3.2).
func TestEnvelopeMemberShapes(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)

	// \u0040 is '@': the escaped name is the @type member.
	c.send(frame.OpText, []byte(`{"\u0040type":"Request","using":["urn:ietf:params:jmap:core","urn:example:test"],"methodCalls":[["Core/echo",{},"c1"]]}`))
	if m := c.recvText(5 * time.Second); m["@type"] != "Response" || m["requestId"] != nil {
		t.Fatalf("escaped @type: %v", m)
	}

	// The duplicated id's last occurrence is the one echoed back; the
	// duplicate itself is refused downstream as I-JSON (RFC 7493
	// section 2.3), proving the envelope kept the last value.
	c.send(frame.OpText, []byte(`{"@type":"Request","id":"first","id":"last","using":[],"methodCalls":[]}`))
	if m := c.recvText(5 * time.Second); m["@type"] != "RequestError" || m["requestId"] != "last" {
		t.Fatalf("duplicate id: %v", m)
	}

	// An explicit null id is an absent id.
	c.send(frame.OpText, []byte(`{"@type":"Request","id":null,"using":["urn:ietf:params:jmap:core","urn:example:test"],"methodCalls":[["Core/echo",{},"c3"]]}`))
	if m := c.recvText(5 * time.Second); m["@type"] != "Response" || m["requestId"] != nil {
		t.Fatalf("null id: %v", m)
	}
}

// A frame header claiming an enormous payload is refused as too big
// before anything is buffered: the socket closes 1009 immediately even
// though the claimed bytes never arrive.
func TestHugeClaimedLengthCloses(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	hdr := []byte{0x81, 0x80 | 127, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 1, 2, 3, 4}
	if _, err := c.nc.Write(hdr); err != nil {
		t.Fatal(err)
	}
	op, payload, err := c.recv(5 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("op %d err %v", op, err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1009 {
		t.Fatalf("close code %d, want 1009", code)
	}
}

// A stream of tiny fragments past the fragment cap closes 1009: the
// per-frame overhead cannot be multiplied without bound.
func TestFragmentFloodCloses(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	c.sendFrame(false, frame.OpText, []byte("{"))
	for i := 0; i < MaxFragments; i++ {
		c.sendFrame(false, frame.OpContinuation, []byte(" "))
	}
	op, payload, err := c.recv(5 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("op %d err %v", op, err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1009 {
		t.Fatalf("close code %d, want 1009", code)
	}
}

// A ping flood earns nothing but its own echoes: every pong arrives in
// order and the connection still serves requests afterwards.
func TestPingFlood(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	const n = 50
	for i := 0; i < n; i++ {
		c.send(frame.OpPing, []byte{byte(i)})
	}
	for i := 0; i < n; i++ {
		op, payload, err := c.recv(5 * time.Second)
		if err != nil || op != frame.OpPong || payload[0] != byte(i) {
			t.Fatalf("pong %d: op %d payload %v err %v", i, op, payload, err)
		}
	}
	c.send(frame.OpText, request("after", "Core/echo", "c0"))
	if m := c.recvText(5 * time.Second); m["requestId"] != "after" {
		t.Fatal("connection unusable after ping flood")
	}
}

// A client Close while a request is in flight cancels the work: the
// close is answered and no data frame ever follows the Close frame
// (RFC 6455 section 5.5.1).
func TestCloseCancelsInFlight(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpText, request("slow", "Test/slow", "c0"))
	time.Sleep(100 * time.Millisecond) // let it start
	c.send(frame.OpClose, []byte{0x03, 0xe8})

	op, _, err := c.recv(5 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("first frame after Close: op %d err %v, want the close reply", op, err)
	}
	if op, payload, err := c.recv(2 * time.Second); err == nil {
		t.Fatalf("frame after the close handshake: op %d %q", op, payload)
	}
}

// Control frames interleaved inside a fragmented request are handled
// mid-message and the request still completes (RFC 6455 section 5.4).
func TestPingInsideFragmentedRequest(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	body := request("frag", "Core/echo", "c0")
	c.sendFrame(false, frame.OpText, body[:10])
	c.send(frame.OpPing, []byte("mid"))
	c.sendFrame(true, frame.OpContinuation, body[10:])

	op, payload, err := c.recv(5 * time.Second)
	if err != nil || op != frame.OpPong || string(payload) != "mid" {
		t.Fatalf("interleaved pong: op %d %q err %v", op, payload, err)
	}
	if m := c.recvText(5 * time.Second); m["requestId"] != "frag" {
		t.Fatalf("fragmented request lost: %v", m)
	}
}

// The upgrade authenticates like any HTTP request: no credentials
// means 401 with a challenge, and no upgrade happens.
func TestUnauthenticatedUpgrade(t *testing.T) {
	env := newTestServer(t)
	nc, err := net.Dial("tcp", strings.TrimPrefix(env.ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	fmt.Fprintf(nc, "GET /ws HTTP/1.1\r\n"+
		"Host: server.example.com\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Protocol: jmap\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(nc), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("status %d, WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

// Request-id edge shapes: an id at exactly the length cap is echoed,
// an empty-string id is present (not omitted) in the response, and a
// duplicate id is the client's own confusion - both responses echo it.
func TestRequestIdEdgeShapes(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)

	exact := strings.Repeat("a", MaxRequestIDLength)
	c.send(frame.OpText, request(exact, "Core/echo", "c0"))
	if m := c.recvText(5 * time.Second); m["requestId"] != exact {
		t.Fatal("exact-cap id not echoed")
	}

	c.send(frame.OpText, []byte(`{"@type":"Request","id":"","using":["urn:ietf:params:jmap:core"],"methodCalls":[]}`))
	m := c.recvText(5 * time.Second)
	if v, present := m["requestId"]; !present || v != "" {
		t.Fatalf("empty-string id must round-trip: %v", m)
	}

	c.send(frame.OpText, request("dup", "Core/echo", "c1"))
	c.send(frame.OpText, request("dup", "Core/echo", "c2"))
	first := c.recvText(5 * time.Second)
	second := c.recvText(5 * time.Second)
	if first["requestId"] != "dup" || second["requestId"] != "dup" {
		t.Fatalf("duplicate ids: %v / %v", first["requestId"], second["requestId"])
	}
}

// An empty "using" with no calls is a valid request: an empty Response
// comes back rather than any error.
func TestEmptyUsingEmptyCalls(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)
	c.send(frame.OpText, []byte(`{"@type":"Request","id":"e","using":[],"methodCalls":[]}`))
	m := c.recvText(5 * time.Second)
	if m["@type"] != "Response" || len(m["methodResponses"].([]any)) != 0 {
		t.Fatalf("empty request: %v", m)
	}
}

// A message, once started, must finish within MessageDeadline: a peer
// dripping an unfinished fragment holds the reassembly buffer and a
// pool slot, and only the deadline bounds that hold (RFC 6455 section
// 10.4). Quiet time between complete messages never counts.
func TestMessageDeadlineKillsSlowMessage(t *testing.T) {
	old := MessageDeadline
	MessageDeadline = 150 * time.Millisecond
	t.Cleanup(func() { MessageDeadline = old })
	env := newTestServer(t)
	c := dialWS(t, env)

	// Quiet gaps well past the deadline are fine: the clock only
	// starts when bytes arrive.
	for i := 0; i < 2; i++ {
		c.send(frame.OpText, []byte(`{"@type":"Request","id":"D1","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c0"]]}`))
		c.recvType("Response", 2*time.Second)
		time.Sleep(200 * time.Millisecond)
	}

	// An unfinished fragment starts the clock; the connection is
	// failed with 1008 when the message does not complete.
	c.sendFrame(false, frame.OpText, []byte(`{"@type":`))
	op, payload, err := c.recv(2 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("op %d, err %v, want close", op, err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1008 {
		t.Fatalf("close code %d, want 1008", code)
	}
}

// A failed connection sends its close code and drops TCP without
// waiting for the peer's Close reply (RFC 6455 section 7.1.7); the
// peer must see EOF well before CloseReplyDeadline.
func TestProtocolErrorClosesWithoutReplyWait(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)

	// An unmasked client frame is a protocol violation (5.1).
	c.nc.Write([]byte{0x81, 0x01, 'x'})
	op, payload, err := c.recv(2 * time.Second)
	if err != nil || op != frame.OpClose {
		t.Fatalf("op %d, err %v, want close", op, err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1002 {
		t.Fatalf("close code %d, want 1002", code)
	}
	start := time.Now()
	c.nc.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.br.ReadByte(); err == nil {
		t.Fatal("read a byte after the close frame")
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("connection lingered %v after failing", waited)
	}
}

// The revocation callback runs on the server's single revocation
// dispatcher, which must never block (see runtime.RegisterConnection):
// it must return promptly even while another writer holds the write
// mutex against a stalled peer, and still close the connection with
// 1008 once the mutex frees.
func TestRevokedCloseDoesNotBlockCaller(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	rd := frame.NewReader(bufio.NewReader(server), 1<<20, 32)
	c := newConn(&Handler{}, &auth.Identity{Username: "john@example.com"}, server, rd)

	c.wmu.Lock()
	done := make(chan struct{})
	go func() {
		c.revoked()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		c.wmu.Unlock()
		t.Fatal("revocation callback blocked behind the write mutex")
	}
	c.wmu.Unlock()

	client.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(client, hdr[:]); err != nil {
		t.Fatal(err)
	}
	if hdr[0] != 0x88 {
		t.Fatalf("opcode %#x, want a Close frame", hdr[0])
	}
	payload := make([]byte, hdr[1]&0x7f)
	if _, err := io.ReadFull(client, payload); err != nil {
		t.Fatal(err)
	}
	if code := binary.BigEndian.Uint16(payload[:2]); code != 1008 {
		t.Fatalf("close code %d, want 1008", code)
	}
	if _, err := client.Read(hdr[:1]); err == nil {
		t.Fatal("connection still open after revocation")
	}
}

// A graceful shutdown that lands mid-frame cannot recognize the peer's
// Close reply (the parse position is unknown), so it must skip the
// reply wait instead of burning CloseReplyDeadline parsing garbage.
func TestShutdownMidFrameSkipsCloseReplyWait(t *testing.T) {
	env := newTestServer(t)
	c := dialWS(t, env)

	// A masked text frame announcing 64 payload bytes, of which only
	// three ever arrive.
	c.nc.Write([]byte{0x81, 0x80 | 64, 1, 2, 3, 4, 'a', 'b', 'c'})
	time.Sleep(150 * time.Millisecond)

	start := time.Now()
	env.handler.Shutdown()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %v; the close-reply wait must be skipped mid-frame", elapsed)
	}
	op, payload, err := c.recv(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if op != frame.OpClose || binary.BigEndian.Uint16(payload[:2]) != 1001 {
		t.Fatalf("opcode %#x payload %v, want Close 1001", op, payload)
	}
}

// After a failed connection the handler returns only once every
// request goroutine has finished, so no request work outlives it -
// even for a method that ignores its context.
func TestTeardownWaitsForInFlightRequests(t *testing.T) {
	env := newTestServer(t)
	served := make(chan struct{})
	ws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.handler.ServeHTTP(w, r)
		close(served)
	}))
	defer ws.Close()

	c := dialURL(t, ws.URL)
	c.send(frame.OpText, []byte(`{"@type":"Request","id":"R1","using":["urn:ietf:params:jmap:core","urn:example:test"],"methodCalls":[["Test/hang",{},"c0"]]}`))
	select {
	case <-env.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Test/hang never started")
	}

	// An unmasked frame fails the connection while the request runs.
	c.nc.Write([]byte{0x81, 0x01, 'x'})
	select {
	case <-served:
		t.Fatal("handler returned while a request goroutine was still running")
	case <-time.After(300 * time.Millisecond):
	}

	close(env.gate)
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after the in-flight request finished")
	}
}
