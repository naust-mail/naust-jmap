package message

// Tests for the write side: the form serializers round-trip through their
// parse-side twins (the RFC 8621 4.1.2 forms), and the streaming writer
// produces exactly the RFC 2046 framing.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestEncodeTextRoundTrip(t *testing.T) {
	cases := []string{
		"",
		"hello world",
		"Grüße: Statusbericht 42",
		"두통 보고서",                    // fully non-ASCII
		"mixed ascii und Grüße too", // forces the whole value into encoded-words
		"double  space and edge ",   // fold-hostile white space stays one atom
		strings.Repeat("Ål ", 60),   // long enough to need several encoded-words and folds
	}
	for _, want := range cases {
		value := FoldValue("Subject", EncodeText(want))
		if got := TextForm(value); got != strings.TrimLeft(want, " \t") {
			t.Errorf("EncodeText(%q): decoded %q", want, got)
		}
		for _, line := range strings.Split("Subject: "+value, "\r\n") {
			if len(line) > 998 {
				t.Errorf("EncodeText(%q): line over the hard limit: %q", want, line)
			}
		}
	}
}

func TestFormatAddressesRoundTrip(t *testing.T) {
	name := func(s string) *string { return &s }
	cases := [][]Address{
		{{Name: nil, Email: "a@example.com"}},
		{{Name: name("John Doe"), Email: "john@example.com"}},
		{{Name: name("Doe, John (Ops)"), Email: "john@example.com"}}, // specials need quoting
		{{Name: name("Jøhn Døe"), Email: "john@example.com"}},        // encoded-word
		{{Name: nil, Email: `"odd local"@example.com`}},              // quoted local part
		{
			{Name: name("A"), Email: "a@example.com"},
			{Name: nil, Email: "b@example.com"},
			{Name: name("Cédric"), Email: "c@example.com"},
		},
	}
	for _, want := range cases {
		atoms, ok := FormatAddresses(want)
		if !ok {
			t.Fatalf("FormatAddresses(%v) not representable", want)
		}
		got := AddressesForm(FoldValue("To", atoms))
		if len(got) != len(want) {
			t.Fatalf("FormatAddresses(%v): parsed back %v", want, got)
		}
		for i := range want {
			if got[i].Email != want[i].Email {
				t.Errorf("address %d: email %q, want %q", i, got[i].Email, want[i].Email)
			}
			switch {
			case want[i].Name == nil && got[i].Name != nil:
				t.Errorf("address %d: name %q, want none", i, *got[i].Name)
			case want[i].Name != nil && (got[i].Name == nil || *got[i].Name != *want[i].Name):
				t.Errorf("address %d: name %v, want %q", i, got[i].Name, *want[i].Name)
			}
		}
	}
	if _, ok := FormatAddresses([]Address{{Email: "no-at-sign"}}); ok {
		t.Error("address without a domain should not be representable")
	}
	if _, ok := FormatAddresses([]Address{{Email: "ünï@example.com"}}); ok {
		t.Error("a non-ASCII addr-spec (EAI) should not be representable")
	}
}

func TestFormatGroupedAddressesRoundTrip(t *testing.T) {
	name := func(s string) *string { return &s }
	want := []AddressGroup{
		{Name: nil, Addresses: []Address{{Email: "solo@example.com"}}},
		{Name: name("Team"), Addresses: []Address{
			{Name: name("A"), Email: "a@example.com"},
			{Email: "b@example.com"},
		}},
		{Name: name("Empty Group"), Addresses: nil},
	}
	atoms, ok := FormatGroupedAddresses(want)
	if !ok {
		t.Fatal("not representable")
	}
	got := GroupedAddressesForm(FoldValue("To", atoms))
	if len(got) != 3 || got[0].Name != nil || got[1].Name == nil || *got[1].Name != "Team" ||
		got[2].Name == nil || *got[2].Name != "Empty Group" {
		t.Fatalf("parsed back %+v", got)
	}
	if len(got[1].Addresses) != 2 || got[1].Addresses[0].Email != "a@example.com" {
		t.Fatalf("group members %+v", got[1].Addresses)
	}
	if len(got[2].Addresses) != 0 {
		t.Fatalf("empty group members %+v", got[2].Addresses)
	}
}

func TestFormatMessageIDsRoundTrip(t *testing.T) {
	want := []string{"one@example.com", "two.2@host.example"}
	atoms, ok := FormatMessageIDs(want)
	if !ok {
		t.Fatal("not representable")
	}
	got := MessageIDsForm(FoldValue("References", atoms))
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parsed back %v", got)
	}
	for _, bad := range [][]string{{""}, {"has space@x"}, {"angle<@x"}, {"ünï@x"}} {
		if _, ok := FormatMessageIDs(bad); ok {
			t.Errorf("FormatMessageIDs(%v) should not be representable", bad)
		}
	}
}

func TestFormatURLsRoundTrip(t *testing.T) {
	want := []string{"https://example.com/unsub?u=1", "mailto:unsub@example.com"}
	atoms, ok := FormatURLs(want)
	if !ok {
		t.Fatal("not representable")
	}
	if got := URLsForm(FoldValue("List-Unsubscribe", atoms)); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parsed back %v", got)
	}
}

func TestFormatDateRoundTrip(t *testing.T) {
	want := time.Date(2024, 5, 4, 10, 30, 9, 0, time.FixedZone("", 2*3600))
	got := DateForm(FormatDate(want))
	if got == nil || !got.Equal(want) {
		t.Fatalf("DateForm(FormatDate) = %v, want %v", got, want)
	}
}

func TestFoldValueLongList(t *testing.T) {
	var addrs []Address
	for i := 0; i < 20; i++ {
		addrs = append(addrs, Address{Email: "recipient" + string(rune('a'+i)) + "@a-long-domain.example.com"})
	}
	atoms, _ := FormatAddresses(addrs)
	value := FoldValue("To", atoms)
	for i, line := range strings.Split("To: "+value, "\r\n") {
		if len(line) > foldLimit+1 { // +1: the folding space itself
			t.Errorf("line %d over the fold limit: %d chars", i, len(line))
		}
	}
	if got := AddressesForm(value); len(got) != 20 {
		t.Fatalf("parsed back %d addresses", len(got))
	}
}

// literal returns a content source over a fixed byte string.
func literal(s string) func(context.Context) (io.ReadCloser, error) {
	return func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(s)), nil
	}
}

// TestWriteMessageFraming pins the exact RFC 2046 multipart framing and
// the three content encodings with a fixed boundary.
func TestWriteMessageFraming(t *testing.T) {
	root := &OutPart{
		Headers:  []HeaderField{{Name: "Content-Type", Value: `multipart/mixed; boundary="BND"`}},
		Boundary: "BND",
		SubParts: []*OutPart{
			{
				Headers: []HeaderField{
					{Name: "Content-Type", Value: "text/plain; charset=utf-8"},
					{Name: "Content-Transfer-Encoding", Value: Enc7Bit},
				},
				Encoding: Enc7Bit,
				Content:  literal("Hello\r\nWorld"),
			},
			{
				Headers: []HeaderField{
					{Name: "Content-Type", Value: "text/plain; charset=utf-8"},
					{Name: "Content-Transfer-Encoding", Value: EncQP},
				},
				Encoding: EncQP,
				Content:  literal("café\r\n"),
			},
			{
				Headers: []HeaderField{
					{Name: "Content-Type", Value: "application/octet-stream"},
					{Name: "Content-Transfer-Encoding", Value: EncBase64},
				},
				Encoding: EncBase64,
				Content:  literal("\x00\x01\x02\x03\x04\x05\x06\x07\x08\x09"),
			},
		},
	}
	var buf bytes.Buffer
	err := WriteMessage(context.Background(), &buf,
		[]HeaderField{{Name: "From", Value: "a@example.com"}, {Name: "MIME-Version", Value: "1.0"}}, root)
	if err != nil {
		t.Fatal(err)
	}
	want := "From: a@example.com\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"BND\"\r\n" +
		"\r\n" +
		"--BND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n" +
		"\r\n" +
		"Hello\r\nWorld\r\n" +
		"--BND\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		"caf=C3=A9\r\n" +
		"\r\n" +
		"--BND\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"AAECAwQFBgcICQ==\r\n" +
		"--BND--\r\n"
	if got := buf.String(); got != want {
		t.Fatalf("framing mismatch:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func TestLineWrapperBase64(t *testing.T) {
	content := strings.Repeat("x", 200)
	p := &OutPart{Encoding: EncBase64, Content: literal(content)}
	var buf bytes.Buffer
	if err := WriteMessage(context.Background(), &buf, nil, p); err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(buf.String(), "\r\n")
	for i, line := range strings.Split(body, "\r\n") {
		if len(line) > 76 {
			t.Fatalf("base64 line %d is %d chars", i, len(line))
		}
	}
}

func TestNewBoundaryUnique(t *testing.T) {
	a, b := NewBoundary(), NewBoundary()
	if a == b || len(a) < 20 {
		t.Fatalf("boundaries %q and %q", a, b)
	}
}
