package report

// Unit tests for the report parsers (RFC 3464 / RFC 8098): field-group
// structure, folding, typed addresses, and dispositions.

import "testing"

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
	ds := ParseDeliveryStatus([]byte(raw))
	if ds == nil {
		t.Fatal("DSN did not parse")
	}
	if ds.Envid != "SUB123" {
		t.Fatalf("envid = %q", ds.Envid)
	}
	if len(ds.Rcpts) != 2 {
		t.Fatalf("recipients = %+v", ds.Rcpts)
	}
	// Final-Recipient wins over Original-Recipient; Action lowercases;
	// the folded smtp Diagnostic-Code unfolds to one line.
	r := ds.Rcpts[0]
	if r.Addr != "jane-alias@remote.example" || r.Action != "failed" || r.Status != "5.1.1" {
		t.Fatalf("recipient 0 = %+v", r)
	}
	if r.SMTPDiag != "550 5.1.1 user unknown, mailbox disabled" {
		t.Fatalf("diag = %q", r.SMTPDiag)
	}
	if ds.Rcpts[1].Action != "delayed" || ds.Rcpts[1].SMTPDiag != "" {
		t.Fatalf("recipient 1 = %+v", ds.Rcpts[1])
	}
}

func TestParseDeliveryStatusRejects(t *testing.T) {
	// No per-recipient group at all.
	if ParseDeliveryStatus([]byte("Reporting-MTA: dns; a.example\r\n")) != nil {
		t.Fatal("no-recipient DSN parsed")
	}
	// A recipient group missing its required Action (RFC 3464 section 2.3).
	if ParseDeliveryStatus([]byte("Reporting-MTA: dns; a.example\r\n\r\nFinal-Recipient: rfc822; x@example.com\r\n")) != nil {
		t.Fatal("actionless DSN parsed")
	}
	// A non-email address type yields no matchable address.
	ds := ParseDeliveryStatus([]byte("Reporting-MTA: dns; a.example\r\n\r\n" +
		"Final-Recipient: X-400; /PN=Foo/\r\nAction: failed\r\n"))
	if ds != nil {
		t.Fatal("unmatchable address type still produced a recipient")
	}
}

func TestParseDispositionNotification(t *testing.T) {
	raw := "Reporting-UA: mail.example.net\r\n" +
		"Final-Recipient: rfc822; jane@remote.example\r\n" +
		"Original-Message-ID: <orig-1@example.com>\r\n" +
		"Disposition: manual-action/MDN-sent-manually; Displayed/Superseded\r\n"
	n := ParseDispositionNotification([]byte(raw))
	if n == nil {
		t.Fatal("MDN did not parse")
	}
	if n.OrigMessageID != "orig-1@example.com" || n.FinalRecipient != "jane@remote.example" {
		t.Fatalf("MDN keys = %+v", n)
	}
	if n.Disposition != "displayed" {
		t.Fatalf("disposition = %q", n.Disposition)
	}
	// No Original-Message-ID: nothing to correlate by, so no report.
	if ParseDispositionNotification([]byte("Disposition: automatic-action/MDN-sent-automatically; deleted\r\n")) != nil {
		t.Fatal("MDN without Original-Message-ID parsed")
	}
	// No Disposition field: not a usable notification (RFC 8098 section 3.1).
	if ParseDispositionNotification([]byte("Original-Message-ID: <x@example.com>\r\n")) != nil {
		t.Fatal("MDN without Disposition parsed")
	}
}

func TestParseFieldGroupsHostile(t *testing.T) {
	// Bare LF line endings, colonless lines, and leading blank lines are
	// tolerated without producing junk fields.
	groups := ParseFieldGroups([]byte("\n\nA: 1\ngarbage line\nB: 2\n\nC: 3\n"))
	if len(groups) != 2 || len(groups[0]) != 2 || groupField(groups[1], "c") != "3" {
		t.Fatalf("groups = %+v", groups)
	}
	// A continuation with no preceding field is dropped.
	if g := ParseFieldGroups([]byte("  floating\nA: 1\n")); groupField(g[0], "A") != "1" {
		t.Fatalf("groups = %+v", g)
	}
}
