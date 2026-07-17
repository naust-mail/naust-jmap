package mail

// Email/parse tests (RFC 8621 section 4.9): a blob renders as an Email built
// from the message, the store-only metadata (id, mailboxIds, keywords,
// receivedAt) and threadId come back null, a header form used on an
// inappropriate field is invalidArguments, a missing blob is notFound, and
// body values are fetched on request.

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// akMessage is the RFC 8621 section 4.1.4 worked example: a text+HTML message
// that passed through list software which attached a header and footer. The
// spec prescribes exactly how it decomposes into textBody / htmlBody /
// attachments; section 4.9 (and section 4.1.4) name Email/parse as the way to
// render such a message, so it is the natural end-to-end fixture for parse.
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

// partIds returns the partId of each EmailBodyPart in a returned array.
func partIds(t *testing.T, arr any) []string {
	t.Helper()
	parts, ok := arr.([]any)
	if !ok {
		t.Fatalf("not an array of parts: %v", arr)
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.(map[string]any)["partId"].(string))
	}
	return out
}

// doParse runs one Email/parse and returns the response (or error) args.
func doParse(t *testing.T, ts *httptest.Server, argsJSON string) map[string]any {
	t.Helper()
	r := callMail(t, ts, inv("Email/parse", argsJSON, "0"))
	name := "Email/parse"
	if r.MethodResponses[0].Name == "error" {
		name = "error"
	}
	return methodArgs(t, r, 0, name)
}

func TestEmailParseBasic(t *testing.T) {
	ts, db, store := emailServer(t)
	blobID := uploadBlob(t, db, store, simpleMessage)

	resp := doParse(t, ts, fmt.Sprintf(`{"accountId":%q,"blobIds":[%q]}`, testAccount, blobID))
	parsed, ok := resp["parsed"].(map[string]any)
	if !ok {
		t.Fatalf("no parsed: %v", resp)
	}
	em, ok := parsed[blobID].(map[string]any)
	if !ok {
		t.Fatalf("blob not parsed: %v", resp)
	}
	if em["subject"] != "Dinner on Thursday?" {
		t.Errorf("subject = %v", em["subject"])
	}
	from := em["from"].([]any)
	if len(from) != 1 || from[0].(map[string]any)["email"] != "joe@example.com" {
		t.Errorf("from = %v", em["from"])
	}
	if mids := em["messageId"].([]any); len(mids) != 1 || mids[0] != "msg1@example.com" {
		t.Errorf("messageId = %v", em["messageId"])
	}
	if em["hasAttachment"] != false {
		t.Errorf("hasAttachment = %v", em["hasAttachment"])
	}
	// The default property list is body-oriented: store-only metadata is not
	// returned unless asked for by name.
	for _, absent := range []string{"id", "blobId", "size", "mailboxIds", "keywords", "receivedAt", "threadId"} {
		if _, has := em[absent]; has {
			t.Errorf("default parse should omit %q", absent)
		}
	}
}

func TestEmailParseMetadataNull(t *testing.T) {
	ts, db, store := emailServer(t)
	blobID := uploadBlob(t, db, store, simpleMessage)

	resp := doParse(t, ts, fmt.Sprintf(`{"accountId":%q,"blobIds":[%q],`+
		`"properties":["id","mailboxIds","keywords","receivedAt","threadId","blobId","size"]}`,
		testAccount, blobID))
	em := resp["parsed"].(map[string]any)[blobID].(map[string]any)

	for _, name := range []string{"id", "mailboxIds", "keywords", "receivedAt", "threadId"} {
		v, has := em[name]
		if !has || v != nil {
			t.Errorf("%s = %v (has=%v), want null", name, v, has)
		}
	}
	// blobId and size are honestly derivable and are returned.
	if em["blobId"] != blobID {
		t.Errorf("blobId = %v, want %q", em["blobId"], blobID)
	}
	if em["size"] != float64(len(simpleMessage)) {
		t.Errorf("size = %v, want %d", em["size"], len(simpleMessage))
	}
}

func TestEmailParseForbiddenHeaderForm(t *testing.T) {
	ts, db, store := emailServer(t)
	blobID := uploadBlob(t, db, store, simpleMessage)

	resp := doParse(t, ts, fmt.Sprintf(`{"accountId":%q,"blobIds":[%q],`+
		`"properties":["header:From:asDate"]}`, testAccount, blobID))
	if resp["type"] != "invalidArguments" {
		t.Errorf("type = %v, want invalidArguments", resp["type"])
	}
}

func TestEmailParseNotFound(t *testing.T) {
	ts, _, _ := emailServer(t)
	resp := doParse(t, ts, fmt.Sprintf(`{"accountId":%q,"blobIds":["Gmissing"]}`, testAccount))
	nf, _ := resp["notFound"].([]any)
	if len(nf) != 1 || nf[0] != "Gmissing" {
		t.Errorf("notFound = %v, want [Gmissing]", resp["notFound"])
	}
	if resp["parsed"] != nil {
		t.Errorf("parsed = %v, want null", resp["parsed"])
	}
}

func TestEmailParseBodyValues(t *testing.T) {
	ts, db, store := emailServer(t)
	blobID := uploadBlob(t, db, store, altMessage)

	resp := doParse(t, ts, fmt.Sprintf(`{"accountId":%q,"blobIds":[%q],`+
		`"properties":["bodyValues"],"fetchTextBodyValues":true}`, testAccount, blobID))
	em := resp["parsed"].(map[string]any)[blobID].(map[string]any)
	bv, ok := em["bodyValues"].(map[string]any)
	if !ok || len(bv) == 0 {
		t.Fatalf("bodyValues = %v", em["bodyValues"])
	}
	if one := bv["1"].(map[string]any); one["value"] != "plain body text here" {
		t.Errorf("bodyValues[1] = %v", one)
	}
}

// TestEmailParseAKExample drives the RFC 8621 section 4.1.4 worked example
// through Email/parse and checks the prescribed decomposition: textBody =
// A,B,C,D,K (partIds 1,2,3,4,10), htmlBody = A,E,K (1,5,10), attachments =
// C,F,G,H,J (3,6,7,8,9), and hasAttachment true.
func TestEmailParseAKExample(t *testing.T) {
	ts, db, store := emailServer(t)
	blobID := uploadBlob(t, db, store, strings.ReplaceAll(akMessage, "\n", "\r\n"))

	resp := doParse(t, ts, fmt.Sprintf(`{"accountId":%q,"blobIds":[%q],`+
		`"properties":["textBody","htmlBody","attachments","hasAttachment"],`+
		`"bodyProperties":["partId","type"]}`, testAccount, blobID))
	em := resp["parsed"].(map[string]any)[blobID].(map[string]any)

	if got, want := fmt.Sprint(partIds(t, em["textBody"])), fmt.Sprint([]string{"1", "2", "3", "4", "10"}); got != want {
		t.Errorf("textBody partIds = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(partIds(t, em["htmlBody"])), fmt.Sprint([]string{"1", "5", "10"}); got != want {
		t.Errorf("htmlBody partIds = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(partIds(t, em["attachments"])), fmt.Sprint([]string{"3", "6", "7", "8", "9"}); got != want {
		t.Errorf("attachments partIds = %s, want %s", got, want)
	}
	if em["hasAttachment"] != true {
		t.Errorf("hasAttachment = %v, want true", em["hasAttachment"])
	}
}

// TestEmailParseDefaultPropertySet checks that an omitted properties argument
// yields exactly the section 4.9 default list, no more and no less.
func TestEmailParseDefaultPropertySet(t *testing.T) {
	ts, db, store := emailServer(t)
	blobID := uploadBlob(t, db, store, simpleMessage)

	em := doParse(t, ts, fmt.Sprintf(`{"accountId":%q,"blobIds":[%q]}`, testAccount, blobID))["parsed"].(map[string]any)[blobID].(map[string]any)
	if len(em) != len(emailParseDefaultProperties) {
		t.Fatalf("got %d properties, want %d: %v", len(em), len(emailParseDefaultProperties), em)
	}
	for _, name := range emailParseDefaultProperties {
		if _, has := em[name]; !has {
			t.Errorf("default parse missing %q", name)
		}
	}
}

// eaiMessage carries an internationalized (RFC 6532) address with raw UTF-8 in
// the display name, local part, and domain. Sections 4.8 and 4.9 both require
// the server to support EAI headers.
const eaiMessage = "From: Jörg <jörg@münchen.example>\r\n" +
	"Subject: grüße\r\n" +
	"Message-ID: <eai1@example.com>\r\n" +
	"\r\n" +
	"body\r\n"

// TestEmailParseEAI checks the section 4.9 MUST that Email/parse supports EAI
// headers: the internationalized From address survives round-trip.
func TestEmailParseEAI(t *testing.T) {
	ts, db, store := emailServer(t)
	blobID := uploadBlob(t, db, store, eaiMessage)

	em := doParse(t, ts, fmt.Sprintf(`{"accountId":%q,"blobIds":[%q],"properties":["from","subject"]}`,
		testAccount, blobID))["parsed"].(map[string]any)[blobID].(map[string]any)
	from := em["from"].([]any)
	if len(from) != 1 {
		t.Fatalf("from = %v", em["from"])
	}
	addr := from[0].(map[string]any)
	if addr["email"] != "jörg@münchen.example" || addr["name"] != "Jörg" {
		t.Errorf("from[0] = %v, want the EAI address preserved", addr)
	}
	if em["subject"] != "grüße" {
		t.Errorf("subject = %v, want the UTF-8 subject preserved", em["subject"])
	}
}
