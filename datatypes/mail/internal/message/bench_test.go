package message

import (
	"encoding/base64"
	"strings"
	"testing"
)

// syntheticEmail builds a realistic multipart/mixed message: a plain+html
// alternative body plus an attachment of the given raw size (base64-inflated
// on the wire, as a real MUA would send it).
func syntheticEmail(attachmentBytes int) string {
	raw := make([]byte, attachmentBytes)
	for i := range raw {
		raw[i] = byte(i)
	}
	att := base64.StdEncoding.EncodeToString(raw)
	var b strings.Builder
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("From: Alice <alice@example.com>\r\n")
	b.WriteString("To: Bob <bob@example.com>\r\n")
	b.WriteString("Subject: Benchmark message\r\n")
	b.WriteString("Date: Mon, 21 Jul 2026 12:00:00 +0000\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=outer\r\n\r\n")
	b.WriteString("--outer\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=inner\r\n\r\n")
	b.WriteString("--inner\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit.\r\n", 40))
	b.WriteString("--inner\r\n")
	b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
	b.WriteString("<html><body>" + strings.Repeat("<p>Lorem ipsum dolor sit amet.</p>", 40) + "</body></html>\r\n")
	b.WriteString("--inner--\r\n")
	b.WriteString("--outer\r\n")
	b.WriteString("Content-Type: application/octet-stream; name=file.bin\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("Content-Disposition: attachment; filename=file.bin\r\n\r\n")
	for len(att) > 0 {
		n := 76
		if n > len(att) {
			n = len(att)
		}
		b.WriteString(att[:n])
		b.WriteString("\r\n")
		att = att[n:]
	}
	b.WriteString("--outer--\r\n")
	return b.String()
}

// structureOnlyFactory requests no content sinks (delivery's
// hasAttachment/threading path).
func structureOnlyFactory(*Part) LeafSinks { return LeafSinks{} }

// digestFactory requests Identity for every leaf (import/delivery computing
// Digest+Size), the most expensive realistic sink combination.
func digestFactory(*Part) LeafSinks {
	return LeafSinks{Identity: true}
}

func BenchmarkParseStructureOnly_Small(b *testing.B) {
	msg := rfc2049Example
	b.ReportAllocs()
	b.SetBytes(int64(len(msg)))
	for i := 0; i < b.N; i++ {
		if _, err := Parse(strings.NewReader(msg), structureOnlyFactory); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseStructureOnly_1MBAttachment(b *testing.B) {
	msg := syntheticEmail(1 << 20)
	b.ReportAllocs()
	b.SetBytes(int64(len(msg)))
	for i := 0; i < b.N; i++ {
		if _, err := Parse(strings.NewReader(msg), structureOnlyFactory); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseWithDigest_1MBAttachment(b *testing.B) {
	msg := syntheticEmail(1 << 20)
	b.ReportAllocs()
	b.SetBytes(int64(len(msg)))
	for i := 0; i < b.N; i++ {
		if _, err := Parse(strings.NewReader(msg), digestFactory); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAddressesForm(b *testing.B) {
	raw := "Alice <alice@example.com>, \"Bob Jones\" <bob@example.com>, carol@example.com, \"Team, Support\" <support@example.com>"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		AddressesForm(raw)
	}
}

func BenchmarkTextForm(b *testing.B) {
	raw := "=?ISO-8859-1?Q?Keld_J=F8rn_Simonsen?= plain trailing text " + strings.Repeat("word ", 30)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		TextForm(raw)
	}
}
