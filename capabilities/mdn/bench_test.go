package mdn

// Cost guards for the two paths remote input drives. MDN/parse feeds
// arbitrary account blobs through report.ParseMDN, so its cost must
// stay linear under the MIME walker's own limits (part count, nesting
// depth, header sizes), not just on well-formed receipts. MDN/send
// assembles a report from client strings, where the largest-count
// input is the extensionFields map. The alloc-ceiling tests pin the
// linear behavior the way the report package's field-group guard does:
// a superlinear regression allocates orders of magnitude more, so the
// ceilings trip regardless of machine speed.

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/naust-mail/naust-jmap/datatypes/mail/report"
)

// hostileManyParts is a multipart/report whose tail is a flood of tiny
// parts, up against the walker's part cap: a valid MDN out front, junk
// breadth behind it.
func hostileManyParts() []byte {
	var b bytes.Buffer
	b.WriteString("From: a@example.com\r\nTo: b@example.com\r\n" +
		"Content-Type: multipart/report; report-type=disposition-notification; boundary=PP\r\n" +
		"\r\n--PP\r\nContent-Type: text/plain\r\n\r\nDisplayed.\r\n" +
		"\r\n--PP\r\nContent-Type: message/disposition-notification\r\n\r\n" +
		"Final-Recipient: rfc822; a@example.com\r\n" +
		"Disposition: manual-action/mdn-sent-manually; displayed\r\n")
	for i := 0; i < 10000; i++ {
		b.WriteString("\r\n--PP\r\nContent-Type: text/plain\r\n\r\nx\r\n")
	}
	b.WriteString("\r\n--PP--\r\n")
	return b.Bytes()
}

// hostileDeepNesting buries the third component under the walker's
// full nesting depth.
func hostileDeepNesting() []byte {
	var b bytes.Buffer
	b.WriteString("From: a@example.com\r\nTo: b@example.com\r\n" +
		"Content-Type: multipart/report; report-type=disposition-notification; boundary=D0\r\n" +
		"\r\n--D0\r\nContent-Type: text/plain\r\n\r\nDisplayed.\r\n" +
		"\r\n--D0\r\nContent-Type: message/disposition-notification\r\n\r\n" +
		"Final-Recipient: rfc822; a@example.com\r\n" +
		"Disposition: manual-action/mdn-sent-manually; displayed\r\n" +
		"\r\n--D0\r\n")
	const depth = 70
	for i := 1; i <= depth; i++ {
		fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=D%d\r\n\r\n--D%d\r\n", i, i)
	}
	b.WriteString("Content-Type: text/plain\r\n\r\ndeep\r\n")
	for i := depth; i >= 1; i-- {
		fmt.Fprintf(&b, "\r\n--D%d--\r\n", i)
	}
	b.WriteString("\r\n--D0--\r\n")
	return b.Bytes()
}

// hostileHugeNotification fills the machine part to the report capture
// bound with minimal fields, and pads every part's headers.
func hostileHugeNotification() []byte {
	var b bytes.Buffer
	b.WriteString("From: a@example.com\r\nTo: b@example.com\r\n" +
		"Content-Type: multipart/report; report-type=disposition-notification; boundary=NN\r\n" +
		"\r\n--NN\r\nContent-Type: text/plain\r\n\r\nDisplayed.\r\n" +
		"\r\n--NN\r\nContent-Type: message/disposition-notification\r\n\r\n" +
		"Final-Recipient: rfc822; a@example.com\r\n" +
		"Disposition: manual-action/mdn-sent-manually; displayed\r\n")
	for i := 0; b.Len() < 60<<10; i++ {
		fmt.Fprintf(&b, "X-Pad-%d: v\r\n", i)
	}
	b.WriteString("\r\n--NN--\r\n")
	return b.Bytes()
}

func benchParseMDN(b *testing.B, raw []byte, wantMDN bool) {
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m, err := report.ParseMDN(bytes.NewReader(raw))
		if wantMDN != (err == nil) {
			b.Fatalf("ParseMDN = %v, err %v", m, err)
		}
	}
}

func BenchmarkParseMDNRealistic(b *testing.B) {
	var buf bytes.Buffer
	err := report.Write(context.Background(), &buf, report.Message{
		From: report.Address{Email: "a@example.com"}, To: report.Address{Email: "b@example.com"},
		Subject: "Read receipt", TextBody: "Displayed.",
		FinalRecipient: report.GenericAddress{Addr: "a@example.com"}, OriginalMessageID: "id-1@example.org",
		Disposition: report.Disposition{ActionMode: "manual-action", SendingMode: "mdn-sent-manually", Type: "displayed"},
	})
	if err != nil {
		b.Fatal(err)
	}
	benchParseMDN(b, buf.Bytes(), true)
}

func BenchmarkParseMDNManyParts(b *testing.B)   { benchParseMDN(b, hostileManyParts(), true) }
func BenchmarkParseMDNDeepNesting(b *testing.B) { benchParseMDN(b, hostileDeepNesting(), true) }
func BenchmarkParseMDNHugeFields(b *testing.B)  { benchParseMDN(b, hostileHugeNotification(), true) }

// manyExtensions builds extension fields near the notification-content
// bound: the total content is capped at the parse-side capture bound,
// so the interesting cost is at the largest field count an assembly
// accepts.
func manyExtensions(n int) []report.ExtensionField {
	out := make([]report.ExtensionField, n)
	for i := range out {
		out[i] = report.ExtensionField{Name: fmt.Sprintf("X-Ext-%d", i), Value: "value"}
	}
	return out
}

func benchAssemble(b *testing.B, ext []report.ExtensionField) {
	msg := report.Message{
		From: report.Address{Email: "a@example.com"}, To: report.Address{Email: "b@example.com"},
		Subject: "Read receipt", TextBody: "Displayed.",
		FinalRecipient: report.GenericAddress{Addr: "a@example.com"}, OriginalMessageID: "id-1@example.org",
		Disposition:     report.Disposition{ActionMode: "manual-action", SendingMode: "mdn-sent-manually", Type: "displayed"},
		ExtensionFields: ext,
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := cappedBuffer{max: 50 << 20}
		if err := report.Write(context.Background(), &buf, msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAssembleRealistic(b *testing.B)      { benchAssemble(b, manyExtensions(2)) }
func BenchmarkAssembleManyExtensions(b *testing.B) { benchAssemble(b, manyExtensions(3000)) }

// TestParseMDNHostileAllocs pins linear-cost parsing of the hostile
// shapes. The measured linear implementation stays in the hundreds of
// allocations per input (part records and header fields); a
// superlinear regression multiplies that by the part or field count,
// so a generous ceiling still trips it.
func TestParseMDNHostileAllocs(t *testing.T) {
	for name, raw := range map[string][]byte{
		"manyParts":  hostileManyParts(),
		"deep":       hostileDeepNesting(),
		"hugeFields": hostileHugeNotification(),
	} {
		allocs := testing.AllocsPerRun(3, func() {
			if _, err := report.ParseMDN(bytes.NewReader(raw)); err != nil {
				t.Fatal(err)
			}
		})
		if allocs > 200_000 {
			t.Errorf("%s: %.0f allocations on %d bytes - parse cost is no longer linear", name, allocs, len(raw))
		}
	}
}

// TestAssembleManyExtensionsAllocs pins the extension-field path of
// the assembly: linear in the field count, measured at the largest
// count the notification-content bound accepts.
func TestAssembleManyExtensionsAllocs(t *testing.T) {
	msg := report.Message{
		From: report.Address{Email: "a@example.com"}, To: report.Address{Email: "b@example.com"},
		FinalRecipient:  report.GenericAddress{Addr: "a@example.com"},
		Disposition:     report.Disposition{ActionMode: "manual-action", SendingMode: "mdn-sent-manually", Type: "displayed"},
		ExtensionFields: manyExtensions(3000),
	}
	allocs := testing.AllocsPerRun(3, func() {
		buf := cappedBuffer{max: 50 << 20}
		if err := report.Write(context.Background(), &buf, msg); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 50_000 {
		t.Errorf("%.0f allocations for 3000 extension fields - assembly cost is no longer linear", allocs)
	}
}
