package mail

// LMTP adapter tests (RFC 2033): the section 4.2 example dialogue as a
// conformance test, the per-recipient replies to the final ".", and the
// command sequencing / hostile-input cases.

import (
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// lmtpDial starts a server-side connection and returns a textproto client
// with the 220 greeting already consumed.
func lmtpDial(t *testing.T, d *Deliverer, hostname string) (*textproto.Conn, func()) {
	t.Helper()
	cli, srv := net.Pipe()
	go serveLMTPConn(srv, d, hostname)
	c := textproto.NewConn(cli)
	if code, _ := readReply(t, c); code != 220 {
		t.Fatalf("greeting code = %d, want 220", code)
	}
	return c, func() { c.Close() }
}

// readReply reads one (possibly multiline) LMTP reply and returns its code
// and every line.
func readReply(t *testing.T, c *textproto.Conn) (int, []string) {
	t.Helper()
	var lines []string
	for {
		line, err := c.ReadLine()
		if err != nil {
			t.Fatalf("read reply: %v", err)
		}
		lines = append(lines, line)
		if len(line) < 4 || line[3] != '-' { // not a continuation
			code, _ := strconv.Atoi(line[:3])
			return code, lines
		}
	}
}

func cmd(t *testing.T, c *textproto.Conn, line string) int {
	t.Helper()
	if err := c.PrintfLine("%s", line); err != nil {
		t.Fatalf("send %q: %v", line, err)
	}
	code, _ := readReply(t, c)
	return code
}

func wantCode(t *testing.T, c *textproto.Conn, line string, want int) {
	t.Helper()
	if got := cmd(t, c, line); got != want {
		t.Fatalf("%q -> %d, want %d", line, got, want)
	}
}

// lmtpDeliverer builds a Deliverer over a fresh server with an inbox in
// testAccount and the given recipient->account resolver.
func lmtpDeliverer(t *testing.T, resolver mapResolver, opts ...DelivererOption) *Deliverer {
	t.Helper()
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	return NewDeliverer(db, store, resolver, opts...)
}

// TestLMTPConformanceDialogue reproduces the RFC 2033 section 4.2 example:
// LHLO advertising PIPELINING and SIZE, a mix of accepted and rejected
// RCPTs, and one reply per successful RCPT after the final ".".
func TestLMTPConformanceDialogue(t *testing.T) {
	d := lmtpDeliverer(t, mapResolver{
		"pat@foo.edu":   testAccount,
		"green@foo.edu": testAccount,
	})
	c, done := lmtpDial(t, d, "foo.edu")
	defer done()

	c.PrintfLine("LHLO foo.edu")
	code, lines := readReply(t, c)
	if code != 250 {
		t.Fatalf("LHLO code = %d, want 250", code)
	}
	if !replyHas(lines, "PIPELINING") || !replyHas(lines, "SIZE") {
		t.Fatalf("LHLO missing required extensions: %v", lines)
	}

	wantCode(t, c, "MAIL FROM:<chris@bar.com>", 250)
	wantCode(t, c, "RCPT TO:<pat@foo.edu>", 250)
	wantCode(t, c, "RCPT TO:<jones@foo.edu>", 550) // no such user
	wantCode(t, c, "RCPT TO:<green@foo.edu>", 250)
	wantCode(t, c, "DATA", 354)

	writeBody(t, c, simpleMessage)

	// One reply per successful RCPT (pat, green), in order.
	if code, _ := readReply(t, c); code != 250 {
		t.Fatalf("first DATA reply (pat) = %d, want 250", code)
	}
	if code, _ := readReply(t, c); code != 250 {
		t.Fatalf("second DATA reply (green) = %d, want 250", code)
	}
	wantCode(t, c, "QUIT", 221)
}

// TestLMTPMixedDataReplies mirrors the example's mixed outcome: two accepted
// recipients where one delivers (250) and one tempfails (451, its account
// has no inbox), each reported separately after the final ".".
func TestLMTPMixedDataReplies(t *testing.T) {
	d := lmtpDeliverer(t, mapResolver{
		"ok@foo.edu":      testAccount,
		"noinbox@foo.edu": jmap.Id("Aother"), // resolves, but no inbox
	})
	c, done := lmtpDial(t, d, "foo.edu")
	defer done()

	wantCode(t, c, "LHLO foo.edu", 250)
	wantCode(t, c, "MAIL FROM:<s@bar.com>", 250)
	wantCode(t, c, "RCPT TO:<ok@foo.edu>", 250)
	wantCode(t, c, "RCPT TO:<noinbox@foo.edu>", 250)
	wantCode(t, c, "DATA", 354)
	writeBody(t, c, simpleMessage)

	if code, _ := readReply(t, c); code != 250 {
		t.Fatalf("first reply (ok) = %d, want 250", code)
	}
	if code, _ := readReply(t, c); code != 451 {
		t.Fatalf("second reply (noinbox) = %d, want 451 tempfail", code)
	}
}

// TestLMTPSequencing covers the command-order rules: LHLO gating, HELO/EHLO
// refusal, RCPT-before-MAIL, DATA with no recipients, RSET, and unknown
// commands.
func TestLMTPSequencing(t *testing.T) {
	d := lmtpDeliverer(t, mapResolver{"a@foo.edu": testAccount})
	c, done := lmtpDial(t, d, "foo.edu")
	defer done()

	wantCode(t, c, "MAIL FROM:<x@y.com>", 503) // before LHLO
	wantCode(t, c, "HELO foo.edu", 500)        // MUST NOT accept (4.1)
	wantCode(t, c, "EHLO foo.edu", 500)        // MUST NOT accept (4.1)
	wantCode(t, c, "VRFY someone", 502)        // not supported
	wantCode(t, c, "FROB nicate", 500)         // unknown command
	wantCode(t, c, "LHLO foo.edu", 250)
	wantCode(t, c, "RCPT TO:<a@foo.edu>", 503) // RCPT before MAIL
	wantCode(t, c, "MAIL FROM:<x@y.com>", 250)
	wantCode(t, c, "DATA", 503) // no valid recipient (4.2 MUST)
	wantCode(t, c, "RSET", 250)
	wantCode(t, c, "RCPT TO:<a@foo.edu>", 503) // RSET cleared the sender
}

// TestLMTPSizeRejectedAtMail: a SIZE parameter over the limit is refused at
// MAIL, before the body is ever sent.
func TestLMTPSizeRejectedAtMail(t *testing.T) {
	d := lmtpDeliverer(t, mapResolver{"a@foo.edu": testAccount}, WithMaxMessageSize(1024))
	c, done := lmtpDial(t, d, "foo.edu")
	defer done()

	wantCode(t, c, "LHLO foo.edu", 250)
	wantCode(t, c, "MAIL FROM:<x@y.com> SIZE=99999", 552)
}

// TestLMTPDotStuffedBody: a body containing a line that begins with "." is
// delivered intact and the stream stays in sync (the next command works),
// exercising dot-stuffing on both ends.
func TestLMTPDotStuffedBody(t *testing.T) {
	d := lmtpDeliverer(t, mapResolver{"a@foo.edu": testAccount})
	c, done := lmtpDial(t, d, "foo.edu")
	defer done()

	wantCode(t, c, "LHLO foo.edu", 250)
	wantCode(t, c, "MAIL FROM:<x@y.com>", 250)
	wantCode(t, c, "RCPT TO:<a@foo.edu>", 250)
	wantCode(t, c, "DATA", 354)
	writeBody(t, c, "Subject: dotted\r\n\r\nline one\r\n.hidden dot line\r\nline three\r\n")
	if code, _ := readReply(t, c); code != 250 {
		t.Fatalf("dotted body reply = %d, want 250", code)
	}
	wantCode(t, c, "NOOP", 250) // stream still in sync
}

// TestLMTPBareLF: a command terminated with a bare LF (no CR) is accepted -
// textproto line reading tolerates both, so a lenient client still works.
func TestLMTPBareLF(t *testing.T) {
	d := lmtpDeliverer(t, mapResolver{"a@foo.edu": testAccount})
	cli, srv := net.Pipe()
	go serveLMTPConn(srv, d, "foo.edu")
	defer cli.Close()
	c := textproto.NewConn(cli)

	readReply(t, c) // greeting (the server is now waiting for a command)
	// Write LHLO with a bare LF, bypassing PrintfLine's CRLF.
	if _, err := io.WriteString(cli, "LHLO foo.edu\n"); err != nil {
		t.Fatalf("bare-LF write: %v", err)
	}
	if code, _ := readReply(t, c); code != 250 {
		t.Fatalf("bare-LF LHLO code = %d, want 250", code)
	}
}

// --- small helpers ---

func writeBody(t *testing.T, c *textproto.Conn, body string) {
	t.Helper()
	w := c.DotWriter()
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
}

func replyHas(lines []string, token string) bool {
	for _, l := range lines {
		if strings.Contains(l, token) {
			return true
		}
	}
	return false
}
