package mail

// Message generation tests (RFC 8621 section 4.6). The star is the
// round-trip oracle: parse(generate(email)) must hand back exactly what
// the creation object declared - the parser is the independent reader the
// generator is judged against. Alongside it: the strict rejection table
// for the 4.6 constraint list, header synthesis, and a fuzz target over
// creation objects.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// mapOpener is a blobOpener over fixed contents.
func mapOpener(m map[jmap.Id][]byte) blobOpener {
	return func(_ context.Context, id jmap.Id) (io.ReadCloser, error) {
		b, ok := m[id]
		if !ok {
			return nil, blob.ErrNotFound
		}
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

var genTestNow = time.Date(2026, 7, 17, 9, 30, 0, 0, time.UTC)

// generateAndParse plans obj, streams it, and parses it back with full
// capture.
func generateAndParse(t *testing.T, obj map[string]json.RawMessage, cfg genConfig, blobs map[jmap.Id][]byte) *parsed {
	t.Helper()
	m, serr := planEmailMessage(obj, cfg, genTestNow, mapOpener(blobs))
	if serr != nil {
		t.Fatalf("plan: %+v", serr)
	}
	var buf bytes.Buffer
	if err := m.write(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	c := newCapture()
	c.identity, c.preview, c.values = true, true, true
	p, err := parseMessage(bytes.NewReader(buf.Bytes()), c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func objOf(t *testing.T, src string) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(src), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// TestEmailGenerateRoundTrip is the oracle: a realistic creation object -
// non-ASCII names and subject, both body alternatives, a cid-referenced
// inline image, a binary attachment with a non-ASCII filename - generates
// a message the parser reads back as exactly the declared inputs.
func TestEmailGenerateRoundTrip(t *testing.T) {
	binContent := make([]byte, 300)
	for i := range binContent {
		binContent[i] = byte(i)
	}
	imgContent := []byte("PNG pretend image bytes \x00\x01\xfe\xff")
	blobs := map[jmap.Id][]byte{"Gimg": imgContent, "Gbin": binContent}

	textValue := "Hej!\nÅäö and a second line.\n"
	htmlValue := `<p>Hello <img src="cid:img1"> world</p>`
	obj := objOf(t, `{
		"mailboxIds": {"Mbx1": true},
		"keywords": {"$draft": true},
		"from": [{"name":"Jøhn Døe","email":"john@example.com"}],
		"to": [{"name":"Doe, Jane","email":"jane@example.com"},{"name":null,"email":"carol@example.com"}],
		"subject": "Grüße: Statusbericht 42",
		"sentAt": "2024-05-04T10:30:00+02:00",
		"messageId": ["mid-1@example.com"],
		"inReplyTo": ["orig@example.com"],
		"header:X-Custom:asText": "hello world",
		"textBody": [{"partId":"t1","type":"text/plain"}],
		"htmlBody": [{"partId":"h1","type":"text/html"}],
		"attachments": [
			{"blobId":"Gimg","type":"image/png","cid":"img1"},
			{"blobId":"Gbin","type":"application/octet-stream","name":"räksmörgås.bin"}
		],
		"bodyValues": {
			"t1": {"value":"Hej!\nÅäö and a second line.\n"},
			"h1": {"value":"<p>Hello <img src=\"cid:img1\"> world</p>"}
		}
	}`)

	m, serr := planEmailMessage(obj, genConfig{}, genTestNow, mapOpener(blobs))
	if serr != nil {
		t.Fatalf("plan: %+v", serr)
	}
	if len(m.blobIds) != 2 || m.blobIds[0] != "Gbin" || m.blobIds[1] != "Gimg" {
		t.Fatalf("blobIds = %v", m.blobIds)
	}
	var buf bytes.Buffer
	if err := m.write(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	c := newCapture()
	c.identity, c.preview, c.values = true, true, true
	p, err := parseMessage(bytes.NewReader(buf.Bytes()), c)
	if err != nil {
		t.Fatal(err)
	}

	// The declared header properties come back through the parsed forms.
	fromRaw, _ := p.msg.HeaderLast("From")
	from := message.AddressesForm(fromRaw)
	if len(from) != 1 || from[0].Email != "john@example.com" || from[0].Name == nil || *from[0].Name != "Jøhn Døe" {
		t.Errorf("from = %+v", from)
	}
	toRaw, _ := p.msg.HeaderLast("To")
	to := message.AddressesForm(toRaw)
	if len(to) != 2 || to[0].Name == nil || *to[0].Name != "Doe, Jane" ||
		to[0].Email != "jane@example.com" || to[1].Name != nil || to[1].Email != "carol@example.com" {
		t.Errorf("to = %+v", to)
	}
	subjRaw, _ := p.msg.HeaderLast("Subject")
	if got := message.TextForm(subjRaw); got != "Grüße: Statusbericht 42" {
		t.Errorf("subject = %q", got)
	}
	dateRaw, _ := p.msg.HeaderLast("Date")
	want, _ := time.Parse(time.RFC3339, "2024-05-04T10:30:00+02:00")
	if got := message.DateForm(dateRaw); got == nil || !got.Equal(want) {
		t.Errorf("date = %v", got)
	}
	midRaw, _ := p.msg.HeaderLast("Message-ID")
	if got := message.MessageIDsForm(midRaw); len(got) != 1 || got[0] != "mid-1@example.com" {
		t.Errorf("messageId = %v", got)
	}
	irtRaw, _ := p.msg.HeaderLast("In-Reply-To")
	if got := message.MessageIDsForm(irtRaw); len(got) != 1 || got[0] != "orig@example.com" {
		t.Errorf("inReplyTo = %v", got)
	}
	custRaw, _ := p.msg.HeaderLast("X-Custom")
	if got := message.TextForm(custRaw); got != "hello world" {
		t.Errorf("X-Custom = %q", got)
	}
	if v, ok := p.msg.HeaderLast("MIME-Version"); !ok || strings.TrimSpace(v) != "1.0" {
		t.Errorf("MIME-Version = %q, %v", v, ok)
	}

	// The body views decompose to exactly the declared parts.
	if len(p.textBody) != 1 || p.textBody[0].Type != "text/plain" {
		t.Fatalf("textBody = %+v", p.textBody)
	}
	if got := p.cap.valueSinks[p.textBody[0]].value; got != textValue {
		t.Errorf("text value = %q, want %q", got, textValue)
	}
	if len(p.htmlBody) != 1 || p.htmlBody[0].Type != "text/html" {
		t.Fatalf("htmlBody = %+v", p.htmlBody)
	}
	if got := p.cap.valueSinks[p.htmlBody[0]].value; got != htmlValue {
		t.Errorf("html value = %q, want %q", got, htmlValue)
	}
	if len(p.attachments) != 2 {
		t.Fatalf("attachments = %+v", p.attachments)
	}
	var img, bin *message.Part
	for _, a := range p.attachments {
		switch a.Type {
		case "image/png":
			img = a
		case "application/octet-stream":
			bin = a
		}
	}
	if img == nil || img.Cid == nil || *img.Cid != "img1" {
		t.Fatalf("image attachment = %+v", img)
	}
	if img.Size != uint64(len(imgContent)) || img.Digest != sha256.Sum256(imgContent) {
		t.Errorf("image content identity: size %d, want %d", img.Size, len(imgContent))
	}
	if bin == nil || bin.Name == nil || *bin.Name != "räksmörgås.bin" {
		t.Fatalf("binary attachment = %+v", bin)
	}
	if bin.Size != uint64(len(binContent)) || bin.Digest != sha256.Sum256(binContent) {
		t.Errorf("binary content identity: size %d, want %d", bin.Size, len(binContent))
	}
	if !p.hasAttachment() {
		t.Error("hasAttachment = false")
	}
	if pv := p.preview(); !strings.Contains(pv, "Hej!") {
		t.Errorf("preview = %q", pv)
	}

	// The structure is the documented convenience mapping:
	// mixed(related(alternative(plain, html), image), bin).
	root := p.msg.Root
	if root.Type != "multipart/mixed" || len(root.SubParts) != 2 {
		t.Fatalf("root = %s with %d children", root.Type, len(root.SubParts))
	}
	rel := root.SubParts[0]
	if rel.Type != "multipart/related" || len(rel.SubParts) != 2 || rel.SubParts[1].Type != "image/png" {
		t.Fatalf("related = %s / %+v", rel.Type, rel.SubParts)
	}
	alt := rel.SubParts[0]
	if alt.Type != "multipart/alternative" || len(alt.SubParts) != 2 ||
		alt.SubParts[0].Type != "text/plain" || alt.SubParts[1].Type != "text/html" {
		t.Fatalf("alternative = %s / %+v", alt.Type, alt.SubParts)
	}
}

// TestEmailGenerateSynthesis: Message-ID and Date are synthesized when
// absent (4.6 MUST), with the Message-ID domain from the config knob,
// else the From domain, else localhost.
func TestEmailGenerateSynthesis(t *testing.T) {
	src := `{"from":[{"name":null,"email":"john@example.com"}],
		"textBody":[{"partId":"t1"}],"bodyValues":{"t1":{"value":"hi"}}}`
	cases := []struct {
		cfg        genConfig
		obj        string
		wantSuffix string
	}{
		{genConfig{msgIDDomain: "mx.example.org"}, src, "@mx.example.org"},
		{genConfig{}, src, "@example.com"},
		{genConfig{}, `{"textBody":[{"partId":"t1"}],"bodyValues":{"t1":{"value":"hi"}}}`, "@localhost"},
	}
	for _, tc := range cases {
		p := generateAndParse(t, objOf(t, tc.obj), tc.cfg, nil)
		midRaw, _ := p.msg.HeaderLast("Message-ID")
		mids := message.MessageIDsForm(midRaw)
		if len(mids) != 1 || !strings.HasSuffix(mids[0], tc.wantSuffix) || len(mids[0]) < len(tc.wantSuffix)+16 {
			t.Errorf("synthesized Message-ID = %v, want unique id %s", mids, tc.wantSuffix)
		}
		dateRaw, _ := p.msg.HeaderLast("Date")
		if got := message.DateForm(dateRaw); got == nil || !got.Equal(genTestNow) {
			t.Errorf("synthesized Date = %v, want %v", got, genTestNow)
		}
	}
}

// TestEmailGenerateEmptyBody: a creation object with no body properties
// is a legal draft - it generates an empty text/plain body.
func TestEmailGenerateEmptyBody(t *testing.T) {
	p := generateAndParse(t, objOf(t, `{"subject":"draft"}`), genConfig{}, nil)
	if len(p.textBody) != 1 || p.textBody[0].Type != "text/plain" {
		t.Fatalf("textBody = %+v", p.textBody)
	}
	if got := p.cap.valueSinks[p.textBody[0]].value; got != "" {
		t.Errorf("value = %q", got)
	}
	if p.hasAttachment() {
		t.Error("hasAttachment = true")
	}
}

// TestEmailGenerateBodyStructure: a declared bodyStructure passes through
// as built, including a digest-style part-level header.
func TestEmailGenerateBodyStructure(t *testing.T) {
	blobs := map[jmap.Id][]byte{"Gmsg": []byte("Subject: inner\r\n\r\ninner body\r\n")}
	obj := objOf(t, `{
		"bodyStructure": {"type":"multipart/mixed","subParts":[
			{"partId":"t1","type":"text/markdown","header:X-Part-Note":"kept"},
			{"blobId":"Gmsg","type":"message/rfc822"}
		]},
		"bodyValues": {"t1":{"value":"# heading\n"}}
	}`)
	p := generateAndParse(t, obj, genConfig{}, blobs)
	root := p.msg.Root
	if root.Type != "multipart/mixed" || len(root.SubParts) != 2 {
		t.Fatalf("root = %s / %d", root.Type, len(root.SubParts))
	}
	md, msg822 := root.SubParts[0], root.SubParts[1]
	if md.Type != "text/markdown" {
		t.Fatalf("first child = %s", md.Type)
	}
	if v, ok := md.HeaderLast("X-Part-Note"); !ok || strings.TrimSpace(v) != "kept" {
		t.Errorf("part header = %q, %v", v, ok)
	}
	// message/rfc822 must not be base64 (RFC 2046 section 5.2.1): its raw
	// octets stream through unencoded, so the embedded message survives
	// byte for byte and its decoded identity equals the blob.
	if msg822.Type != "message/rfc822" {
		t.Fatalf("second child = %s", msg822.Type)
	}
	if msg822.Digest != sha256.Sum256(blobs["Gmsg"]) {
		t.Error("embedded message bytes changed in transit")
	}
}

// TestEmailGenerateRejections is the 4.6 constraint list as a table:
// every rule violation is rejected with invalidProperties naming the
// offending property.
func TestEmailGenerateRejections(t *testing.T) {
	values := `"bodyValues":{"t1":{"value":"x"}}`
	cases := []struct {
		name     string
		obj      string
		wantProp string
	}{
		{"headers property", `{"headers":[]}`, "headers"},
		{"two properties one field", `{"from":[{"email":"a@b.c"}],"header:From":"a@b.c"}`, "from"},
		{"forbidden form", `{"header:From:asDate":"2024-01-01T00:00:00Z"}`, "header:From:asDate"},
		{"content header on email", `{"header:Content-Type":"text/plain"}`, "header:Content-Type"},
		{"mime-version", `{"header:MIME-Version":"1.0"}`, "header:MIME-Version"},
		{"unknown property", `{"wibble":1}`, "wibble"},
		{"server-set property", `{"hasAttachment":true}`, "hasAttachment"},
		{"raw header injection", `{"header:X-Evil":"a\r\nBcc: x@y.z"}`, "header:X-Evil"},
		{"bad date", `{"sentAt":"yesterday"}`, "sentAt"},
		{"address without email", `{"from":[{"name":"ghost"}]}`, "from"},
		{"structure and views", `{"bodyStructure":{"partId":"t1"},"textBody":[{"partId":"t1"}],` + values + `}`, "textBody"},
		{"textBody two parts", `{"textBody":[{"partId":"t1"},{"partId":"t2"}],` + values + `}`, "textBody"},
		{"textBody wrong type", `{"textBody":[{"partId":"t1","type":"text/html"}],` + values + `}`, "textBody/0"},
		{"htmlBody default type", `{"htmlBody":[{"partId":"t1"}],` + values + `}`, "htmlBody/0"},
		{"partId and blobId", `{"textBody":[{"partId":"t1","blobId":"Gx"}],` + values + `}`, "textBody/0"},
		{"neither partId nor blobId", `{"textBody":[{"type":"text/plain"}]}`, "textBody/0"},
		{"charset with partId", `{"textBody":[{"partId":"t1","charset":"utf-8"}],` + values + `}`, "textBody/0/charset"},
		{"size with partId", `{"textBody":[{"partId":"t1","size":12}],` + values + `}`, "textBody/0/size"},
		{"partId not in bodyValues", `{"textBody":[{"partId":"missing"}],` + values + `}`, "textBody/0/partId"},
		{"duplicate partId", `{"textBody":[{"partId":"t1"}],"attachments":[{"partId":"t1"}],` + values + `}`, "attachments/0/partId"},
		{"cte header on part", `{"textBody":[{"partId":"t1","header:Content-Transfer-Encoding":"8bit"}],` + values + `}`, "textBody/0/header:Content-Transfer-Encoding"},
		{"content header on part", `{"textBody":[{"partId":"t1","header:Content-Type":"text/plain"}],` + values + `}`, "textBody/0/header:Content-Type"},
		{"headers on part", `{"textBody":[{"partId":"t1","headers":[]}],` + values + `}`, "textBody/0/headers"},
		{"isTruncated true", `{"textBody":[{"partId":"t1"}],"bodyValues":{"t1":{"value":"x","isTruncated":true}}}`, "bodyValues/t1"},
		{"isEncodingProblem true", `{"textBody":[{"partId":"t1"}],"bodyValues":{"t1":{"value":"x","isEncodingProblem":true}}}`, "bodyValues/t1"},
		{"unknown bodyValue field", `{"textBody":[{"partId":"t1"}],"bodyValues":{"t1":{"value":"x","wibble":1}}}`, "bodyValues"},
		{"multipart with blobId", `{"bodyStructure":{"type":"multipart/mixed","blobId":"Gx","subParts":[{"partId":"t1"}]},` + values + `}`, "bodyStructure"},
		{"multipart without subParts", `{"bodyStructure":{"type":"multipart/mixed"}}`, "bodyStructure/subParts"},
		{"subParts on a leaf", `{"bodyStructure":{"partId":"t1","subParts":[{"partId":"t1"}]},` + values + `}`, "bodyStructure/subParts"},
		{"multipart attachment", `{"attachments":[{"type":"multipart/mixed","subParts":[{"partId":"t1"}]}],` + values + `}`, "attachments/0"},
		{"non-text partId part", `{"bodyStructure":{"partId":"t1","type":"application/json"},` + values + `}`, "bodyStructure/type"},
		{"bad media type", `{"bodyStructure":{"partId":"t1","type":"not a type"},` + values + `}`, "bodyStructure/type"},
		{"part header field also on email", `{"header:X-Note":"top","textBody":[{"partId":"t1","header:X-Note":"part"}],` + values + `}`, "header:X-Note"},
		{"non-ASCII messageId", `{"messageId":["ünï@x"]}`, "messageId"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, serr := planEmailMessage(objOf(t, tc.obj), genConfig{}, genTestNow, mapOpener(nil))
			if serr == nil {
				t.Fatal("plan accepted")
			}
			if serr.Type != jmap.SetErrInvalidProperties {
				t.Fatalf("error type %s", serr.Type)
			}
			found := false
			for _, prop := range serr.Properties {
				if prop == tc.wantProp {
					found = true
				}
			}
			if !found {
				t.Fatalf("properties %v, want %s", serr.Properties, tc.wantProp)
			}
		})
	}
}

// TestEmailGenerateAllForm: an :all header property writes one instance
// per element.
func TestEmailGenerateAllForm(t *testing.T) {
	p := generateAndParse(t, objOf(t, `{"header:X-Tag:asText:all":["one","two"]}`), genConfig{}, nil)
	got := p.msg.HeaderInstances("X-Tag")
	if len(got) != 2 || message.TextForm(got[0]) != "one" || message.TextForm(got[1]) != "two" {
		t.Fatalf("X-Tag instances = %v", got)
	}
}

// FuzzEmailGenerate: any JSON object either rejects cleanly or generates
// a message the parser reads back without error - never a panic, never
// unparseable output.
func FuzzEmailGenerate(f *testing.F) {
	f.Add([]byte(`{"subject":"hi","textBody":[{"partId":"t1"}],"bodyValues":{"t1":{"value":"x"}}}`))
	f.Add([]byte(`{"from":[{"name":"Jø","email":"a@b.c"}],"header:X-T:asText:all":["a","b"]}`))
	f.Add([]byte(`{"bodyStructure":{"type":"multipart/mixed","subParts":[{"blobId":"G1"},{"partId":"p","type":"text/html"}]},"bodyValues":{"p":{"value":"<b>x</b>"}}}`))
	f.Add([]byte(`{"headers":[],"wibble":true}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var obj map[string]json.RawMessage
		if json.Unmarshal(data, &obj) != nil {
			return
		}
		open := func(_ context.Context, id jmap.Id) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("blob bytes for " + string(id))), nil
		}
		m, serr := planEmailMessage(obj, genConfig{}, genTestNow, open)
		if serr != nil {
			return
		}
		var buf bytes.Buffer
		if err := m.write(context.Background(), &buf); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := parseMessage(bytes.NewReader(buf.Bytes()), newCapture()); err != nil {
			t.Fatalf("parse of generated message: %v", err)
		}
	})
}
