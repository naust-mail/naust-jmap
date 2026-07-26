package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/naust-mail/naust-jmap/capabilities/websocket/internal/frame"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// conn is one established JMAP WebSocket connection. One goroutine
// (run) owns all reads; request goroutines own their own lifetimes and
// share the write mutex.
type conn struct {
	h     *Handler
	ident *auth.Identity
	nc    net.Conn
	rd    *frame.Reader

	// ctx is the connection's lifetime: closing the socket, revoking
	// the identity, or aborting on a write failure cancels it, and
	// every in-flight request's context descends from it.
	ctx    context.Context
	cancel context.CancelFunc

	// wmu serializes frame writes; sentClose blocks data frames after a
	// Close frame has been sent (RFC 6455 section 5.5.1).
	wmu       sync.Mutex
	sentClose atomic.Bool

	// lanes bounds concurrent labeled requests (LaneCap); serial is the
	// one-at-a-time lane for requests without an id, whose responses
	// would otherwise be uncorrelatable (RFC 8887 section 4.3.2).
	lanes  chan struct{}
	serial chan struct{}

	// wg tracks in-flight request goroutines; inflight feeds the
	// busy-aware idle timeout.
	wg       sync.WaitGroup
	inflight atomic.Int64

	// draining is set by a graceful shutdown: the read loop hands the
	// connection over instead of tearing it down on its way out.
	// drainMu makes request admission and the draining flip mutually
	// exclusive, so every request is inside wg before shutdown starts
	// waiting on it - or is refused. readerDone closes when the read
	// loop has returned, so shutdown never touches the frame reader
	// while the loop still owns it.
	drainMu    sync.Mutex
	draining   atomic.Bool
	readerDone chan struct{}

	teardown sync.Once

	// pushMu/push* live in push.go (WebSocketPushEnable state).
	pushMu     sync.Mutex
	pushCancel context.CancelFunc
	pushOn     atomic.Bool
}

func newConn(h *Handler, ident *auth.Identity, nc net.Conn, rd *frame.Reader) *conn {
	ctx, cancel := context.WithCancel(context.Background())
	lanes := LaneCap
	if lanes < 1 {
		lanes = 1
	}
	return &conn{
		h:          h,
		ident:      ident,
		nc:         nc,
		rd:         rd,
		ctx:        ctx,
		cancel:     cancel,
		lanes:      make(chan struct{}, lanes),
		serial:     make(chan struct{}, 1),
		readerDone: make(chan struct{}),
	}
}

// abort tears the connection down without a closing handshake: for
// write failures and revocations there is no peer worth waiting for.
func (c *conn) abort() {
	c.teardown.Do(func() {
		c.cancel()
		c.nc.Close()
	})
}

// write sends one frame under the write mutex and deadline. Data
// frames are suppressed once a Close frame has been sent (RFC 6455
// section 5.5.1). A false return means the connection is dead.
func (c *conn) write(op byte, payload []byte) bool {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.sentClose.Load() && op != frame.OpClose {
		return false
	}
	if WriteDeadline > 0 {
		c.nc.SetWriteDeadline(time.Now().Add(WriteDeadline))
	}
	if err := frame.WriteMessage(c.nc, op, payload); err != nil {
		c.abort()
		return false
	}
	return true
}

// writeClose sends the Close frame once; later data writes are
// suppressed via sentClose.
func (c *conn) writeClose(code uint16, reason string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.sentClose.Swap(true) {
		return
	}
	if WriteDeadline > 0 {
		c.nc.SetWriteDeadline(time.Now().Add(WriteDeadline))
	}
	if err := frame.WriteClose(c.nc, code, reason); err != nil {
		c.abort()
	}
}

// closeWith performs the server-initiated closing handshake (RFC 6455
// section 7.1.1): send Close, cancel in-flight work, wait briefly for
// the peer's Close, then close the TCP connection. Read-loop paths
// only - the reader must not be running concurrently.
func (c *conn) closeWith(code uint16, reason string) {
	c.writeClose(code, reason)
	c.cancel()
	c.awaitCloseReply()
	c.teardown.Do(func() { c.nc.Close() })
}

// awaitCloseReply consumes frames until the peer's Close or an error,
// bounded by CloseReplyDeadline.
func (c *conn) awaitCloseReply() {
	c.nc.SetReadDeadline(time.Now().Add(CloseReplyDeadline))
	for {
		msg, err := c.rd.Next()
		if err != nil || msg.Opcode == frame.OpClose {
			return
		}
	}
}

// run is the read loop; it returns when the connection is finished.
func (c *conn) run() {
	// On a normal exit the loop owns the teardown; when a graceful
	// shutdown kicked it, shutdown() owns the drain and the handshake.
	defer func() {
		if !c.draining.Load() {
			c.abort()
		}
	}()
	defer close(c.readerDone)
	for {
		// Read-gating (RFC 8887 section 4.3.2's shared
		// maxConcurrentRequests): hold a slot from the server-wide pool
		// BEFORE reading the next message, so a frame is never consumed
		// that cannot be started. A full pool blocks here - the client
		// experiences TCP backpressure, never a spurious limit error.
		slot, err := c.h.srv.AcquireSlot(c.ctx, c.ident)
		if err != nil {
			return // connection canceled while gated
		}
		if c.draining.Load() {
			// A graceful shutdown began while gated: stop reading before
			// re-arming any deadline, and hand the connection over.
			slot.Release()
			return
		}

		// The idle timeout counts only truly-idle time: it is armed
		// after the gate, only when nothing is in flight, and never on a
		// connection that has push enabled - a push subscriber is doing
		// its job by sitting quiet and listening.
		if IdleTimeout > 0 && c.inflight.Load() == 0 && !c.pushOn.Load() {
			c.nc.SetReadDeadline(time.Now().Add(IdleTimeout))
		} else {
			c.nc.SetReadDeadline(time.Time{})
		}

		msg, err := c.rd.Next()
		if err != nil {
			slot.Release()
			if c.draining.Load() {
				return // a graceful shutdown owns the teardown
			}
			var pe *frame.ProtocolError
			var ne net.Error
			switch {
			case errors.As(err, &pe):
				c.closeWith(pe.Code, pe.Reason)
			case errors.As(err, &ne) && ne.Timeout():
				c.closeWith(frame.CloseNormal, "idle timeout")
			}
			return
		}

		switch msg.Opcode {
		case frame.OpPing:
			// Pong replies happen synchronously on this goroutine, so at
			// most one reply is ever pending - the coalescing RFC 6455
			// section 5.5.3 permits - and a ping flood gains nothing
			// beyond its own echo, bounded by the write deadline.
			slot.Release()
			if !c.write(frame.OpPong, msg.Payload) {
				return
			}
		case frame.OpPong:
			slot.Release() // unsolicited pongs are heartbeats (5.5.3)
		case frame.OpClose:
			slot.Release()
			// Answer the peer's Close and drop the TCP connection
			// immediately, as the server side must (5.5.1).
			c.writeClose(closeEchoCode(msg.Payload), "")
			return
		case frame.OpBinary:
			slot.Release()
			// Binary frames are not part of the jmap subprotocol; this
			// server takes RFC 8887 section 4.3.1's close option.
			c.closeWith(frame.CloseUnsupportedData, "binary frames are not part of the jmap subprotocol")
			return
		case frame.OpText:
			c.dispatch(slot, msg.Payload)
		}
	}
}

// closeEchoCode picks the status to echo in the answering Close frame
// (5.5.1: "typically echos the status code it received"); an empty
// close body echoes as normal closure.
func closeEchoCode(payload []byte) uint16 {
	if len(payload) >= 2 {
		return uint16(payload[0])<<8 | uint16(payload[1])
	}
	return frame.CloseNormal
}

// notJSONDetail matches the request-level error text of the RFC 8887
// section 4.4 example exchange.
const notJSONDetail = "The request did not parse as I-JSON."

// dispatch routes one text message (RFC 8887 section 4.3): a Request
// starts on a lane, push enable/disable adjusts push state, and
// anything else is answered with a RequestError object as section
// 4.3.1 requires - request-level errors never kill the connection.
// dispatch owns slot: it hands it to a request goroutine or releases it.
func (c *conn) dispatch(slot *runtime.RequestSlot, payload []byte) {
	var env struct {
		Type *string `json:"@type"`
		ID   *string `json:"id"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Type == nil {
		slot.Release()
		// Two distinct request-level problems (RFC 8620 section 3.6.1):
		// bytes that do not parse as JSON at all are notJSON; valid JSON
		// that is not a JMAP message object - an array, a bare value, an
		// object with a missing or non-string @type - is notRequest.
		reqErr := jmap.RequestError{Type: jmap.ProblemNotJSON, Status: 400, Detail: notJSONDetail}
		if err == nil || json.Valid(payload) {
			reqErr = jmap.RequestError{Type: jmap.ProblemNotRequest, Status: 400,
				Detail: "the message is not a JMAP WebSocket message object"}
		}
		c.writeRequestError(nil, reqErr)
		return
	}
	if env.ID != nil && len(*env.ID) > MaxRequestIDLength {
		// The id is echoed back verbatim; an oversized one is refused
		// without echoing it.
		slot.Release()
		c.writeRequestError(nil, jmap.RequestError{Type: jmap.ProblemNotRequest, Status: 400,
			Detail: "the request id exceeds the maximum length"})
		return
	}

	switch *env.Type {
	case "Request":
		// Labeled requests may run concurrently up to the lane cap;
		// id-less requests take the serial lane so responses arrive in
		// request order (section 4.3.2 correlates only by id).
		lane := c.serial
		if env.ID != nil {
			lane = c.lanes
		}
		select {
		case lane <- struct{}{}:
		case <-c.ctx.Done():
			slot.Release()
			return
		}
		// A message read just before a graceful shutdown kicked the
		// reader still lands here; once the closing handshake is under
		// way no new request may start (RFC 6455 section 7.1.1), and
		// admitting one after the drain began would race its response
		// against the Close frame.
		c.drainMu.Lock()
		if c.draining.Load() {
			c.drainMu.Unlock()
			<-lane
			slot.Release()
			return
		}
		c.inflight.Add(1)
		c.wg.Add(1)
		c.drainMu.Unlock()
		go c.process(slot, lane, env.ID, payload)
	case "WebSocketPushEnable":
		slot.Release()
		c.handlePushEnable(env.ID, payload)
	case "WebSocketPushDisable":
		slot.Release()
		c.handlePushDisable()
	default:
		slot.Release()
		c.writeRequestError(env.ID, jmap.RequestError{Type: jmap.ProblemNotRequest, Status: 400,
			Detail: "unknown @type"})
	}
}

// process runs one Request through the server's pipeline and writes
// the Response or RequestError, echoing the request id as sections
// 4.3.3 and 4.3.4 require.
func (c *conn) process(slot *runtime.RequestSlot, lane chan struct{}, id *string, payload []byte) {
	defer func() {
		slot.Release()
		<-lane
		c.inflight.Add(-1)
		c.wg.Done()
	}()
	// ParseRequest tolerates unknown members, so the @type and id
	// wrapper properties ride along inside the standard Request body.
	resp, reqErr := slot.Process(c.ctx, payload)
	if reqErr != nil {
		c.writeRequestError(id, *reqErr)
		return
	}
	c.writeResponse(id, resp)
}

// wsResponse is the Response object with the RFC 8887 section 4.3.3
// extensions: @type is always "Response"; requestId echoes the request
// id and MUST be present when the request carried one.
type wsResponse struct {
	Type            string              `json:"@type"`
	RequestID       *string             `json:"requestId,omitempty"`
	MethodResponses []jmap.Invocation   `json:"methodResponses"`
	CreatedIds      map[jmap.Id]jmap.Id `json:"createdIds,omitzero"`
	SessionState    string              `json:"sessionState"`
}

func (c *conn) writeResponse(id *string, resp *jmap.Response) {
	body, err := json.Marshal(wsResponse{
		Type:            "Response",
		RequestID:       id,
		MethodResponses: resp.MethodResponses,
		CreatedIds:      resp.CreatedIds,
		SessionState:    resp.SessionState,
	})
	if err != nil {
		c.writeRequestError(id, jmap.RequestError{Type: jmap.ProblemNotRequest, Status: 500,
			Detail: "response encoding failed"})
		return
	}
	c.write(frame.OpText, body)
}

// wsRequestError is the Problem Details object with the RFC 8887
// section 4.3.4 extensions. requestId is always emitted - null when
// the request carried no id, matching the section 4.4 example.
type wsRequestError struct {
	TypeName  string  `json:"@type"`
	RequestID *string `json:"requestId"`
	Type      string  `json:"type"`
	Status    int     `json:"status"`
	Detail    string  `json:"detail,omitzero"`
	Limit     string  `json:"limit,omitzero"`
}

func (c *conn) writeRequestError(id *string, reqErr jmap.RequestError) {
	body, err := json.Marshal(wsRequestError{
		TypeName:  "RequestError",
		RequestID: id,
		Type:      reqErr.Type,
		Status:    reqErr.Status,
		Detail:    reqErr.Detail,
		Limit:     reqErr.Limit,
	})
	if err != nil {
		c.abort()
		return
	}
	c.write(frame.OpText, body)
}

// shutdown is the graceful path (RFC 6455 section 7.1.1 with 1001
// Going Away): stop reading, let in-flight requests finish and flush
// under DrainDeadline, then run the closing handshake.
func (c *conn) shutdown() {
	c.drainMu.Lock()
	c.draining.Store(true)
	c.drainMu.Unlock()
	// Kick the read loop out of its blocking read; it sees draining and
	// leaves the teardown to us.
	c.nc.SetReadDeadline(time.Now())

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(DrainDeadline):
	}
	c.writeClose(frame.CloseGoingAway, "server shutting down")
	c.cancel()
	// The frame reader has a single owner: wait for the read loop to
	// exit before reading the peer's Close reply ourselves, re-kicking
	// in case the loop re-armed a deadline before it saw draining.
	for {
		select {
		case <-c.readerDone:
		case <-time.After(50 * time.Millisecond):
			c.nc.SetReadDeadline(time.Now())
			continue
		}
		break
	}
	c.awaitCloseReply()
	c.teardown.Do(func() { c.nc.Close() })
}
