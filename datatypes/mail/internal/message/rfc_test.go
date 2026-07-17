package message

// Messages taken from the specifications themselves. Each is run through the
// differential harness, so the streaming parser must make of them exactly what
// the parser it replaces made; the ones whose shape the RFC states outright are
// also asserted directly, so the corpus is not merely self-consistent but right.
//
// RFC 2045 (transfer encodings, parameters), RFC 2046 (multipart structure,
// digest, alternative, message/rfc822), RFC 2047 (encoded words in headers),
// RFC 2049 (the worked example), RFC 2183 (Content-Disposition), RFC 2231
// (parameter continuations and charsets), RFC 5322 (folding, white space,
// comments), RFC 8621 4.1.4 (the A-K decomposition, in structure_test.go).

import (
	"strings"
	"testing"
)

// rfc2049Example is the complete message of RFC 2049 Appendix A: a
// multipart/mixed carrying text, an alternative with plain and richtext, an
// image, and an attached message.
const rfc2049Example = `MIME-Version: 1.0
From: Nathaniel Borenstein <nsb@nsb.fv.com>
To: Ned Freed <ned@innosoft.com>
Date: Fri, 07 Oct 1994 16:15:05 -0700 (PDT)
Subject: A multipart example
Content-Type: multipart/mixed; boundary=unique-boundary-1

This is the preamble area of a multipart message.
Mail readers that understand multipart format
should ignore this preamble.

If you are reading this text, you might want to
consider changing to a mail reader that understands
how to properly display multipart messages.

--unique-boundary-1

  ... Some text appears here ...

--unique-boundary-1
Content-type: text/plain; charset=US-ASCII

This could have been part of the previous part, but
illustrates explicit versus implicit typing of body
parts.

--unique-boundary-1
Content-Type: multipart/parallel; boundary=unique-boundary-2

--unique-boundary-2
Content-Type: audio/basic
Content-Transfer-Encoding: base64

aGVsbG8gd29ybGQ=

--unique-boundary-2
Content-Type: image/jpeg
Content-Transfer-Encoding: base64

/9j/4AAQSkZJRgABAQEAYABgAAD/2wBD

--unique-boundary-2--

--unique-boundary-1
Content-type: text/enriched

This is <bold><italic>enriched.</italic></bold>
<smaller>as defined in RFC 1896</smaller>

Isn't it
<bigger><bigger>cool?</bigger></bigger>

--unique-boundary-1
Content-Type: message/rfc822

From: (mailbox in US-ASCII)
To: (address in US-ASCII)
Subject: (subject in US-ASCII)
Content-Type: Text/plain; charset=ISO-8859-1
Content-Transfer-Encoding: Quoted-printable

  ... Additional text in ISO-8859-1 goes here ...

--unique-boundary-1--
`

// rfcCases are the specification-derived messages, shared with the chunked and
// differential tests. They are named so an assertion about one cannot drift onto
// another when the corpus grows.
func rfcCases() map[string]string {
	return map[string]string{
		"rfc2049 worked example": crlf(rfc2049Example),

		// RFC 2046 5.1.1: the simplest multipart, with a preamble and epilogue.
		"rfc2046 simple multipart": crlf(`From: Nathaniel Borenstein <nsb@bellcore.com>
To: Ned Freed <ned@innosoft.com>
Subject: Sample message
MIME-Version: 1.0
Content-type: multipart/mixed; boundary="simple boundary"

This is the preamble.  It is to be ignored, though it
is a handy place for composition agents to include an
explanatory note to non-MIME conformant readers.

--simple boundary

This is implicitly typed plain US-ASCII text.
It does NOT end with a linebreak.
--simple boundary
Content-type: text/plain; charset=us-ascii

This is explicitly typed plain US-ASCII text.
It DOES end with a linebreak.

--simple boundary--

This is the epilogue.  It is also to be ignored.
`),

		// RFC 2046 5.1.4: multipart/alternative, best version last.
		"rfc2046 alternative": crlf(`Content-Type: multipart/alternative; boundary=boundary42

--boundary42
Content-Type: text/plain; charset=us-ascii

... plain text version of message goes here ...

--boundary42
Content-Type: text/enriched

... RFC 1896 text/enriched version of same message goes here ...

--boundary42
Content-Type: application/x-whatever

... fanciest version of same message goes here ...

--boundary42--
`),

		// RFC 2046 5.1.5: multipart/digest, whose parts default to message/rfc822.
		"rfc2046 digest": crlf(`From: Moderator-Address <mod@example.com>
Subject: Internet Digest, volume 42
MIME-Version: 1.0
Content-Type: multipart/digest; boundary="---- next message ----"

------ next message ----

From: someone-else@example.com
Subject: my opinion

    ...body goes here ...

------ next message ----

From: someone-else-again@example.com
Subject: my different opinion

    ... another body goes here ...

------ next message ------
`),

		// RFC 2046 5.2.1: message/rfc822 is a leaf - the parser does not recurse
		// into the message it carries (RFC 8621 4.1.4).
		"rfc2046 message/rfc822 leaf": crlf(`Content-Type: message/rfc822

From: inner@example.com
Subject: the embedded message
Content-Type: multipart/mixed; boundary=inner

--inner
Content-Type: text/plain

this boundary belongs to the embedded message, not to ours
--inner--
`),

		// RFC 2045 6.7: quoted-printable, including a soft line break and an
		// encoded trailing space.
		"rfc2045 quoted-printable": crlf(`Content-Type: text/plain; charset=iso-8859-1
Content-Transfer-Encoding: quoted-printable

Now's the time =
for all folk to come=
 to the aid of their country.
Trailing whitespace here:=20
Caf=E9 and =3D signs.
`),

		// RFC 2045 6.8: base64, wrapped across lines.
		"rfc2045 base64": crlf(`Content-Type: application/octet-stream
Content-Transfer-Encoding: base64

VGhpcyBpcyBhIHRlc3Qgb2YgYmFzZTY0IGVuY29kaW5nIHRoYXQgcnVucyBh
Y3Jvc3MgbW9yZSB0aGFuIG9uZSBsaW5lIHNvIHRoZSBkZWNvZGVyIG11c3Qg
am9pbiB0aGVtLg==
`),

		// RFC 2047: encoded words in header fields - B and Q encodings, adjacent
		// words that must be joined, and one that must not be decoded (no charset).
		"rfc2047 encoded words": crlf(`From: =?US-ASCII?Q?Keith_Moore?= <moore@cs.utk.edu>
To: =?ISO-8859-1?Q?Keld_J=F8rn_Simonsen?= <keld@dkuug.dk>
Cc: =?ISO-8859-1?Q?Andr=E9?= Pirard <PIRARD@vm1.ulg.ac.be>
Subject: =?ISO-8859-1?B?SWYgeW91IGNhbiByZWFkIHRoaXMgeW8=?=
 =?ISO-8859-2?B?dSB1bmRlcnN0YW5kIHRoZSBleGFtcGxlLg==?=
Content-Type: text/plain

body
`),

		// RFC 2183 + RFC 2231: a disposition with a continued, charset-tagged
		// filename parameter, and one encoded with RFC 2047 words (illegal but
		// common).
		"rfc2231 filename continuation": crlf(`Content-Type: multipart/mixed; boundary=b

--b
Content-Type: application/pdf
Content-Disposition: attachment;
 filename*0*=us-ascii'en'This%20is%20a%20very%20long%20;
 filename*1*=file%20name.pdf

pdf bytes
--b
Content-Type: image/png; name="=?utf-8?Q?sm=C3=A5.png?="
Content-Disposition: inline; filename="=?utf-8?Q?sm=C3=A5.png?="
Content-Id: <img@example.com>

png bytes
--b
Content-Type: text/plain; name="=?utf-8?Q?sm=C3=A5?=.txt"
Content-Disposition: attachment; filename="=?utf-8?Q?sm=C3=A5?=.txt"

glued to a suffix, so not an encoded word at all
--b--
`),

		// RFC 5322 A.5: white space, folding, and comments everywhere they are
		// allowed - the header block must survive all of it.
		"rfc5322 white space and comments": crlf(`From: Pete(A nice \) chap) <pete(his account)@silly.test(his host)>
To:A Group(Some people)
     :Chris Jones <c@(Chris's host.)public.example>,
         joe@example.org,
  John <jdoe@one.test> (my dear friend); (the end of the group)
Cc:(Empty list)(start)Hidden recipients  :(nobody(that I know))  ;
Date: Thu,
      13
        Feb
          1969
      23:32
               -0330 (Newfoundland Time)
Message-ID:              <testabcd.1234@silly.test>

Testing.
`),

		// A boundary that is a prefix of another: only an exact match, allowing
		// trailing white space, is a delimiter (RFC 2046 5.1.1).
		"boundary prefix collision": crlf(`Content-Type: multipart/mixed; boundary=b

--bb
--b
Content-Type: text/plain

first
--b2
--b
Content-Type: text/plain

second
--b--
`),

		// A quoted boundary holding spaces and specials, as agents really emit.
		"quoted boundary": crlf(`Content-Type: multipart/mixed; boundary="---- =_NextPart_000_0000"

------ =_NextPart_000_0000
Content-Type: text/plain

x
------ =_NextPart_000_0000--
`),

		// No Content-Type at all: implicitly text/plain, us-ascii (RFC 2045 5.2).
		"implicit typing": crlf(`From: a@example.com
Subject: implicit typing

just text
`),

		// Headers only, no body at all.
		"headers only": crlf(`From: a@example.com
Subject: no body

`),

		// 8-bit content in a declared charset, no transfer encoding.
		"8-bit charset": "Content-Type: text/plain; charset=iso-8859-1\r\n\r\nCaf\xe9 na\xefve\r\n",

		// An inline image in a multipart/related, referenced by cid (RFC 2392).
		"multipart/related cid": crlf(`Content-Type: multipart/related; boundary=r; type="text/html"; start="<html@x>"

--r
Content-Type: text/html
Content-Id: <html@x>

<p><img src="cid:pic@x"></p>
--r
Content-Type: image/png
Content-Id: <pic@x>
Content-Transfer-Encoding: base64

iVBORw0KGgo=
--r--
`),
	}
}

// TestRFCCorpus runs every specification example through the differential
// harness.
func TestRFCCorpus(t *testing.T) {
	for name, raw := range rfcCases() {
		t.Run(name, func(t *testing.T) {
			assertSameAsOracle(t, []byte(raw))
		})
	}
}

// TestRFC2049Structure checks the worked example against what RFC 2049 says it
// is, rather than only against what the old parser said it was: five parts at
// the top level, an untyped part that is text/plain by default (RFC 2045 5.2), a
// nested multipart/parallel of two encoded parts, and an embedded message that
// is a leaf (RFC 8621 4.1.4).
func TestRFC2049Structure(t *testing.T) {
	m := parseDoc(t, []byte(crlf(rfc2049Example)))
	root := m.Root
	if root.Type != "multipart/mixed" || len(root.SubParts) != 5 {
		t.Fatalf("root = %s with %d parts, want multipart/mixed with 5", root.Type, len(root.SubParts))
	}
	if got := root.SubParts[0].Type; got != "text/plain" {
		t.Errorf("the untyped part is %s, want the text/plain default", got)
	}
	if cs := root.SubParts[0].Charset; cs == nil || *cs != "us-ascii" {
		t.Errorf("the untyped part's charset is %v, want the us-ascii default", cs)
	}
	par := root.SubParts[2]
	if par.Type != "multipart/parallel" || len(par.SubParts) != 2 {
		t.Fatalf("part 3 = %s with %d parts, want multipart/parallel with 2", par.Type, len(par.SubParts))
	}
	if got := string(m.Content[par.SubParts[0]]); got != "hello world" {
		t.Errorf("the base64 audio part decoded to %q, want %q", got, "hello world")
	}
	embedded := root.SubParts[4]
	if embedded.Type != "message/rfc822" || embedded.SubParts != nil {
		t.Fatalf("part 5 = %s subParts=%v, want a message/rfc822 leaf", embedded.Type, embedded.SubParts)
	}
	if !strings.Contains(string(m.Content[embedded]), "Subject: (subject in US-ASCII)") {
		t.Error("the embedded message's own headers are part of its content, not parsed away")
	}
	// The preamble and the epilogue are not content of anything.
	for _, p := range m.Parts() {
		if strings.Contains(string(m.Content[p]), "reading this text") {
			t.Errorf("part %s swallowed the preamble", p.PartID)
		}
	}
}

// TestRFC2046Digest: inside multipart/digest a part with no Content-Type is
// message/rfc822, not text/plain (RFC 2046 5.1.5).
func TestRFC2046Digest(t *testing.T) {
	m := parseDoc(t, []byte(rfcCases()["rfc2046 digest"]))
	if len(m.Root.SubParts) != 2 {
		t.Fatalf("got %d parts, want 2", len(m.Root.SubParts))
	}
	for i, p := range m.Root.SubParts {
		if p.Type != "message/rfc822" {
			t.Errorf("digest part %d is %s, want the message/rfc822 default", i, p.Type)
		}
	}
}

// TestRFC2231Filename: a filename split across continuations and tagged with a
// charset is reassembled and decoded (RFC 2231 3 and 4).
func TestRFC2231Filename(t *testing.T) {
	m := parseDoc(t, []byte(rfcCases()["rfc2231 filename continuation"]))
	pdf := m.Root.SubParts[0]
	if pdf.Name == nil || *pdf.Name != "This is a very long file name.pdf" {
		t.Errorf("filename = %q, want the continuations joined and percent-decoded", deref(pdf.Name))
	}
	// An encoded word in a parameter is illegal (RFC 2231 is the sanctioned way)
	// but common, and is decoded when it really is one.
	png := m.Root.SubParts[1]
	if png.Name == nil || *png.Name != "små.png" {
		t.Errorf("filename = %q, want the encoded word decoded", deref(png.Name))
	}
	if png.Cid == nil || *png.Cid != "img@example.com" {
		t.Errorf("cid = %q, want the angle brackets stripped", deref(png.Cid))
	}
	// RFC 2047 section 5: an encoded word must be delimited from the text around
	// it. Run together with a suffix it is not an encoded word, and the filename
	// is the literal characters - decoding it anyway would let a sender smuggle
	// one file name past a check and land another on disk.
	txt := m.Root.SubParts[2]
	if txt.Name == nil || *txt.Name != "=?utf-8?Q?sm=C3=A5?=.txt" {
		t.Errorf("filename = %q, want the undelimited word left alone", deref(txt.Name))
	}
}
