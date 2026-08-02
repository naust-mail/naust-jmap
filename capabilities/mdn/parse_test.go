package mdn

// MDN/parse tests over the wire path: the RFC 9007 section 3.3 sample
// parses exactly (including the forEmailId correlation through the
// submission Message-ID index), the notFound and notParsable response
// shapes match the section's samples, a foreign MDN's fields that the
// send side never generates (MDN-Gateway, Error, extension fields)
// round-trip, and the id cap returns requestTooLarge.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/datatypes/mail/report"
)

// uploadBlob uploads raw content and returns its blob id.
func (e *sendEnv) uploadBlob(t *testing.T, contentType, raw string) jmap.Id {
	t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+"/upload/Atest1", strings.NewReader(raw))
	req.Header.Set("Content-Type", contentType)
	req.SetBasicAuth("john@example.com", "secret")
	res, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var up struct {
		BlobId jmap.Id `json:"blobId"`
	}
	if err := json.NewDecoder(res.Body).Decode(&up); err != nil || up.BlobId == "" {
		t.Fatalf("upload: status %d, err %v", res.StatusCode, err)
	}
	return up.BlobId
}

// submitOriginal imports a message the account sent (the RFC 9007
// section 3 story's original, Message-ID
// <199509192301.23456@example.org>) and creates its EmailSubmission, so
// the Message-ID index can answer the MDN/parse correlation.
func (e *sendEnv) submitOriginal(t *testing.T) jmap.Id {
	t.Helper()
	emailId := e.importMsg(t, strings.Join([]string{
		"From: John <john@example.com>",
		"To: Joe Bloggs <joe@example.com>",
		"Subject: World domination",
		"Date: Tue, 19 Sep 1995 23:01:00 +0000",
		"Message-ID: <199509192301.23456@example.org>",
		"MIME-Version: 1.0",
		"Content-Type: text/plain",
		"",
		"How about it?",
		"",
	}, "\r\n"))
	r := call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],"methodCalls":[
		["EmailSubmission/set",{"accountId":"Atest1","create":{"s1":{"identityId":%q,"emailId":%q}}},"0"]]}`,
		e.identity, emailId))
	var out struct {
		Created map[string]json.RawMessage `json:"created"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Created["s1"] == nil {
		t.Fatalf("EmailSubmission/set: %s %s", r.MethodResponses[0].Name, r.MethodResponses[0].Args)
	}
	return emailId
}

// sampleMDNWire renders the MDN of the section 3.3 sample as a wire
// message, through the same writer the send side uses.
func sampleMDNWire(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	err := report.Write(context.Background(), &buf, report.Message{
		From:              report.Address{Name: "John", Email: "john@example.com"},
		To:                report.Address{Name: "Joe Bloggs", Email: "joe@example.com"},
		Subject:           "Read receipt for: World domination",
		TextBody:          "This receipt shows that the email has been displayed on your recipient's computer. There is no guarantee it has been read or understood.",
		ReportingUA:       "joes-pc.cs.example.com; Foomail 97.1",
		FinalRecipient:    "john@example.com",
		OriginalMessageID: "199509192301.23456@example.org",
		Disposition: report.Disposition{
			ActionMode:  "manual-action",
			SendingMode: "mdn-sent-manually",
			Type:        "displayed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// assertProps compares each property of a parsed MDN against its
// expected JSON, comparing decoded values so encoder escaping does not
// matter.
func assertProps(t *testing.T, got map[string]json.RawMessage, want map[string]string) {
	t.Helper()
	for prop, w := range want {
		raw, ok := got[prop]
		if !ok {
			t.Errorf("%s missing", prop)
			continue
		}
		var gv, wv any
		json.Unmarshal(raw, &gv)
		json.Unmarshal([]byte(w), &wv)
		if !reflect.DeepEqual(gv, wv) {
			t.Errorf("%s = %s, want %s", prop, raw, w)
		}
	}
}

func parseCall(t *testing.T, e *sendEnv, blobIdsJSON string) jmap.Response {
	t.Helper()
	return call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:mdn"],"methodCalls":[
		["MDN/parse",{"accountId":"Atest1","blobIds":%s},"0"]]}`, blobIdsJSON))
}

// TestParseSample runs the RFC 9007 section 3.3 exchange: the parsed
// map holds the full MDN object with every sample property, and
// forEmailId resolves to the submitted original.
func TestParseSample(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.submitOriginal(t)
	blobId := e.uploadBlob(t, "message/rfc822", sampleMDNWire(t))

	r := parseCall(t, e, fmt.Sprintf(`[%q]`, blobId))
	var out struct {
		AccountID   jmap.Id                                `json:"accountId"`
		Parsed      map[jmap.Id]map[string]json.RawMessage `json:"parsed"`
		NotParsable json.RawMessage                        `json:"notParsable"`
		NotFound    json.RawMessage                        `json:"notFound"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if string(out.NotParsable) != "null" || string(out.NotFound) != "null" {
		t.Fatalf("notParsable=%s notFound=%s", out.NotParsable, out.NotFound)
	}
	got := out.Parsed[blobId]
	if got == nil {
		t.Fatalf("no parsed entry: %s", r.MethodResponses[0].Args)
	}
	want := map[string]string{
		"forEmailId":             fmt.Sprintf("%q", emailId),
		"subject":                `"Read receipt for: World domination"`,
		"textBody":               `"This receipt shows that the email has been displayed on your recipient's computer. There is no guarantee it has been read or understood."`,
		"reportingUA":            `"joes-pc.cs.example.com; Foomail 97.1"`,
		"disposition":            `{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}`,
		"finalRecipient":         `"rfc822; john@example.com"`,
		"originalMessageId":      `"<199509192301.23456@example.org>"`,
		"includeOriginalMessage": `false`,
		"mdnGateway":             `null`,
		"originalRecipient":      `null`,
		"error":                  `null`,
		"extensionFields":        `null`,
	}
	assertProps(t, got, want)
}

// TestParseSampleErrors pins the section 3.3 notFound and notParsable
// response shapes.
func TestParseSampleErrors(t *testing.T) {
	e := setupSend(t, nil)

	r := parseCall(t, e, `["Gnope"]`)
	var out struct {
		Parsed      json.RawMessage `json:"parsed"`
		NotParsable json.RawMessage `json:"notParsable"`
		NotFound    []jmap.Id       `json:"notFound"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if string(out.Parsed) != "null" || string(out.NotParsable) != "null" || len(out.NotFound) != 1 || out.NotFound[0] != "Gnope" {
		t.Errorf("unknown blob = %s", r.MethodResponses[0].Args)
	}

	blobId := e.uploadBlob(t, "text/plain", "just some text, not an MDN")
	r = parseCall(t, e, fmt.Sprintf(`[%q]`, blobId))
	var out2 struct {
		Parsed      json.RawMessage `json:"parsed"`
		NotParsable []jmap.Id       `json:"notParsable"`
		NotFound    json.RawMessage `json:"notFound"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out2)
	if string(out2.Parsed) != "null" || string(out2.NotFound) != "null" || len(out2.NotParsable) != 1 || out2.NotParsable[0] != blobId {
		t.Errorf("non-MDN blob = %s", r.MethodResponses[0].Args)
	}
}

// foreignMDN is a received MDN carrying the fields the send side never
// generates: MDN-Gateway, Error fields, an extension field, and a
// returned original as the third component - plus mixed-case grammar
// words, which parse must lowercase (RFC 9007 section 2).
const foreignMDN = "From: gateway@example.net\r\n" +
	"To: john@example.com\r\n" +
	"Subject: Delivered elsewhere\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/report; report-type=disposition-notification; boundary=BB\r\n" +
	"\r\n--BB\r\n" +
	"Content-Type: text/plain\r\n\r\nYour message was dispatched onward.\r\n" +
	"\r\n--BB\r\n" +
	"Content-Type: message/disposition-notification\r\n\r\n" +
	"Reporting-UA: gw.example.net; ForeignGW 2\r\n" +
	"MDN-Gateway: smtp; gw.example.net\r\n" +
	"Original-Recipient: rfc822; john-alias@example.net\r\n" +
	"Final-Recipient: rfc822; john@example.net\r\n" +
	"Original-Message-ID: <unknown-original@example.org>\r\n" +
	"Disposition: Automatic-Action/MDN-Sent-Automatically; Dispatched\r\n" +
	"Error: first diagnostic\r\n" +
	"Error: second diagnostic\r\n" +
	"X-Vendor-Extension: vendor value\r\n" +
	"\r\n--BB\r\n" +
	"Content-Type: message/rfc822\r\n\r\n" +
	"Subject: the original\r\n\r\nbody\r\n" +
	"\r\n--BB--\r\n"

// TestParseForeignMDN parses a gatewayed MDN: every RFC 9007 section 2
// property surfaces, the case-insensitive wire grammar comes back
// lowercase, and the unknown Original-Message-ID leaves forEmailId
// null.
func TestParseForeignMDN(t *testing.T) {
	e := setupSend(t, nil)
	blobId := e.uploadBlob(t, "message/rfc822", foreignMDN)
	r := parseCall(t, e, fmt.Sprintf(`[%q]`, blobId))
	var out struct {
		Parsed map[jmap.Id]map[string]json.RawMessage `json:"parsed"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	got := out.Parsed[blobId]
	if got == nil {
		t.Fatalf("not parsed: %s", r.MethodResponses[0].Args)
	}
	want := map[string]string{
		"forEmailId":             `null`,
		"mdnGateway":             `"smtp; gw.example.net"`,
		"originalRecipient":      `"rfc822; john-alias@example.net"`,
		"finalRecipient":         `"rfc822; john@example.net"`,
		"originalMessageId":      `"<unknown-original@example.org>"`,
		"disposition":            `{"actionMode":"automatic-action","sendingMode":"mdn-sent-automatically","type":"dispatched"}`,
		"error":                  `["first diagnostic","second diagnostic"]`,
		"extensionFields":        `{"X-Vendor-Extension":"vendor value"}`,
		"includeOriginalMessage": `true`,
	}
	assertProps(t, got, want)
}

// TestParseRequestTooLarge pins the section 2.2 id cap.
func TestParseRequestTooLarge(t *testing.T) {
	e := setupSend(t, nil)
	ids := make([]string, 501)
	for i := range ids {
		ids[i] = fmt.Sprintf(`"G%d"`, i)
	}
	r := parseCall(t, e, "["+strings.Join(ids, ",")+"]")
	assertMethodError(t, r, "requestTooLarge")
}
