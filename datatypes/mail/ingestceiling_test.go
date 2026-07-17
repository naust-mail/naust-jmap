package mail

// What bounds an ingest is how many connections it is serving, not how many
// messages it is parsing: a delivery streams, so a sender in flight costs a
// buffer rather than a message. A cap on parses would also be the wrong shape -
// held across a network read, a few slow senders could take every slot and stall
// delivery for everyone - so these tests hold the adapters to the ceiling they do
// have, and to answering rather than dropping the sender who meets it.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLMTPConnectionCeiling: a connection beyond the limit is answered 421 and
// closed (RFC 5321 section 3.8), so the MTA queues and retries rather than being
// dropped or served without room for it.
func TestLMTPConnectionCeiling(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	d := NewDeliverer(db, store, mapResolver{"jane@example.com": testAccount})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go ServeLMTP(ln, d, "test.example", WithMaxLMTPConnections(1))

	// The first connection takes the only slot, and keeps it: it is a sender in
	// the middle of a session, which is exactly what a busy server is full of.
	first := dialLMTP(t, ln.Addr().String())
	defer first.Close()
	if line := readLMTPLine(t, first); !strings.HasPrefix(line, "220") {
		t.Fatalf("greeting on the first connection = %q, want a 220", line)
	}

	// The second meets the ceiling.
	second := dialLMTP(t, ln.Addr().String())
	defer second.Close()
	line := readLMTPLine(t, second)
	if !strings.HasPrefix(line, "421") {
		t.Errorf("reply on the connection over the ceiling = %q, want a 421", line)
	}
	// And it is closed, not left open holding resources.
	second.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := bufio.NewReader(second).ReadString('\n'); err == nil {
		t.Error("the refused connection was left open")
	}

	// The slot comes back when its connection ends, so the ceiling is a ceiling
	// and not a one-way ratchet.
	first.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		third := dialLMTP(t, ln.Addr().String())
		line := readLMTPLine(t, third)
		third.Close()
		if strings.HasPrefix(line, "220") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slot was never released: a new connection still gets %q", line)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func dialLMTP(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn
}

func readLMTPLine(t *testing.T, conn net.Conn) string {
	t.Helper()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && line == "" {
		t.Fatalf("read reply: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}

// TestHTTPIngestRecipientCeiling: one message is one message, but one recipient
// is one delivery, so the recipient count is the amplification factor of an
// ingest and is capped on every path into it - the LMTP adapter caps it per
// transaction (RFC 5321 section 4.5.3.1.8 requires room for at least 100), and
// so must this one, where the whole list arrives in a single header.
func TestHTTPIngestRecipientCeiling(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	h := NewHTTPIngest(NewDeliverer(db, store, mapResolver{"jane@example.com": testAccount}))

	many := make([]string, maxRecipients+1)
	for i := range many {
		many[i] = "jane@example.com"
	}
	rec := postIngest(t, h, "joe@example.com", many, simpleMessage)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status for %d recipients = %d, want 400", len(many), rec.Code)
	}

	// The cap is a ceiling, not a lockout: a request at the limit is served.
	rec = postIngest(t, h, "joe@example.com", many[:maxRecipients], simpleMessage)
	if rec.Code != http.StatusOK {
		t.Errorf("status for %d recipients = %d, want 200", maxRecipients, rec.Code)
	}
}

// TestHTTPIngestCeiling: a request beyond the in-flight limit is answered 503
// with a Retry-After rather than served, and the slot is released when the
// request that held it finishes.
func TestHTTPIngestCeiling(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	h := NewHTTPIngest(
		NewDeliverer(db, store, mapResolver{"jane@example.com": testAccount}),
		WithMaxIngestInFlight(1),
	)

	// A sender whose message is still arriving holds the only slot. The write
	// below returns once the handler has read it, which is proof the request is
	// in flight rather than merely started.
	body, sender := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/ingest", body)
		req.Header.Set(headerMailFrom, "joe@example.com")
		req.Header.Set(headerRcptTo, "jane@example.com")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	if _, err := sender.Write([]byte("Subject: slow\r\n")); err != nil {
		t.Fatal(err)
	}

	rec := postIngest(t, h, "joe@example.com", []string{"jane@example.com"}, simpleMessage)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status over the ceiling = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After: a sender told to come back must be told when")
	}

	// The slow sender finishes; its slot returns, and the next request is served.
	sender.Close()
	<-done
	if rec := postIngest(t, h, "joe@example.com", []string{"jane@example.com"}, simpleMessage); rec.Code != http.StatusOK {
		t.Errorf("status once the slot was released = %d, want 200", rec.Code)
	}
}
