package report

// Tests for MDN generation (RFC 8098 section 3): that a written MDN parses
// back to the Message it was built from, that it matches the shape of the
// RFC's own example, that the returned-original component takes the form
// the caller asked for (RFC 6522 sections 3 and 4), and that no
// caller-supplied value can break out of a header field.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// capture collects one leaf part's decoded content.
type capture struct{ b bytes.Buffer }

func (c *capture) Write(p []byte) (int, error) { return c.b.Write(p) }
func (c *capture) Close() error                { return nil }

// written is a generated MDN, parsed back: the raw octets, the parse tree,
// and each leaf part's decoded content keyed by its media type (the three
// components of an MDN all have distinct types).
type written struct {
	raw    string
	parsed *message.Message
	parts  map[string]string
}

// writeMDN generates m and parses the result.
func writeMDN(t *testing.T, m Message) written {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw := buf.String()
	caps := map[string]*capture{}
	parsed, err := message.Parse(strings.NewReader(raw), func(p *message.Part) message.LeafSinks {
		c := &capture{}
		caps[p.Type] = c
		return message.LeafSinks{Sinks: []message.Sink{c}}
	})
	if err != nil {
		t.Fatalf("parsing generated MDN: %v", err)
	}
	parts := map[string]string{}
	for typ, c := range caps {
		parts[typ] = c.b.String()
	}
	return written{raw: raw, parsed: parsed, parts: parts}
}

// sampleMDN is a fully populated Message: every optional field set, so a
// test that mutates one field starts from something that writes cleanly.
func sampleMDN() Message {
	return Message{
		From:              Address{Name: "Joe Recipient", Email: "joe@example.com"},
		To:                Address{Name: "Jane Sender", Email: "jane@example.org"},
		Subject:           "Read receipt for: quarterly report",
		TextBody:          "This receipt shows the message was displayed.",
		ReportingUA:       "mail.example.net; Naust 1.0",
		FinalRecipient:    GenericAddress{Addr: "joe@example.com"},
		OriginalRecipient: GenericAddress{Addr: "joe-alias@example.com"},
		OriginalMessageID: "orig-1@example.org",
		Disposition: Disposition{
			ActionMode:  "manual-action",
			SendingMode: "mdn-sent-manually",
			Type:        "displayed",
		},
		ExtensionFields: []ExtensionField{
			{Name: "X-Naust-Trace", Value: "abc123"},
			{Name: "X-Naust-Note", Value: "second field keeps its order"},
		},
	}
}

// originalMessage is the message an MDN reports on, header block first.
const originalMessage = "From: jane@example.org\r\n" +
	"To: joe@example.com\r\n" +
	"Subject: quarterly report\r\n" +
	"Message-ID: <orig-1@example.org>\r\n" +
	"\r\n" +
	"body text that a headers-only report must not carry\r\n"

func TestWriteRoundTrip(t *testing.T) {
	m := sampleMDN()
	m.Original = strings.NewReader(originalMessage)
	m.HeadersOnly = true
	w := writeMDN(t, m)

	// The MDN's own header block (RFC 8098 section 3).
	for _, want := range []struct{ name, value string }{
		{"From", "Joe Recipient <joe@example.com>"},
		{"To", "Jane Sender <jane@example.org>"},
		{"Subject", "Read receipt for: quarterly report"},
		{"MIME-Version", "1.0"},
		{"Auto-Submitted", "auto-replied"},
	} {
		got, ok := w.parsed.HeaderLast(want.name)
		if !ok || strings.TrimSpace(got) != want.value {
			t.Fatalf("%s = %q, want %q", want.name, got, want.value)
		}
	}
	if _, ok := w.parsed.HeaderLast("Date"); !ok {
		t.Fatal("no Date header")
	}
	// The generated Message-ID must differ from the original's (section 3).
	msgID, ok := w.parsed.HeaderLast("Message-ID")
	if !ok || strings.Contains(msgID, m.OriginalMessageID) {
		t.Fatalf("Message-ID = %q", msgID)
	}
	// An MDN never requests an MDN of its own (section 3).
	if _, ok := w.parsed.HeaderLast("Disposition-Notification-To"); ok {
		t.Fatal("generated MDN requests an MDN")
	}
	// The multipart/report container (RFC 6522 section 3).
	ct, _ := w.parsed.HeaderLast("Content-Type")
	if w.parsed.Root.Type != "multipart/report" || !strings.Contains(ct, "report-type=disposition-notification") {
		t.Fatalf("container = %q / %q", w.parsed.Root.Type, ct)
	}
	if len(w.parsed.Root.SubParts) != 3 {
		t.Fatalf("components = %d", len(w.parsed.Root.SubParts))
	}
	if w.parts["text/plain"] != m.TextBody {
		t.Fatalf("human-readable part = %q", w.parts["text/plain"])
	}

	// The machine-readable part, read back with this package's own parser.
	notif := w.parts["message/disposition-notification"]
	n := ParseDispositionNotification([]byte(notif))
	if n == nil {
		t.Fatal("generated notification did not parse")
	}
	if n.FinalRecipient != m.FinalRecipient.Addr || n.OrigMessageID != m.OriginalMessageID || n.Disposition != "displayed" {
		t.Fatalf("notification = %+v", n)
	}
	groups := ParseFieldGroups([]byte(notif))
	if len(groups) != 1 {
		t.Fatalf("notification field groups = %d", len(groups))
	}
	g := groups[0]
	for _, want := range []struct{ field, value string }{
		{"Reporting-UA", m.ReportingUA},
		{"Original-Recipient", "rfc822; " + m.OriginalRecipient.Addr},
		{"Final-Recipient", "rfc822; " + m.FinalRecipient.Addr},
		{"Original-Message-ID", "<" + m.OriginalMessageID + ">"},
		{"Disposition", "manual-action/mdn-sent-manually; displayed"},
		{"X-Naust-Trace", "abc123"},
		{"X-Naust-Note", "second field keeps its order"},
	} {
		if got := groupField(g, want.field); got != want.value {
			t.Fatalf("%s = %q, want %q", want.field, got, want.value)
		}
	}
	// Extension fields keep the order they were given (RFC 8098 3.3).
	if g[len(g)-2].name != "X-Naust-Trace" || g[len(g)-1].name != "X-Naust-Note" {
		t.Fatalf("extension order = %+v", g)
	}
	// The returned header block still names the original message.
	if id := MessageIDFromHeaderBlock([]byte(w.parts["text/rfc822-headers"])); id != m.OriginalMessageID {
		t.Fatalf("returned header block Message-ID = %q", id)
	}
}

// TestWriteRFC8098Example builds the Message corresponding to the MDN of
// RFC 8098 section 9 (a message displayed to the user of a mail user
// agent) and checks the generated notification content field by field.
// The RFC's own rendering differs only where the grammar makes it
// immaterial: it omits the optional white space after "rfc822;" (section
// 3.1.1) and spells the sending mode "MDN-sent-manually", which section
// 3.2.6 declares case insensitive and RFC 9007 section 2 fixes as
// lowercase.
func TestWriteRFC8098Example(t *testing.T) {
	m := Message{
		From:              Address{Name: "Joe Recipient", Email: "Joe_Recipient@example.com"},
		To:                Address{Name: "Jane Sender", Email: "Jane_Sender@example.org"},
		Subject:           "Disposition notification",
		TextBody:          "The message sent on 1995 Sep 19 has been displayed.",
		ReportingUA:       "joes-pc.cs.example.com; Foomail 97.1",
		OriginalRecipient: GenericAddress{Addr: "Joe_Recipient@example.com"},
		FinalRecipient:    GenericAddress{Addr: "Joe_Recipient@example.com"},
		OriginalMessageID: "199509192301.23456@example.org",
		Disposition: Disposition{
			ActionMode:  "manual-action",
			SendingMode: "mdn-sent-manually",
			Type:        "displayed",
		},
	}
	w := writeMDN(t, m)
	want := "Reporting-UA: joes-pc.cs.example.com; Foomail 97.1\r\n" +
		"Original-Recipient: rfc822; Joe_Recipient@example.com\r\n" +
		"Final-Recipient: rfc822; Joe_Recipient@example.com\r\n" +
		"Original-Message-ID: <199509192301.23456@example.org>\r\n" +
		"Disposition: manual-action/mdn-sent-manually; displayed\r\n"
	if got := w.parts["message/disposition-notification"]; got != want {
		t.Fatalf("notification content =\n%q\nwant\n%q", got, want)
	}
	// The example's report has no returned original, so two components.
	if len(w.parsed.Root.SubParts) != 2 {
		t.Fatalf("components = %d", len(w.parsed.Root.SubParts))
	}
	// The address case of the recipient is preserved (section 3.2.4).
	if !strings.Contains(w.raw, "Joe_Recipient@example.com") {
		t.Fatal("recipient address case was not preserved")
	}
}

func TestWriteReturnedOriginalForms(t *testing.T) {
	// No original: a two-component report (RFC 6522 section 3).
	w := writeMDN(t, sampleMDN())
	if len(w.parsed.Root.SubParts) != 2 {
		t.Fatalf("components without an original = %d", len(w.parsed.Root.SubParts))
	}
	if _, ok := w.parts["message/rfc822"]; ok {
		t.Fatal("a returned original appeared with none supplied")
	}

	// The whole original, as message/rfc822 (RFC 8098 section 3).
	full := sampleMDN()
	full.Original = strings.NewReader(originalMessage)
	w = writeMDN(t, full)
	if w.parsed.Root.SubParts[2].Type != "message/rfc822" {
		t.Fatalf("third component = %q", w.parsed.Root.SubParts[2].Type)
	}
	if w.parts["message/rfc822"] != originalMessage {
		t.Fatalf("returned original = %q", w.parts["message/rfc822"])
	}

	// Headers only, as text/rfc822-headers (RFC 6522 section 4): the block
	// through the blank line, and no body octet.
	headers := sampleMDN()
	headers.Original = strings.NewReader(originalMessage)
	headers.HeadersOnly = true
	w = writeMDN(t, headers)
	if w.parsed.Root.SubParts[2].Type != "text/rfc822-headers" {
		t.Fatalf("third component = %q", w.parsed.Root.SubParts[2].Type)
	}
	block, _, _ := strings.Cut(originalMessage, "\r\n\r\n")
	if got := w.parts["text/rfc822-headers"]; got != block+"\r\n\r\n" {
		t.Fatalf("returned header block = %q", got)
	}
	if strings.Contains(w.raw, "must not carry") {
		t.Fatal("headers-only report leaked body content")
	}
}

// TestWriteHeaderBlockLineEndings checks the truncation of a returned
// original whose header section ends in bare LF, and one with no body at
// all (RFC 5322 section 2.1: the block is all there is).
func TestWriteHeaderBlockLineEndings(t *testing.T) {
	for name, orig := range map[string]struct{ in, want string }{
		"bare LF":     {"A: 1\nB: 2\n\nbody\n", "A: 1\nB: 2\n\n"},
		"no body":     {"A: 1\r\nB: 2\r\n", "A: 1\r\nB: 2\r\n"},
		"blank first": {"\r\nbody\r\n", "\r\n"},
	} {
		m := sampleMDN()
		m.Original = strings.NewReader(orig.in)
		m.HeadersOnly = true
		w := writeMDN(t, m)
		if got := w.parts["text/rfc822-headers"]; got != orig.want {
			t.Fatalf("%s: header block = %q, want %q", name, got, orig.want)
		}
	}
}

func TestWriteRejectsHeaderInjection(t *testing.T) {
	mutations := map[string]func(m *Message, v string){
		"Subject":           func(m *Message, v string) { m.Subject += v },
		"From name":         func(m *Message, v string) { m.From.Name += v },
		"From email":        func(m *Message, v string) { m.From.Email += v },
		"To name":           func(m *Message, v string) { m.To.Name += v },
		"To email":          func(m *Message, v string) { m.To.Email += v },
		"ReportingUA":       func(m *Message, v string) { m.ReportingUA += v },
		"FinalRecipient":    func(m *Message, v string) { m.FinalRecipient.Addr += v },
		"OriginalRecipient": func(m *Message, v string) { m.OriginalRecipient.Addr += v },
		"OriginalMessageID": func(m *Message, v string) { m.OriginalMessageID += v },
		"extension name":    func(m *Message, v string) { m.ExtensionFields[0].Name += v },
		"extension value":   func(m *Message, v string) { m.ExtensionFields[0].Value += v },
		"action mode":       func(m *Message, v string) { m.Disposition.ActionMode += v },
		"sending mode":      func(m *Message, v string) { m.Disposition.SendingMode += v },
		"disposition type":  func(m *Message, v string) { m.Disposition.Type += v },
	}
	injections := map[string]string{
		"CRLF":      "\r\nX-Injected: yes",
		"lone CR":   "\rX-Injected: yes",
		"lone LF":   "\nX-Injected: yes",
		"NUL":       "\x00X-Injected: yes",
		"fold CRLF": "\r\n X-Injected: yes",
	}
	for field, mutate := range mutations {
		for kind, value := range injections {
			m := sampleMDN()
			mutate(&m, value)
			var buf bytes.Buffer
			if err := Write(context.Background(), &buf, m); err == nil {
				t.Fatalf("%s with %s: Write succeeded", field, kind)
			}
			// Validation runs before framing, so a rejected Message
			// produces no output at all, partial or otherwise.
			if buf.Len() != 0 {
				t.Fatalf("%s with %s: wrote %d octets", field, kind, buf.Len())
			}
		}
	}
}

// TestWriteRejectsMalformedFields covers the caller mistakes that are not
// line-break smuggling: an unusable address, a missing required field, an
// address-type label Write does not implement, a non-ASCII address (the
// RFC 6533 format is not generated), an unknown disposition value, and
// an extension field restating a defined one.
func TestWriteRejectsMalformedFields(t *testing.T) {
	cases := map[string]func(m *Message){
		"no final recipient":   func(m *Message) { m.FinalRecipient = GenericAddress{} },
		"unknown address type": func(m *Message) { m.FinalRecipient.Type = "x-panda" },
		"non-ASCII address":    func(m *Message) { m.FinalRecipient = GenericAddress{Type: "utf-8", Addr: "jäne@example.com"} },
		"prefixed address":     func(m *Message) { m.FinalRecipient.Addr = "rfc822; joe@example.com" },
		"bracketed message id": func(m *Message) { m.OriginalMessageID = "<orig-1@example.org>" },
		"domainless from":      func(m *Message) { m.From.Email = "joe" },
		"unknown action mode":  func(m *Message) { m.Disposition.ActionMode = "semi-automatic" },
		"unknown sending mode": func(m *Message) { m.Disposition.SendingMode = "mdn-sent" },
		"unknown type":         func(m *Message) { m.Disposition.Type = "read" },
		"reserved extension":   func(m *Message) { m.ExtensionFields[0].Name = "Disposition" },
		"non-ASCII UA":         func(m *Message) { m.ReportingUA = "Fo\u00f6mail" },
	}
	for name, mutate := range cases {
		m := sampleMDN()
		mutate(&m)
		var buf bytes.Buffer
		if err := Write(context.Background(), &buf, m); err == nil {
			t.Fatalf("%s: Write succeeded", name)
		} else if buf.Len() != 0 {
			t.Fatalf("%s: wrote %d octets", name, buf.Len())
		}
	}
}

// TestWriteNonASCIIHeaderText checks the one place non-ASCII is carried
// rather than rejected: the MDN's own unstructured header text and display
// names become RFC 2047 encoded-words, while the human-readable part stays
// UTF-8 under quoted-printable.
func TestWriteNonASCIIHeaderText(t *testing.T) {
	m := sampleMDN()
	// Written as escapes so the source stays ASCII; the values carry
	// non-ASCII runes.
	m.Subject = "Kvittering p\u00e5 lesing"
	m.From.Name = "J\u00f8rgen Mottaker"
	m.TextBody = "Meldingen ble vist p\u00e5 skjermen."
	w := writeMDN(t, m)
	subject, _ := w.parsed.HeaderLast("Subject")
	from, _ := w.parsed.HeaderLast("From")
	if !strings.Contains(subject, "=?utf-8?B?") || !strings.Contains(from, "=?utf-8?B?") {
		t.Fatalf("non-ASCII header text was not encoded: %q / %q", subject, from)
	}
	if w.parts["text/plain"] != m.TextBody {
		t.Fatalf("human-readable part = %q", w.parts["text/plain"])
	}
	charset := w.parsed.Root.SubParts[0].Charset
	if charset == nil || !strings.EqualFold(*charset, "utf-8") {
		t.Fatalf("human-readable charset = %v", charset)
	}
}

// TestWriteAutoSubmitted checks RFC 3834 section 5: every generated MDN
// carries exactly one Auto-Submitted: auto-replied, whatever its shape.
func TestWriteAutoSubmitted(t *testing.T) {
	minimal := Message{
		From:           Address{Email: "joe@example.com"},
		To:             Address{Email: "jane@example.org"},
		FinalRecipient: GenericAddress{Addr: "joe@example.com"},
		Disposition: Disposition{
			ActionMode:  "automatic-action",
			SendingMode: "mdn-sent-automatically",
			Type:        "deleted",
		},
	}
	full := sampleMDN()
	full.Original = strings.NewReader(originalMessage)
	headers := sampleMDN()
	headers.Original = strings.NewReader(originalMessage)
	headers.HeadersOnly = true
	for name, m := range map[string]Message{"minimal": minimal, "full": full, "headers only": headers} {
		w := writeMDN(t, m)
		got := w.parsed.HeaderInstances("Auto-Submitted")
		if len(got) != 1 || strings.TrimSpace(got[0]) != "auto-replied" {
			t.Fatalf("%s: Auto-Submitted = %q", name, got)
		}
		if n := strings.Count(w.raw, "Auto-Submitted"); n != 1 {
			t.Fatalf("%s: Auto-Submitted appears %d times", name, n)
		}
	}
}

func TestWriteCaptureBounds(t *testing.T) {
	// A generated MDN must be one this package's parser reads back: the
	// notification content and the text body are bounded by the parse
	// side's capture bound, so an over-bound Message is refused before a
	// byte is emitted rather than producing a report that fails its own
	// round-trip.
	base := Message{
		From: Address{Email: "a@example.com"}, To: Address{Email: "b@example.com"},
		FinalRecipient: GenericAddress{Addr: "a@example.com"},
		Disposition:    Disposition{ActionMode: "manual-action", SendingMode: "mdn-sent-manually", Type: "displayed"},
	}

	huge := base
	for i := 0; len(huge.ExtensionFields) < 2000; i++ {
		huge.ExtensionFields = append(huge.ExtensionFields, ExtensionField{
			Name: fmt.Sprintf("X-Ext-%d", i), Value: strings.Repeat("v", 40),
		})
	}
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, huge); err == nil || !strings.Contains(err.Error(), "notification content exceeds") {
		t.Errorf("over-bound notification: err = %v, want refusal naming the bound", err)
	}

	bigText := base
	bigText.TextBody = strings.Repeat("x", (64<<10)+1)
	buf.Reset()
	if err := Write(context.Background(), &buf, bigText); err == nil || !strings.Contains(err.Error(), "TextBody exceeds") {
		t.Errorf("over-bound TextBody: err = %v, want refusal naming the bound", err)
	}

	// Just under the bound: writes, and the parser reads it back whole.
	nearly := base
	for i := 0; i < 60; i++ {
		nearly.ExtensionFields = append(nearly.ExtensionFields, ExtensionField{
			Name: fmt.Sprintf("X-Ext-%d", i), Value: strings.Repeat("v", 900),
		})
	}
	buf.Reset()
	if err := Write(context.Background(), &buf, nearly); err != nil {
		t.Fatalf("under-bound Write: %v", err)
	}
	p, err := ParseMDN(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ParseMDN of generated MDN: %v", err)
	}
	if len(p.ExtensionFields) != 60 {
		t.Errorf("round-trip kept %d extension fields, want 60", len(p.ExtensionFields))
	}
}

// TestWriteCarriesAddressType: a stated address-type survives to the
// wire (RFC 6533 section 3 registers utf-8 alongside rfc822); an
// unstated one stays rfc822.
func TestWriteCarriesAddressType(t *testing.T) {
	m := sampleMDN()
	m.FinalRecipient = GenericAddress{Type: "utf-8", Addr: "joe@example.com"}
	m.OriginalRecipient = GenericAddress{Type: "utf-8", Addr: "joe-alias@example.com"}
	w := writeMDN(t, m)
	notif := w.parts["message/disposition-notification"]
	for _, want := range []string{
		"Final-Recipient: utf-8; joe@example.com",
		"Original-Recipient: utf-8; joe-alias@example.com",
	} {
		if !strings.Contains(notif, want) {
			t.Errorf("notification missing %q:\n%s", want, notif)
		}
	}
}

// TestGenericAddressValidateDEL: DEL (0x7f) is ASCII, so it gets the
// addr-spec refusal - the RFC 6533 pointer is only for bytes an
// internationalized address could actually contain.
func TestGenericAddressValidateDEL(t *testing.T) {
	err := GenericAddress{Addr: "jo\x7fhn@example.com"}.Validate()
	if err == nil {
		t.Fatal("DEL address accepted")
	}
	if strings.Contains(err.Error(), "6533") {
		t.Fatalf("DEL address pointed at RFC 6533: %v", err)
	}
}
