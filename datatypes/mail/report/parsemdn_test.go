package report

// Tests for MDN parsing (RFC 8098 section 3): that a written MDN parses
// back to the values it was built from, that recognition is strict about
// the multipart/report shape and the closed Disposition vocabularies, and
// that hostile input (truncation, wrong types, garbage) yields ErrNotMDN
// rather than a misread.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// parseWritten generates m with Write and parses it back with ParseMDN.
func parseWritten(t *testing.T, m Message) *ParsedMDN {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	p, err := ParseMDN(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ParseMDN on generated MDN: %v", err)
	}
	return p
}

func TestParseMDNRoundTrip(t *testing.T) {
	m := sampleMDN()
	p := parseWritten(t, m)
	if p.Subject != m.Subject {
		t.Errorf("Subject = %q, want %q", p.Subject, m.Subject)
	}
	if strings.TrimSpace(p.TextBody) != m.TextBody {
		t.Errorf("TextBody = %q, want %q", p.TextBody, m.TextBody)
	}
	if p.ReportingUA != m.ReportingUA {
		t.Errorf("ReportingUA = %q, want %q", p.ReportingUA, m.ReportingUA)
	}
	// Write supplies the rfc822 address-type prefix (RFC 8098 sections
	// 3.2.3-3.2.4); the parse keeps the field value whole.
	if want := "rfc822; " + m.FinalRecipient; p.FinalRecipient != want {
		t.Errorf("FinalRecipient = %q, want %q", p.FinalRecipient, want)
	}
	if want := "rfc822; " + m.OriginalRecipient; p.OriginalRecipient != want {
		t.Errorf("OriginalRecipient = %q, want %q", p.OriginalRecipient, want)
	}
	if p.OriginalMessageID != m.OriginalMessageID {
		t.Errorf("OriginalMessageID = %q, want %q", p.OriginalMessageID, m.OriginalMessageID)
	}
	if p.Disposition != m.Disposition {
		t.Errorf("Disposition = %+v, want %+v", p.Disposition, m.Disposition)
	}
	if len(p.ExtensionFields) != len(m.ExtensionFields) {
		t.Fatalf("ExtensionFields = %+v, want %+v", p.ExtensionFields, m.ExtensionFields)
	}
	for i := range m.ExtensionFields {
		if p.ExtensionFields[i] != m.ExtensionFields[i] {
			t.Errorf("ExtensionFields[%d] = %+v, want %+v", i, p.ExtensionFields[i], m.ExtensionFields[i])
		}
	}
	if p.HasOriginal {
		t.Error("HasOriginal = true for a two-part MDN")
	}
}

func TestParseMDNHasOriginal(t *testing.T) {
	m := sampleMDN()
	m.Original = strings.NewReader("Message-ID: <orig-1@example.org>\r\nSubject: original\r\n\r\nbody\r\n")
	p := parseWritten(t, m)
	if !p.HasOriginal {
		t.Error("HasOriginal = false for a three-part MDN")
	}
}

func TestParseMDNMinimal(t *testing.T) {
	// Every optional field absent (RFC 8098 section 3.1: only
	// Final-Recipient and Disposition are required).
	m := Message{
		From:           Address{Email: "joe@example.com"},
		To:             Address{Email: "jane@example.org"},
		FinalRecipient: "joe@example.com",
		Disposition: Disposition{
			ActionMode:  "automatic-action",
			SendingMode: "mdn-sent-automatically",
			Type:        "deleted",
		},
	}
	p := parseWritten(t, m)
	if p.ReportingUA != "" || p.MDNGateway != "" || p.OriginalRecipient != "" || p.OriginalMessageID != "" {
		t.Errorf("optional fields not empty: %+v", p)
	}
	if len(p.Errors) != 0 || len(p.ExtensionFields) != 0 {
		t.Errorf("Errors/ExtensionFields not empty: %+v", p)
	}
	if p.Disposition != m.Disposition {
		t.Errorf("Disposition = %+v, want %+v", p.Disposition, m.Disposition)
	}
}

// wireMDN assembles a raw MDN message directly, so tests can produce
// shapes Write refuses to generate.
func wireMDN(notification string) string {
	return "From: joe@example.com\r\n" +
		"To: jane@example.org\r\n" +
		"Subject: =?utf-8?q?L=C3=A4sekvitto?=\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/report; report-type=disposition-notification;\r\n" +
		"\tboundary=\"bb\"\r\n" +
		"\r\n" +
		"--bb\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"The message was displayed.\r\n" +
		"--bb\r\n" +
		"Content-Type: message/disposition-notification\r\n" +
		"\r\n" +
		notification +
		"--bb--\r\n"
}

func TestParseMDNWire(t *testing.T) {
	p, err := ParseMDN(strings.NewReader(wireMDN(
		"Reporting-UA: joes-pc.cs.example.com; Foomail 97.1\r\n" +
			"MDN-Gateway: smtp; mail.example.net\r\n" +
			"Original-Recipient: rfc822; joe-alias@example.com\r\n" +
			"Final-Recipient: RFC822; joe@example.com\r\n" +
			"Original-Message-ID: <199509192301.23456@example.org>\r\n" +
			"Disposition: MANUAL-ACTION/MDN-Sent-Manually; Displayed/error\r\n" +
			"Error: something odd happened\r\n" +
			"Error: and then something else\r\n" +
			"X-Vendor-Field: opaque\r\n")))
	if err != nil {
		t.Fatalf("ParseMDN: %v", err)
	}
	if p.Subject != "L\u00e4sekvitto" {
		t.Errorf("Subject = %q, want decoded encoded-word", p.Subject)
	}
	if strings.TrimSpace(p.TextBody) != "The message was displayed." {
		t.Errorf("TextBody = %q", p.TextBody)
	}
	if p.MDNGateway != "smtp; mail.example.net" {
		t.Errorf("MDNGateway = %q", p.MDNGateway)
	}
	// Mixed-case grammar words are lowercased (RFC 8098 section 3.2.6 is
	// case-insensitive on the wire; RFC 9007 section 2 mandates lowercase).
	want := Disposition{ActionMode: "manual-action", SendingMode: "mdn-sent-manually", Type: "displayed"}
	if p.Disposition != want {
		t.Errorf("Disposition = %+v, want %+v", p.Disposition, want)
	}
	if p.FinalRecipient != "RFC822; joe@example.com" {
		t.Errorf("FinalRecipient = %q", p.FinalRecipient)
	}
	if p.OriginalMessageID != "199509192301.23456@example.org" {
		t.Errorf("OriginalMessageID = %q, want bare id", p.OriginalMessageID)
	}
	if len(p.Errors) != 2 || p.Errors[0] != "something odd happened" || p.Errors[1] != "and then something else" {
		t.Errorf("Errors = %+v", p.Errors)
	}
	if len(p.ExtensionFields) != 1 || p.ExtensionFields[0] != (ExtensionField{Name: "X-Vendor-Field", Value: "opaque"}) {
		t.Errorf("ExtensionFields = %+v", p.ExtensionFields)
	}
	if p.HasOriginal {
		t.Error("HasOriginal = true for a two-part MDN")
	}
}

func TestParseMDNRejects(t *testing.T) {
	valid := "Final-Recipient: rfc822; joe@example.com\r\n" +
		"Disposition: manual-action/MDN-sent-manually; displayed\r\n"
	cases := []struct {
		name string
		raw  string
	}{
		{"not multipart", "From: joe@example.com\r\nContent-Type: text/plain\r\n\r\nhello\r\n"},
		{"wrong machine part type", strings.Replace(wireMDN(valid), "message/disposition-notification", "message/delivery-status", 1)},
		{"empty notification", wireMDN("")},
		{"missing final recipient", wireMDN("Disposition: manual-action/mdn-sent-manually; displayed\r\n")},
		{"missing disposition", wireMDN("Final-Recipient: rfc822; joe@example.com\r\n")},
		{"disposition without mode", wireMDN("Final-Recipient: rfc822; joe@example.com\r\nDisposition: displayed\r\n")},
		{"disposition type outside the closed list", wireMDN("Final-Recipient: rfc822; joe@example.com\r\nDisposition: manual-action/mdn-sent-manually; forwarded\r\n")},
		{"action mode outside the closed list", wireMDN("Final-Recipient: rfc822; joe@example.com\r\nDisposition: robot-action/mdn-sent-manually; displayed\r\n")},
		{"machine part over the capture bound", wireMDN("Final-Recipient: rfc822; joe@example.com\r\nX-Pad: " + strings.Repeat("a", 70<<10) + "\r\n" + "Disposition: manual-action/mdn-sent-manually; displayed\r\n")},
		{"garbage", "\x00\xff\xfe not a message at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParseMDN(strings.NewReader(tc.raw))
			if !errors.Is(err, ErrNotMDN) {
				t.Fatalf("ParseMDN = (%+v, %v), want ErrNotMDN", p, err)
			}
		})
	}
}

func TestParseMDNThirdPartContentUnread(t *testing.T) {
	// The returned original in the third component is structure only: a
	// text/plain original must not leak into TextBody (RFC 6522 section 3:
	// the first component is the human-readable one).
	raw := strings.Replace(wireMDN(
		"Final-Recipient: rfc822; joe@example.com\r\n"+
			"Disposition: manual-action/mdn-sent-manually; displayed\r\n"),
		"--bb--\r\n",
		"--bb\r\nContent-Type: message/rfc822\r\n\r\n"+
			"Subject: original\r\nContent-Type: text/plain\r\n\r\nsecret original body\r\n"+
			"--bb--\r\n", 1)
	p, err := ParseMDN(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseMDN: %v", err)
	}
	if !p.HasOriginal {
		t.Error("HasOriginal = false with a third component present")
	}
	if strings.Contains(p.TextBody, "secret original body") {
		t.Errorf("third-part content leaked into TextBody: %q", p.TextBody)
	}
}
