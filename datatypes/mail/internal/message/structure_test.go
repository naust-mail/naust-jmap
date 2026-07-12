package message

import (
	"strings"
	"testing"
)

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

// akMessage is the example structure of RFC 8621 4.1.4: a text+HTML
// message that went through list software attaching a header and footer.
const akMessage = `From: sender@example.com
To: rcpt@example.com
Subject: A-K structure example
Content-Type: multipart/mixed; boundary=b1

preamble is discarded
--b1
Content-Type: text/plain
Content-Disposition: inline

A
--b1
Content-Type: multipart/mixed; boundary=b2

--b2
Content-Type: multipart/alternative; boundary=b3

--b3
Content-Type: multipart/mixed; boundary=b4

--b4
Content-Type: text/plain
Content-Disposition: inline

B
--b4
Content-Type: image/jpeg
Content-Disposition: inline

C
--b4
Content-Type: text/plain
Content-Disposition: inline

D
--b4--
--b3
Content-Type: multipart/related; boundary=b5

--b5
Content-Type: text/html

E
--b5
Content-Type: image/jpeg

F
--b5--
--b3--
--b2
Content-Type: image/jpeg
Content-Disposition: attachment

G
--b2
Content-Type: application/x-excel

H
--b2
Content-Type: message/rfc822

Subject: an attached message

J
--b2--
--b1
Content-Type: text/plain
Content-Disposition: inline

K
--b1--
epilogue is discarded
`

func partContents(parts []*Part) []string {
	var out []string
	for _, p := range parts {
		s := string(p.Decoded)
		if i := strings.IndexByte(s, '\r'); i >= 0 { // first line marker
			s = s[:i]
		}
		if strings.HasPrefix(s, "Subject:") {
			s = "J"
		}
		out = append(out, s)
	}
	return out
}

func TestAKDecomposition(t *testing.T) {
	m := Parse([]byte(crlf(akMessage)))
	if got, want := strings.Join(partContents(m.TextBody), ""), "ABCDK"; got != want {
		t.Errorf("textBody = %v, want A B C D K", partContents(m.TextBody))
	}
	if got, want := strings.Join(partContents(m.HTMLBody), ""), "AEK"; got != want {
		t.Errorf("htmlBody = %v, want A E K", partContents(m.HTMLBody))
	}
	if got, want := strings.Join(partContents(m.Attachments), ""), "CFGHJ"; got != want {
		t.Errorf("attachments = %v, want C F G H J", partContents(m.Attachments))
	}
	if !m.HasAttachment {
		t.Error("hasAttachment = false, want true (G, H, J are not inline)")
	}
	if m.Preview != "A B D K" {
		t.Errorf("preview = %q, want %q", m.Preview, "A B D K")
	}

	root := m.Root
	if root.Type != "multipart/mixed" || root.PartID != "" || root.SubParts == nil || len(root.SubParts) != 3 {
		t.Fatalf("root: type=%q partId=%q subparts=%d", root.Type, root.PartID, len(root.SubParts))
	}
	// Leaf partIds are assigned depth-first.
	a := root.SubParts[0]
	if a.PartID != "1" || a.Type != "text/plain" || string(a.Decoded) != "A" {
		t.Errorf("part A: id=%q type=%q content=%q", a.PartID, a.Type, a.Decoded)
	}
	k := root.SubParts[2]
	if k.PartID != "10" || string(k.Decoded) != "K" {
		t.Errorf("part K: id=%q content=%q", k.PartID, k.Decoded)
	}
	// message/rfc822 is a leaf: no recursion into the embedded message.
	j := root.SubParts[1].SubParts[3]
	if j.Type != "message/rfc822" || j.SubParts != nil || j.PartID != "9" {
		t.Errorf("part J: type=%q partId=%q subparts=%v", j.Type, j.PartID, j.SubParts)
	}
	if !strings.Contains(string(j.Decoded), "Subject: an attached message") {
		t.Errorf("part J content lost embedded headers: %q", j.Decoded)
	}
}

func TestSimpleMessage(t *testing.T) {
	raw := crlf(`From: a@example.com
Subject: hi

body text
`)
	m := Parse([]byte(raw))
	p := m.Root
	if p.Type != "text/plain" || p.PartID != "1" || p.SubParts != nil {
		t.Fatalf("root: type=%q partId=%q", p.Type, p.PartID)
	}
	if p.Charset == nil || *p.Charset != "us-ascii" {
		t.Errorf("implicit charset = %v, want us-ascii", p.Charset)
	}
	if string(p.Decoded) != "body text\r\n" || p.Size != 11 {
		t.Errorf("content = %q size=%d", p.Decoded, p.Size)
	}
	if len(p.Headers) != 2 || p.Headers[0].Name != "From" || p.Headers[1].Value != " hi" {
		t.Errorf("headers = %#v", p.Headers)
	}
	if m.Preview != "body text" {
		t.Errorf("preview = %q", m.Preview)
	}
	if m.HasAttachment {
		t.Error("hasAttachment on a plain message")
	}
}

func TestCharsetRules(t *testing.T) {
	m := Parse([]byte(crlf(`Content-Type: text/plain; charset=UTF-8

x`)))
	if m.Root.Charset == nil || *m.Root.Charset != "UTF-8" {
		t.Errorf("explicit charset = %v", m.Root.Charset)
	}
	m = Parse([]byte(crlf(`Content-Type: application/pdf

x`)))
	if m.Root.Charset != nil {
		t.Errorf("non-text charset = %q, want nil", *m.Root.Charset)
	}
	m = Parse([]byte(crlf(`Content-Type: text/html

x`)))
	if m.Root.Charset == nil || *m.Root.Charset != "us-ascii" {
		t.Errorf("text without charset param = %v, want us-ascii", m.Root.Charset)
	}
}

func TestTransferDecoding(t *testing.T) {
	m := Parse([]byte(crlf(`Content-Transfer-Encoding: base64

aGVsbG8g
d29ybGQ=
`)))
	if string(m.Root.Decoded) != "hello world" || m.Root.EncodingProblem {
		t.Errorf("base64: %q problem=%v", m.Root.Decoded, m.Root.EncodingProblem)
	}
	m = Parse([]byte(crlf(`Content-Transfer-Encoding: quoted-printable

caf=C3=A9 says =
hi
`)))
	if got := string(m.Root.Decoded); got != "café says hi\r\n" {
		t.Errorf("quoted-printable: %q", got)
	}
	m = Parse([]byte(crlf(`Content-Transfer-Encoding: x-braille

x`)))
	if !m.Root.EncodingProblem || string(m.Root.Decoded) != "x" {
		t.Errorf("unknown cte: %q problem=%v", m.Root.Decoded, m.Root.EncodingProblem)
	}
}

func TestPartMetadata(t *testing.T) {
	raw := crlf(`Content-Type: multipart/mixed; boundary=bb

--bb
Content-Type: image/png; name="pic.png"
Content-Id: <img1@example.com>
Content-Language: en, de
Content-Location: http://example.com/pic.png

P
--bb
Content-Type: application/pdf
Content-Disposition: attachment; filename*=UTF-8''na%C3%AFve%20plan.pdf

Q
--bb
Content-Type: application/octet-stream
Content-Disposition: attachment; filename="=?utf-8?Q?r=C3=A9sum=C3=A9.pdf?="

R
--bb--
`)
	m := Parse([]byte(raw))
	p, q, r := m.Root.SubParts[0], m.Root.SubParts[1], m.Root.SubParts[2]
	if p.Name == nil || *p.Name != "pic.png" {
		t.Errorf("name from Content-Type name param = %v", p.Name)
	}
	if p.Cid == nil || *p.Cid != "img1@example.com" {
		t.Errorf("cid = %v", p.Cid)
	}
	if len(p.Language) != 2 || p.Language[0] != "en" || p.Language[1] != "de" {
		t.Errorf("language = %v", p.Language)
	}
	if p.Location == nil || *p.Location != "http://example.com/pic.png" {
		t.Errorf("location = %v", p.Location)
	}
	if q.Disposition == nil || *q.Disposition != "attachment" {
		t.Errorf("disposition = %v", q.Disposition)
	}
	if q.Name == nil || *q.Name != "naïve plan.pdf" {
		t.Errorf("rfc2231 filename = %v", q.Name)
	}
	if r.Name == nil || *r.Name != "résumé.pdf" {
		t.Errorf("rfc2047 filename = %v", r.Name)
	}
}

func TestMalformedInput(t *testing.T) {
	// Missing boundary parameter: multipart with no children.
	m := Parse([]byte(crlf(`Content-Type: multipart/mixed

whatever`)))
	if m.Root.SubParts == nil || len(m.Root.SubParts) != 0 || m.Root.PartID != "" {
		t.Errorf("boundary-less multipart: subparts=%v partId=%q", m.Root.SubParts, m.Root.PartID)
	}
	// Header line without a colon ends the header block.
	m = Parse([]byte(crlf(`Subject: ok
this line has no colon
body`)))
	if len(m.Headers) != 1 || m.Headers[0].Name != "Subject" {
		t.Errorf("headers = %#v", m.Headers)
	}
	if !strings.HasPrefix(string(m.Root.Decoded), "this line has no colon") {
		t.Errorf("body = %q", m.Root.Decoded)
	}
	// NUL dropped and invalid UTF-8 replaced in raw header values.
	m = Parse([]byte("Subject: a\x00b\xff\r\n\r\n"))
	if v, _ := m.HeaderLast("subject"); v != " ab�" {
		t.Errorf("sanitized raw = %q", v)
	}
	// Bare LF line endings parse the same as CRLF.
	m = Parse([]byte("Subject: lf\n\nbody\n"))
	if v, _ := m.HeaderLast("Subject"); v != " lf" {
		t.Errorf("bare-lf subject = %q", v)
	}
	if string(m.Root.Decoded) != "body\n" {
		t.Errorf("bare-lf body = %q", m.Root.Decoded)
	}
	// Empty input.
	m = Parse(nil)
	if m.Root == nil || m.Root.Type != "text/plain" || m.Preview != "" {
		t.Errorf("empty message: %#v", m.Root)
	}
	// A folded header continues across lines.
	m = Parse([]byte(crlf(`Subject: part one
 part two

`)))
	if v, _ := m.HeaderLast("Subject"); v != " part one\r\n part two" {
		t.Errorf("folded raw = %q", v)
	}
}

func TestHeaderInstances(t *testing.T) {
	m := Parse([]byte(crlf(`Received: one
received: two
Subject: s

`)))
	if got := m.HeaderInstances("RECEIVED"); len(got) != 2 || got[0] != " one" || got[1] != " two" {
		t.Errorf("instances = %#v", got)
	}
	if v, ok := m.HeaderLast("Received"); !ok || v != " two" {
		t.Errorf("last = %q ok=%v", v, ok)
	}
	if _, ok := m.HeaderLast("Missing"); ok {
		t.Error("found a missing header")
	}
}

func TestDigestDefaultType(t *testing.T) {
	m := Parse([]byte(crlf(`Content-Type: multipart/digest; boundary=dd

--dd

Subject: embedded

hi
--dd--
`)))
	if got := m.Root.SubParts[0].Type; got != "message/rfc822" {
		t.Errorf("digest child default type = %q, want message/rfc822", got)
	}
}

func TestBlobDedupHash(t *testing.T) {
	// Same decoded octets under different transfer encodings hash the
	// same, which is what gives them the same blob id (RFC 8621 4.1.4).
	m1 := Parse([]byte(crlf(`Content-Transfer-Encoding: base64

aGVsbG8=`)))
	m2 := Parse([]byte(crlf(`Content-Transfer-Encoding: 7bit

hello`)))
	if m1.Root.SHA256 != m2.Root.SHA256 {
		t.Error("identical decoded content produced different hashes")
	}
}
