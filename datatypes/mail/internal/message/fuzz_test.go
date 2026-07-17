package message

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// checkInvariants walks a parse result and verifies the structural
// promises the rest of the mail datatype relies on.
func checkInvariants(t *testing.T, m *doc) {
	t.Helper()
	if m.Root == nil {
		t.Fatal("nil bodyStructure root")
	}
	if utf8.RuneCountInString(m.Preview) > previewMaxChars {
		t.Errorf("preview exceeds %d characters", previewMaxChars)
	}
	var walk func(p *Part)
	walk = func(p *Part) {
		isMultipart := strings.HasPrefix(p.Type, "multipart/")
		if isMultipart != (p.SubParts != nil) {
			t.Errorf("part %q type %q: subParts must be non-nil iff multipart/*", p.PartID, p.Type)
		}
		if isMultipart != (p.PartID == "") {
			t.Errorf("part type %q: partId (%q) must be empty iff multipart/*", p.Type, p.PartID)
		}
		if _, captured := m.Content[p]; isMultipart && captured {
			t.Errorf("multipart part %q carries content: the factory must see only leaves", p.Type)
		}
		if p.Size != uint64(len(m.Content[p])) {
			t.Errorf("part %q: size %d != len(content) %d", p.PartID, p.Size, len(m.Content[p]))
		}
		if !utf8.ValidString(p.Type) {
			t.Errorf("part type %q is not valid UTF-8", p.Type)
		}
		for _, h := range p.Headers {
			if !utf8.ValidString(h.Name) || !utf8.ValidString(h.Value) {
				t.Errorf("raw header %q not sanitized to valid UTF-8", h.Name)
			}
		}
		for _, sub := range p.SubParts {
			walk(sub)
		}
	}
	walk(m.Root)
}

func fuzzSeeds() []string {
	return []string{
		crlf(akMessage),
		"Subject: hi\r\n\r\nbody",
		"Subject: a\x00b\xff\r\n\r\n\xfe\xff",
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\nx\r\n--b--",
		"Content-Type: multipart/mixed\r\n\r\nno boundary",
		"Content-Type: multipart/mixed; boundary=b\r\n\r\nno delimiters at all",
		"Content-Transfer-Encoding: base64\r\n\r\n!!!not base64!!!",
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n=ZZ broken",
		"Content-Type: text/plain; charset=x-unknown\r\n\r\nhi",
		"no colon here\r\n\r\n",
		": empty name\r\n\r\n",
		" \t\r\n\r\n",
		"Content-Type: message/rfc822\r\n\r\nSubject: inner\r\n\r\ndeep",
		"Subject: =?utf-8?B?SGVsbG8=?= =?bad\r\n\r\n",
		strings.Repeat("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n", 80),
	}
}

func TestParseCorpus(t *testing.T) {
	for i, seed := range fuzzSeeds() {
		checkInvariants(t, parseDoc(t, []byte(seed)))
		_ = i
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range fuzzSeeds() {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkInvariants(t, parseDoc(t, data))
	})
}

func FuzzTextForm(f *testing.F) {
	f.Add(" =?UTF-8?Q?John_Sm=C3=AEth?=")
	f.Add("=?utf-8?B?SGVsbG8=?= =?utf-8?B?d29ybGQ=?=")
	f.Add("plain value")
	f.Fuzz(func(t *testing.T, raw string) {
		_ = TextForm(raw)
	})
}

func FuzzAddresses(f *testing.F) {
	f.Add(specAddressList)
	f.Add("a@b.c, Group: d@e.f;, (comment) <g@h.i>")
	f.Add(`"unterminated <x@`)
	f.Fuzz(func(t *testing.T, raw string) {
		for _, g := range GroupedAddressesForm(raw) {
			for _, a := range g.Addresses {
				if a.Name != nil && !utf8.ValidString(*a.Name) {
					t.Errorf("address name not valid UTF-8: %q", *a.Name)
				}
			}
		}
	})
}
