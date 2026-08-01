package deliver

// Tests for extractReport: recognizing a well-formed multipart/report (RFC
// 6522) and reducing it to a report.Inbound, including the fail-open paths
// for malformed or oversized content. The pure field/DSN/MDN parsing tests
// live in report/report_test.go.

import (
	"strings"
	"testing"

	mailparse "github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
	"github.com/naust-mail/naust-jmap/datatypes/mail/report"
)

func TestExtractReportShape(t *testing.T) {
	parse := func(raw string) *mailparse.Parsed {
		t.Helper()
		c := mailparse.NewCapture()
		c.Reports = true
		p, err := mailparse.ParseMessage(strings.NewReader(raw), c)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// A well-formed DSN extracts, with the returned content's Message-ID.
	rep := extractReport(parse(dsnFor("SUB1", "jane@remote.example", "failed", "5.1.1", "", "orig-9@example.com")))
	if rep == nil || rep.Envid != "SUB1" || rep.ReturnedMessageID != "orig-9@example.com" {
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
		// 64<<10 octets: larger than internal/parse's maxReportCapture bound,
		// so the sink marks the part over and extraction fails open.
		"Comment: " + strings.Repeat("x", 64<<10) + "\r\n" +
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
	c := mailparse.NewCapture()
	c.Reports = true
	p, err := mailparse.ParseMessage(strings.NewReader(raw), c)
	if err != nil {
		t.Fatal(err)
	}
	rep := extractReport(p)
	if rep == nil || rep.Kind != report.KindDSN {
		t.Fatal("Appendix E DSN did not extract")
	}
	if rep.Envid != "" {
		t.Fatalf("envid = %q, example has none", rep.Envid)
	}
	if len(rep.Rcpts) != 1 {
		t.Fatalf("recipients = %+v", rep.Rcpts)
	}
	r := rep.Rcpts[0]
	if r.Addr != "louisl@larry.slip.example.org" || r.Action != "failed" ||
		r.Status != "4.0.0" || r.SMTPDiag != "426 connection timed out" {
		t.Fatalf("recipient = %+v", r)
	}
	if rep.ReturnedMessageID != "original-1@example.org" {
		t.Fatalf("returned Message-ID = %q", rep.ReturnedMessageID)
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
	c := mailparse.NewCapture()
	c.Reports = true
	p, err := mailparse.ParseMessage(strings.NewReader(raw), c)
	if err != nil {
		t.Fatal(err)
	}
	rep := extractReport(p)
	if rep == nil || rep.Kind != report.KindMDN {
		t.Fatal("section 9 MDN did not extract")
	}
	if rep.OrigMessageID != "199509192301.23456@example.org" ||
		rep.FinalRecipient != "Joe_Recipient@example.com" ||
		rep.Disposition != "displayed" {
		t.Fatalf("MDN = %+v", rep)
	}
}
