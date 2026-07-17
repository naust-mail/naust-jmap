package mail

// LMTP hardening tests (RFC 5321 size/timeout limits + the envelope grammar):
// the command-line cap, the oversize-drain close, the recipient and
// RCPT-attempt caps, the control-character envelope reject, and the
// connection-level panic backstop.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// TestValidEnvelopeAddr unit-tests the envelope address filter: control
// characters (C0 including NUL/CR/LF/HT, DEL, and C1) are rejected; ASCII
// graphics and the 8-bit UTF-8 of internationalized (EAI, RFC 6531/6532)
// addresses are allowed; the empty null-sender string is valid.
func TestValidEnvelopeAddr(t *testing.T) {
	good := []string{
		"",                        // null sender <>
		"user@example.com",        // RFC 5321 4.1.2 Dot-string local-part
		"a.b+c-d@sub.example.org", //
		`"quoted string"@x.com`,   // Quoted-string local-part
		"пример@почта.рф",         // EAI: UTF-8 code points well above 0x9f
		"üser@exämple.com",        //
	}
	for _, s := range good {
		if !validEnvelopeAddr(s) {
			t.Errorf("validEnvelopeAddr(%q) = false, want true", s)
		}
	}
	bad := []string{
		"a\x00b@x", // NUL
		"a\rb@x",   // bare CR
		"a\nb@x",   // bare LF
		"a\tb@x",   // HT (a C0 control)
		"a\x1bb@x", // ESC
		"a\x7fb@x", // DEL
		"ab@x",    // NEL (C1 control, encoded as UTF-8)
		"ab@x",    // CSI (C1 control)
	}
	for _, s := range bad {
		if validEnvelopeAddr(s) {
			t.Errorf("validEnvelopeAddr(%q) = true, want false", s)
		}
	}
}

// TestLMTPControlCharAddressRejected: MAIL/RCPT with a control character in the
// address are refused at parse time - not legal in an RFC 5321 4.1.2 path.
func TestLMTPControlCharAddressRejected(t *testing.T) {
	d := lmtpDeliverer(t, mapResolver{"a@foo.edu": testAccount})
	c, done := lmtpDial(t, d, "foo.edu")
	defer done()

	wantCode(t, c, "LHLO foo.edu", 250)
	wantCode(t, c, "MAIL FROM:<a\x00b@y.com>", 501) // NUL in reverse-path
	wantCode(t, c, "MAIL FROM:<x@y.com>", 250)
	wantCode(t, c, "RCPT TO:<a\x1bb@foo.edu>", 501) // ESC in forward-path
	wantCode(t, c, "RCPT TO:<a\x7fb@foo.edu>", 501) // DEL in forward-path
	// A legitimate UTF-8 (EAI) recipient is still accepted.
	wantCode(t, c, "RCPT TO:<a@foo.edu>", 250)
}

// TestLMTPCommandLineTooLong: a command line over maxCommandLine is refused 500
// (RFC 5321 4.5.3.1.9) and the connection closed, before an unbounded buffer
// can grow.
func TestLMTPCommandLineTooLong(t *testing.T) {
	d := lmtpDeliverer(t, mapResolver{"a@foo.edu": testAccount})
	cli, srv := net.Pipe()
	go serveLMTPConn(srv, d, "foo.edu")
	defer cli.Close()
	c := textproto.NewConn(cli)
	readReply(t, c) // greeting

	long := "LHLO " + strings.Repeat("x", maxCommandLine+64) + "\r\n"
	go io.WriteString(cli, long) // server stops reading at the cap; write in bg
	if code, _ := readReply(t, c); code != 500 {
		t.Fatalf("over-length command line -> %d, want 500", code)
	}
	if _, err := c.ReadLine(); err == nil {
		t.Fatal("want connection close after 500 line-too-long")
	}
}

// TestLMTPRecipientCap: accepted recipients are capped at maxRecipients; the
// next accepted recipient is refused 452 (RFC 5321 4.5.3.1.10), orderly - the
// earlier acceptances stand.
func TestLMTPRecipientCap(t *testing.T) {
	res := mapResolver{}
	for i := 0; i <= maxRecipients; i++ {
		res[fmt.Sprintf("r%d@foo.edu", i)] = testAccount
	}
	d := lmtpDeliverer(t, res)
	c, done := lmtpDial(t, d, "foo.edu")
	defer done()

	wantCode(t, c, "LHLO foo.edu", 250)
	wantCode(t, c, "MAIL FROM:<s@bar.com>", 250)
	for i := 0; i < maxRecipients; i++ {
		wantCode(t, c, fmt.Sprintf("RCPT TO:<r%d@foo.edu>", i), 250)
	}
	wantCode(t, c, fmt.Sprintf("RCPT TO:<r%d@foo.edu>", maxRecipients), 452)
}

// countingResolver refuses every recipient but counts the calls, to prove the
// RCPT-attempt cap gates before the resolver (no amplification).
type countingResolver struct{ n *int }

func (c countingResolver) Resolve(context.Context, string) (jmap.Id, bool) {
	*c.n++
	return "", false
}

// TestLMTPRcptAttemptCap: total RCPT commands are bounded at maxRcptAttempts
// even when every recipient is refused (so the accepted buffer never grows);
// past the cap the reply is 452 and the resolver is not consulted.
func TestLMTPRcptAttemptCap(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	var calls int
	d := NewDeliverer(db, store, countingResolver{&calls})
	cli, srv := net.Pipe()
	go serveLMTPConn(srv, d, "foo.edu")
	defer cli.Close()
	c := textproto.NewConn(cli)
	readReply(t, c) // greeting

	wantCode(t, c, "LHLO foo.edu", 250)
	wantCode(t, c, "MAIL FROM:<s@bar.com>", 250)
	for i := 0; i < maxRcptAttempts; i++ {
		if code := cmd(t, c, fmt.Sprintf("RCPT TO:<u%d@foo.edu>", i)); code != 550 {
			t.Fatalf("RCPT %d -> %d, want 550 (unknown user)", i, code)
		}
	}
	if code := cmd(t, c, "RCPT TO:<extra@foo.edu>"); code != 452 {
		t.Fatalf("over-attempt RCPT -> %d, want 452", code)
	}
	if calls != maxRcptAttempts {
		t.Fatalf("resolver called %d times, want %d (cap must gate before resolve)", calls, maxRcptAttempts)
	}
}

// TestLMTPOversizeDrainCloses: a body streamed far past the size cap is
// rejected by Deliver, and the bounded drain abandons resync and closes the
// connection rather than reading forever.
func TestLMTPOversizeDrainCloses(t *testing.T) {
	d := lmtpDeliverer(t, mapResolver{"a@foo.edu": testAccount}, WithMaxMessageSize(16))
	cli, srv := net.Pipe()
	go serveLMTPConn(srv, d, "foo.edu")
	defer cli.Close()
	c := textproto.NewConn(cli)
	readReply(t, c) // greeting

	wantCode(t, c, "LHLO foo.edu", 250)
	wantCode(t, c, "MAIL FROM:<x@y.com>", 250)
	wantCode(t, c, "RCPT TO:<a@foo.edu>", 250)
	wantCode(t, c, "DATA", 354)
	// The body far exceeds maxDrain past the cap; the write races the close, so
	// run it in the background and ignore its expected broken-pipe error.
	go func() {
		w := c.DotWriter()
		io.WriteString(w, "Subject: big\r\n\r\n"+strings.Repeat("A", maxDrain+4096)+"\r\n")
		w.Close()
	}()
	if _, err := c.ReadLine(); err == nil {
		t.Fatal("want connection close after oversize drain, got a reply")
	}
}

// TestLMTPRcptPanicBackstop: a panic at RCPT (resolve runs OUTSIDE the Deliver
// panic boundary) is caught by the connection-level backstop, which replies 421
// and closes rather than crashing the process (RFC 5321 3.8).
func TestLMTPRcptPanicBackstop(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	d := NewDeliverer(db, store, panicResolver{})
	cli, srv := net.Pipe()
	go serveLMTPConn(srv, d, "foo.edu")
	defer cli.Close()
	c := textproto.NewConn(cli)
	readReply(t, c) // greeting

	wantCode(t, c, "LHLO foo.edu", 250)
	wantCode(t, c, "MAIL FROM:<s@bar.com>", 250)
	if code := cmd(t, c, "RCPT TO:<a@foo.edu>"); code != 421 {
		t.Fatalf("panic at RCPT -> %d, want 421 backstop", code)
	}
	if _, err := c.ReadLine(); err == nil {
		t.Fatal("want connection close after 421 backstop")
	}
}
