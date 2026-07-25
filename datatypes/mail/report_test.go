package mail

// Unit tests for the report parsers (RFC 3464 / RFC 8098 / RFC 6522):
// field-group structure, folding, typed addresses, dispositions, and the
// fail-open paths for malformed or oversized content.

import (
	"strings"
	"testing"
)

func TestParseDeliveryStatusFields(t *testing.T) {
	raw := "Reporting-MTA: dns; mx.remote.example\r\n" +
		"Original-Envelope-Id: SUB123\r\n" +
		"\r\n" +
		"Original-Recipient: rfc822; jane@remote.example\r\n" +
		"Final-Recipient: rfc822; jane-alias@remote.example\r\n" +
		"Action: FAILED\r\n" +
		"Status: 5.1.1\r\n" +
		"Diagnostic-Code: smtp; 550 5.1.1 user unknown,\r\n" +
		"  mailbox disabled\r\n" +
		"\r\n" +
		"Final-Recipient: rfc822; other@remote.example\r\n" +
		"Action: delayed\r\n" +
		"Status: 4.4.1\r\n"
	rep := parseDeliveryStatus([]byte(raw))
	if rep == nil || rep.kind != reportKindDSN {
		t.Fatal("DSN did not parse")
	}
	if rep.envid != "SUB123" {
		t.Fatalf("envid = %q", rep.envid)
	}
	if len(rep.rcpts) != 2 {
		t.Fatalf("recipients = %+v", rep.rcpts)
	}
	// Final-Recipient wins over Original-Recipient; Action lowercases;
	// the folded smtp Diagnostic-Code unfolds to one line.
	r := rep.rcpts[0]
	if r.addr != "jane-alias@remote.example" || r.action != "failed" || r.status != "5.1.1" {
		t.Fatalf("recipient 0 = %+v", r)
	}
	if r.smtpDiag != "550 5.1.1 user unknown, mailbox disabled" {
		t.Fatalf("diag = %q", r.smtpDiag)
	}
	if rep.rcpts[1].action != "delayed" || rep.rcpts[1].smtpDiag != "" {
		t.Fatalf("recipient 1 = %+v", rep.rcpts[1])
	}
}

func TestParseDeliveryStatusRejects(t *testing.T) {
	// No per-recipient group at all.
	if parseDeliveryStatus([]byte("Reporting-MTA: dns; a.example\r\n")) != nil {
		t.Fatal("no-recipient DSN parsed")
	}
	// A recipient group missing its required Action (RFC 3464 section 2.3).
	if parseDeliveryStatus([]byte("Reporting-MTA: dns; a.example\r\n\r\nFinal-Recipient: rfc822; x@example.com\r\n")) != nil {
		t.Fatal("actionless DSN parsed")
	}
	// A non-email address type yields no matchable address.
	rep := parseDeliveryStatus([]byte("Reporting-MTA: dns; a.example\r\n\r\n" +
		"Final-Recipient: X-400; /PN=Foo/\r\nAction: failed\r\n"))
	if rep != nil {
		t.Fatal("unmatchable address type still produced a recipient")
	}
}

func TestParseDispositionNotification(t *testing.T) {
	raw := "Reporting-UA: mail.example.net\r\n" +
		"Final-Recipient: rfc822; jane@remote.example\r\n" +
		"Original-Message-ID: <orig-1@example.com>\r\n" +
		"Disposition: manual-action/MDN-sent-manually; Displayed/Superseded\r\n"
	rep := parseDispositionNotification([]byte(raw))
	if rep == nil || rep.kind != reportKindMDN {
		t.Fatal("MDN did not parse")
	}
	if rep.origMessageID != "orig-1@example.com" || rep.finalRecipient != "jane@remote.example" {
		t.Fatalf("MDN keys = %+v", rep)
	}
	if rep.disposition != "displayed" {
		t.Fatalf("disposition = %q", rep.disposition)
	}
	// No Original-Message-ID: nothing to correlate by, so no report.
	if parseDispositionNotification([]byte("Disposition: automatic-action/MDN-sent-automatically; deleted\r\n")) != nil {
		t.Fatal("MDN without Original-Message-ID parsed")
	}
	// No Disposition field: not a usable notification (RFC 8098 section 3.1).
	if parseDispositionNotification([]byte("Original-Message-ID: <x@example.com>\r\n")) != nil {
		t.Fatal("MDN without Disposition parsed")
	}
}

func TestExtractReportShape(t *testing.T) {
	parse := func(raw string) *parsed {
		t.Helper()
		c := newCapture()
		c.reports = true
		p, err := parseMessage(strings.NewReader(raw), c)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// A well-formed DSN extracts, with the returned content's Message-ID.
	rep := extractReport(parse(dsnFor("SUB1", "jane@remote.example", "failed", "5.1.1", "", "orig-9@example.com")))
	if rep == nil || rep.envid != "SUB1" || rep.returnedMessageID != "orig-9@example.com" {
		t.Fatalf("extract = %+v", rep)
	}
	// Not multipart/report at all: plain mail never extracts.
	if extractReport(parse(simpleMessage)) != nil {
		t.Fatal("plain message extracted as report")
	}
	// multipart/report whose second part is not a status type: no report.
	notStatus := "MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/report; report-type=delivery-status; boundary=\"b\"\r\n" +
		"\r\n--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nnot a status\r\n--b--\r\n"
	if extractReport(parse(notStatus)) != nil {
		t.Fatal("non-status second part extracted")
	}
	// An oversized machine part is left uninterpreted (fail-open).
	big := "MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/report; report-type=delivery-status; boundary=\"b\"\r\n" +
		"\r\n--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n" +
		"--b\r\nContent-Type: message/delivery-status\r\n\r\n" +
		"Reporting-MTA: dns; a.example\r\n\r\nFinal-Recipient: rfc822; x@example.com\r\nAction: failed\r\n" +
		"Comment: " + strings.Repeat("x", maxReportCapture) + "\r\n" +
		"\r\n--b--\r\n"
	if extractReport(parse(big)) != nil {
		t.Fatal("oversized machine part extracted")
	}
}

// TestParseRFC3464AppendixExample is the "Simple DSN" of RFC 3464
// Appendix E, structure preserved (lowercase content-type, a boundary
// containing "/" and ".", no space after "rfc822;", no
// Original-Envelope-Id, Action failed with a 4.x.x Status), with the
// example's hostnames rewritten to RFC-reserved names.
func TestParseRFC3464AppendixExample(t *testing.T) {
	raw := "Date: Thu, 7 Jul 1994 17:16:05 -0400\r\n" +
		"From: Mail Delivery Subsystem <MAILER-DAEMON@cs.example.edu.example>\r\n" +
		"Message-Id: <199407072116.RAA14128@cs.example.edu.example>\r\n" +
		"Subject: Returned mail: Cannot send message for 5 days\r\n" +
		"To: <owner-info-mime@cs.example.edu.example>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/report; report-type=delivery-status;\r\n" +
		"\tboundary=\"RAA14128.773615765/cs.example.edu.example\"\r\n" +
		"\r\n" +
		"--RAA14128.773615765/cs.example.edu.example\r\n" +
		"\r\n" +
		"The original message was received at Sat, 2 Jul 1994 17:10:28 -0400\r\n" +
		"from root@localhost\r\n" +
		"\r\n" +
		"--RAA14128.773615765/cs.example.edu.example\r\n" +
		"content-type: message/delivery-status\r\n" +
		"\r\n" +
		"Reporting-MTA: dns; cs.example.edu.example\r\n" +
		"\r\n" +
		"Original-Recipient: rfc822;louisl@larry.slip.example.org\r\n" +
		"Final-Recipient: rfc822;louisl@larry.slip.example.org\r\n" +
		"Action: failed\r\n" +
		"Status: 4.0.0\r\n" +
		"Diagnostic-Code: smtp; 426 connection timed out\r\n" +
		"Last-Attempt-Date: Thu, 7 Jul 1994 17:15:49 -0400\r\n" +
		"\r\n" +
		"--RAA14128.773615765/cs.example.edu.example\r\n" +
		"content-type: message/rfc822\r\n" +
		"\r\n" +
		"Message-ID: <original-1@example.org>\r\n" +
		"Subject: the original\r\n" +
		"\r\n" +
		"body\r\n" +
		"--RAA14128.773615765/cs.example.edu.example--\r\n"
	c := newCapture()
	c.reports = true
	p, err := parseMessage(strings.NewReader(raw), c)
	if err != nil {
		t.Fatal(err)
	}
	rep := extractReport(p)
	if rep == nil || rep.kind != reportKindDSN {
		t.Fatal("Appendix E DSN did not extract")
	}
	if rep.envid != "" {
		t.Fatalf("envid = %q, example has none", rep.envid)
	}
	if len(rep.rcpts) != 1 {
		t.Fatalf("recipients = %+v", rep.rcpts)
	}
	r := rep.rcpts[0]
	if r.addr != "louisl@larry.slip.example.org" || r.action != "failed" ||
		r.status != "4.0.0" || r.smtpDiag != "426 connection timed out" {
		t.Fatalf("recipient = %+v", r)
	}
	if rep.returnedMessageID != "original-1@example.org" {
		t.Fatalf("returned Message-ID = %q", rep.returnedMessageID)
	}
}

// TestParseRFC8098Example is the RFC 8098 section 9 example MDN (already
// on reserved example domains), including the Reporting-UA and the
// manual-action Disposition.
func TestParseRFC8098Example(t *testing.T) {
	raw := "Date: Wed, 20 Sep 1995 00:19:00 -0400\r\n" +
		"From: Joe Recipient <Joe_Recipient@example.com>\r\n" +
		"Message-Id: <199509200019.12345@example.com>\r\n" +
		"Subject: Disposition notification\r\n" +
		"To: Jane Sender <Jane_Sender@example.org>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/report; report-type=disposition-notification;\r\n" +
		"   boundary=\"RAA14128.773615765/example.com\"\r\n" +
		"\r\n" +
		"--RAA14128.773615765/example.com\r\n" +
		"\r\n" +
		"The message sent on 1995 Sep 19 at 13:30:00 (EDT) -0400 to Joe\r\n" +
		"Recipient <Joe_Recipient@example.com> with subject \"First draft of\r\n" +
		"report\" has been displayed.\r\n" +
		"\r\n" +
		"--RAA14128.773615765/example.com\r\n" +
		"Content-Type: message/disposition-notification\r\n" +
		"\r\n" +
		"Reporting-UA: joes-pc.cs.example.com; Foomail 97.1\r\n" +
		"Original-Recipient: rfc822;Joe_Recipient@example.com\r\n" +
		"Final-Recipient: rfc822;Joe_Recipient@example.com\r\n" +
		"Original-Message-ID: <199509192301.23456@example.org>\r\n" +
		"Disposition: manual-action/MDN-sent-manually; displayed\r\n" +
		"\r\n" +
		"--RAA14128.773615765/example.com\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"\r\n" +
		"Subject: original\r\n\r\nbody\r\n" +
		"--RAA14128.773615765/example.com--\r\n"
	c := newCapture()
	c.reports = true
	p, err := parseMessage(strings.NewReader(raw), c)
	if err != nil {
		t.Fatal(err)
	}
	rep := extractReport(p)
	if rep == nil || rep.kind != reportKindMDN {
		t.Fatal("section 9 MDN did not extract")
	}
	if rep.origMessageID != "199509192301.23456@example.org" ||
		rep.finalRecipient != "Joe_Recipient@example.com" ||
		rep.disposition != "displayed" {
		t.Fatalf("MDN = %+v", rep)
	}
}

func TestParseFieldGroupsHostile(t *testing.T) {
	// Bare LF line endings, colonless lines, and leading blank lines are
	// tolerated without producing junk fields.
	groups := parseFieldGroups([]byte("\n\nA: 1\ngarbage line\nB: 2\n\nC: 3\n"))
	if len(groups) != 2 || len(groups[0]) != 2 || groupField(groups[1], "c") != "3" {
		t.Fatalf("groups = %+v", groups)
	}
	// A continuation with no preceding field is dropped.
	if g := parseFieldGroups([]byte("  floating\nA: 1\n")); groupField(g[0], "A") != "1" {
		t.Fatalf("groups = %+v", g)
	}
}
