package deliver

// Trace stamping tests (RFC 5321 section 4.4): the Return-Path and Received
// prefix a delivery gets, the stamp grammar, its injection resistance, and
// the stamped headers surfacing on the delivered Email through both ingest
// adapters.

import (
	"context"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var traceStampTime = time.Date(2021, 3, 4, 12, 0, 0, 0, time.UTC)

// TestTracePrefixReturnPathOnly: an envelope with no LocalName describes no
// transport hop, so only the Return-Path line is built (RFC 5321 4.4 inserts
// Return-Path at final delivery); the null reverse-path renders as <>.
func TestTracePrefixReturnPathOnly(t *testing.T) {
	got := tracePrefix(Envelope{MailFrom: "joe@example.com"}, traceStampTime)
	if got != "Return-Path: <joe@example.com>\r\n" {
		t.Fatalf("prefix = %q", got)
	}
	if got := tracePrefix(Envelope{}, traceStampTime); got != "Return-Path: <>\r\n" {
		t.Fatalf("null-sender prefix = %q", got)
	}
}

// TestTracePrefixReceivedFormat: the full stamp follows the RFC 5321 4.4
// grammar - FROM the LHLO claim with the observed address as TCP-info, BY the
// local name, WITH the registered protocol, then ";" date-time - and an IPv6
// peer uses the RFC 5321 4.1.3 IPv6 address-literal tag.
func TestTracePrefixReceivedFormat(t *testing.T) {
	env := Envelope{
		MailFrom:  "joe@example.com",
		HeloName:  "client.example",
		PeerAddr:  "192.0.2.7:4242",
		Protocol:  "LMTP",
		LocalName: "mx.example",
	}
	got := tracePrefix(env, traceStampTime)
	want := "Return-Path: <joe@example.com>\r\n" +
		"Received: from client.example ([192.0.2.7]) by mx.example with LMTP; " +
		"Thu, 4 Mar 2021 12:00:00 +0000\r\n"
	if got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}

	env.PeerAddr = "[2001:db8::1]:4242"
	if got := tracePrefix(env, traceStampTime); !strings.Contains(got, "([IPv6:2001:db8::1])") {
		t.Fatalf("IPv6 literal missing: %q", got)
	}

	// No WITH clause for a transport with no registered protocol value, and
	// a bare peer with no LHLO claim is the address literal alone.
	env.Protocol = ""
	env.HeloName = ""
	env.PeerAddr = "192.0.2.7:4242"
	if got := tracePrefix(env, traceStampTime); !strings.Contains(got, "from [192.0.2.7] by mx.example; ") {
		t.Fatalf("bare-peer stamp = %q", got)
	}
}

// TestTracePrefixInjection: a hostile LHLO name cannot terminate the stamp
// (CR/LF), open a comment (parentheses), or smuggle a new header; a control
// character in a directly-supplied reverse-path is stripped from Return-Path.
func TestTracePrefixInjection(t *testing.T) {
	env := Envelope{
		MailFrom:  "a@example.com\r\nX-Evil: 1",
		HeloName:  "evil.example\r\nX-Injected: yes (boo) end",
		PeerAddr:  "192.0.2.9:1\r\n",
		Protocol:  "LMTP",
		LocalName: "mx.example",
	}
	got := tracePrefix(env, traceStampTime)
	if strings.Count(got, "\r\n") != 2 {
		t.Fatalf("prefix grew extra lines: %q", got)
	}
	for _, bad := range []string{"\r\nX-Evil", "(boo)", "\r\nX-Injected"} {
		if strings.Contains(got, bad) {
			t.Fatalf("prefix carries %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "from evil.exampleX-Injected:yesbooend (") {
		// The sanitized claim is inert mid-line residue, not a new header
		// (a header field exists only at the start of a line, RFC 5322 2.2).
		t.Fatalf("sanitized claim unexpected: %q", got)
	}
}

// TestLMTPDeliveryStampsTrace: a message delivered over LMTP is stored and
// parsed with the stamp - the blob starts with Return-Path then a Received
// naming the LHLO claim, the peer, this server, and "with LMTP" (IANA Mail
// Transmission Types, RFC 2033) - and the stamped Received surfaces as a
// header of the delivered Email (RFC 8621 4.1.2).
func TestLMTPDeliveryStampsTrace(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	sink := &captureSink{}
	d := mustDeliverer(t, db, store, mapResolver{"jane@example.com": testAccount}, WithSink(sink))

	c, done := lmtpDial(t, d, "mx.example")
	defer done()
	wantCode(t, c, "LHLO client.example", 250)
	wantCode(t, c, "MAIL FROM:<joe@example.com>", 250)
	wantCode(t, c, "RCPT TO:<jane@example.com>", 250)
	wantCode(t, c, "DATA", 354)
	writeBody(t, c, simpleMessage)
	if code, _ := readReply(t, c); code != 250 {
		t.Fatalf("delivery reply != 250")
	}

	if len(sink.events) != 1 || sink.events[0].Outcome != mail.Accepted {
		t.Fatalf("events: %+v", sink.events)
	}
	ev := sink.events[0]
	rc, _, err := store.Open(context.Background(), testAccount, ev.BlobId)
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	stored, _ := io.ReadAll(rc)
	rc.Close()
	s := string(stored)
	if !strings.HasPrefix(s, "Return-Path: <joe@example.com>\r\nReceived: from client.example (") {
		t.Fatalf("stored prefix wrong: %q", s[:min(len(s), 120)])
	}
	if !strings.Contains(s, ") by mx.example with LMTP; ") {
		t.Fatalf("stored Received wrong: %q", s[:min(len(s), 200)])
	}
	// The dot-decoder hands Deliver the body with LF line endings; the
	// original octets follow the stamp unchanged beyond that mapping.
	if !strings.HasSuffix(s, strings.ReplaceAll(simpleMessage, "\r\n", "\n")) {
		t.Fatal("stored blob does not end with the original message")
	}

	obj := emailGet(t, ts, string(ev.EmailId), `,"properties":["header:Received:asText"]`)
	recv, _ := obj["header:Received:asText"].(string)
	if !strings.Contains(recv, "by mx.example with LMTP") {
		t.Fatalf("Email Received header = %q", recv)
	}
}

// TestHTTPIngestStampsTrace: with WithIngestHostname the HTTP adapter stamps
// a Received with the observed peer and no WITH clause (HTTP has no
// registered protocol value); without it, Return-Path only.
func TestHTTPIngestStampsTrace(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	sink := &captureSink{}
	d := mustDeliverer(t, db, store, mapResolver{"jane@example.com": testAccount}, WithSink(sink))

	deliver := func(h *HTTPIngest) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/ingest", strings.NewReader(simpleMessage))
		req.Header.Set(headerMailFrom, "joe@example.com")
		req.Header.Set(headerRcptTo, "jane@example.com")
		req.RemoteAddr = "192.0.2.5:9999"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("ingest status = %d", w.Code)
		}
		ev := sink.events[len(sink.events)-1]
		rc, _, err := store.Open(context.Background(), testAccount, ev.BlobId)
		if err != nil {
			t.Fatalf("open blob: %v", err)
		}
		stored, _ := io.ReadAll(rc)
		rc.Close()
		return string(stored)
	}

	got := deliver(NewHTTPIngest(d, WithIngestHostname("mx.example")))
	if !strings.Contains(got, "Received: from [192.0.2.5] by mx.example; ") {
		t.Fatalf("stamped ingest blob = %q", got[:min(len(got), 160)])
	}

	got = deliver(NewHTTPIngest(d))
	if strings.Contains(got, "Received:") || !strings.HasPrefix(got, "Return-Path: <joe@example.com>\r\n") {
		t.Fatalf("unstamped ingest blob = %q", got[:min(len(got), 160)])
	}
}
