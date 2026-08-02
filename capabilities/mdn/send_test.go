package mdn

// MDN/send tests over the wire path: the RFC 9007 section 3.1 sample
// round-trips shape-for-shape, and every SetError the method defines is
// reachable - mdnAlreadySent through both the visible keyword and the
// server's own issue record, notFound, forbiddenFrom, forbidden (the
// RFC 8098 section 2.1 automatic-send refusal), tooLarge, and the
// invalidProperties family including header-injection attempts.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/datatypes/mail/submit"
)

// receivedMsg is a received message requesting an MDN (RFC 8098 section
// 2.1), shaped after the RFC 9007 section 3 story: Joe asks John for a
// read receipt.
func receivedMsg(headers map[string]string) string {
	h := map[string]string{
		"Return-Path":                 "<joe@example.com>",
		"From":                        "Joe Bloggs <joe@example.com>",
		"To":                          "John <john@example.com>",
		"Subject":                     "World domination",
		"Message-ID":                  "<199509192301.23456@example.org>",
		"Disposition-Notification-To": "Joe Bloggs <joe@example.com>",
		"MIME-Version":                "1.0",
		"Content-Type":                "text/plain",
	}
	for name, v := range headers {
		if v == "" {
			delete(h, name)
		} else {
			h[name] = v
		}
	}
	// Stable field order so fixtures are reproducible; extra fields the
	// base set does not name follow the known ones.
	known := []string{"Return-Path", "From", "To", "Subject", "Message-ID", "Disposition-Notification-To", "MIME-Version", "Content-Type"}
	var b strings.Builder
	for _, name := range known {
		if v, ok := h[name]; ok {
			b.WriteString(name + ": " + v + "\r\n")
			delete(h, name)
		}
	}
	extra := make([]string, 0, len(h))
	for name := range h {
		extra = append(extra, name)
	}
	sort.Strings(extra)
	for _, name := range extra {
		b.WriteString(name + ": " + h[name] + "\r\n")
	}
	b.WriteString("\r\nHello John.\r\n")
	return b.String()
}

// sendEnv is a server prepared for sending: mailboxes, an identity, and
// helpers to import received fixtures.
type sendEnv struct {
	ts       *httptest.Server
	identity jmap.Id
	inbox    jmap.Id
	sent     jmap.Id
}

func setupSend(t *testing.T, limits *submit.Limits) *sendEnv {
	t.Helper()
	ts := newTestServerLimits(t, limits)
	e := &sendEnv{ts: ts, identity: createIdentity(t, ts)}
	r := call(t, ts, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Mailbox/set",{"accountId":"Atest1","create":{
			"mi":{"name":"Inbox","role":"inbox"},
			"ms":{"name":"Sent","role":"sent"}}},"0"]]}`)
	var out struct {
		Created map[string]struct {
			Id jmap.Id `json:"id"`
		} `json:"created"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	e.inbox, e.sent = out.Created["mi"].Id, out.Created["ms"].Id
	if e.inbox == "" || e.sent == "" {
		t.Fatalf("Mailbox/set: %s %s", r.MethodResponses[0].Name, r.MethodResponses[0].Args)
	}
	return e
}

// importMsg uploads raw and imports it into the inbox, returning the
// Email id.
func (e *sendEnv) importMsg(t *testing.T, raw string) jmap.Id {
	t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+"/upload/Atest1", strings.NewReader(raw))
	req.Header.Set("Content-Type", "message/rfc822")
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
		t.Fatalf("upload: status %d, blobId %q, err %v", res.StatusCode, up.BlobId, err)
	}
	r := call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/import",{"accountId":"Atest1","emails":{"e1":{"blobId":%q,"mailboxIds":{%q:true}}}},"0"]]}`,
		up.BlobId, e.inbox))
	var out struct {
		Created map[string]struct {
			Id jmap.Id `json:"id"`
		} `json:"created"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Created["e1"].Id == "" {
		t.Fatalf("Email/import: %s %s", r.MethodResponses[0].Name, r.MethodResponses[0].Args)
	}
	return out.Created["e1"].Id
}

// mdnSendCall posts one MDN/send with a single creation under id
// "k1546" and returns the response.
func (e *sendEnv) mdnSendCall(t *testing.T, mdnJSON, onSuccessJSON string) jmap.Response {
	t.Helper()
	return call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:mdn"],"methodCalls":[
		["MDN/send",{"accountId":"Atest1","identityId":%q,"send":{"k1546":%s},"onSuccessUpdateEmail":%s},"0"]]}`,
		e.identity, mdnJSON, onSuccessJSON))
}

// notSentError digs the SetError for creation id k1546 out of an
// MDN/send response.
func notSentError(t *testing.T, r jmap.Response) *jmap.SetError {
	t.Helper()
	if r.MethodResponses[0].Name != "MDN/send" {
		t.Fatalf("response = %s %s", r.MethodResponses[0].Name, r.MethodResponses[0].Args)
	}
	var out struct {
		Sent    json.RawMessage            `json:"sent"`
		NotSent map[jmap.Id]*jmap.SetError `json:"notSent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.NotSent["k1546"] == nil {
		t.Fatalf("no notSent entry: sent=%s args=%s", out.Sent, r.MethodResponses[0].Args)
	}
	return out.NotSent["k1546"]
}

// sampleMDN is the RFC 9007 section 3.1 request's MDN creation, with
// the sample's "extension" property under its section 2 name
// extensionFields (the normative property list defines no "extension"
// property; the sample predates it).
func sampleMDN(emailId jmap.Id) string {
	return fmt.Sprintf(`{
		"forEmailId": %q,
		"subject": "Read receipt for: World domination",
		"textBody": "This receipt shows that the email has been displayed on your recipient's computer. There is no guarantee it has been read or understood.",
		"reportingUA": "joes-pc.cs.example.com; Foomail 97.1",
		"disposition": {
			"actionMode": "manual-action",
			"sendingMode": "mdn-sent-manually",
			"type": "displayed"
		},
		"extensionFields": {"EXTENSION-EXAMPLE": "example.com"}
	}`, emailId)
}

const sampleOnSuccess = `{"#k1546": {"keywords/$mdnsent": true}}`

// TestSendSample runs the RFC 9007 section 3.1 exchange: the MDN is
// sent, the sent echo carries exactly the server-set properties the
// sample shows, and the mandated implicit Email/set follows under the
// same call id, leaving $mdnsent on the message.
func TestSendSample(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(nil))
	r := e.mdnSendCall(t, sampleMDN(emailId), sampleOnSuccess)

	if len(r.MethodResponses) != 2 {
		t.Fatalf("responses = %d, want MDN/send + implicit Email/set", len(r.MethodResponses))
	}
	var out struct {
		AccountID jmap.Id                     `json:"accountId"`
		Sent      map[jmap.Id]json.RawMessage `json:"sent"`
		NotSent   json.RawMessage             `json:"notSent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if string(out.NotSent) != "null" {
		t.Fatalf("notSent = %s", out.NotSent)
	}
	// The echo holds exactly the properties the server set or defaulted
	// (section 3.1): the identity-derived finalRecipient in typed form
	// and the original's Message-ID with its angle brackets.
	var echo map[string]string
	json.Unmarshal(out.Sent["k1546"], &echo)
	want := map[string]string{
		"finalRecipient":    "rfc822; john@example.com",
		"originalMessageId": "<199509192301.23456@example.org>",
	}
	if len(echo) != len(want) || echo["finalRecipient"] != want["finalRecipient"] || echo["originalMessageId"] != want["originalMessageId"] {
		t.Errorf("sent echo = %s, want %v", out.Sent["k1546"], want)
	}

	// The implicit Email/set (section 2.1) follows under the same call
	// id and applied the patch.
	if r.MethodResponses[1].Name != "Email/set" || r.MethodResponses[1].CallID != "0" {
		t.Fatalf("continuation = %s %q", r.MethodResponses[1].Name, r.MethodResponses[1].CallID)
	}
	var set struct {
		Updated map[jmap.Id]json.RawMessage `json:"updated"`
	}
	json.Unmarshal(r.MethodResponses[1].Args, &set)
	if _, ok := set.Updated[emailId]; !ok {
		t.Fatalf("implicit Email/set updated = %s", r.MethodResponses[1].Args)
	}
	g := call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/get",{"accountId":"Atest1","ids":[%q],"properties":["keywords"]},"0"]]}`, emailId))
	var got struct {
		List []struct {
			Keywords map[string]bool `json:"keywords"`
		} `json:"list"`
	}
	json.Unmarshal(g.MethodResponses[0].Args, &got)
	if len(got.List) != 1 || !got.List[0].Keywords["$mdnsent"] {
		t.Errorf("keywords after send = %s", g.MethodResponses[0].Args)
	}
}

// TestSendAlreadySent covers both duplicate defenses: the visible
// keyword (the section 3.1 error sample), and - after a client clears
// the keyword, which RFC 8621 lets it do - the server's internal issue
// record, which RFC 8098 section 2.1's one-MDN-per-message rule still
// enforces.
func TestSendAlreadySent(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(nil))
	r := e.mdnSendCall(t, sampleMDN(emailId), sampleOnSuccess)
	var first struct {
		Sent map[jmap.Id]json.RawMessage `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &first)
	if first.Sent["k1546"] == nil {
		t.Fatalf("first send failed: %s", r.MethodResponses[0].Args)
	}

	// Second attempt: the keyword is set, the section 3.1 error sample's
	// answer.
	serr := notSentError(t, e.mdnSendCall(t, sampleMDN(emailId), sampleOnSuccess))
	if serr.Type != "mdnAlreadySent" || serr.Description != "$mdnsent keyword is already present" {
		t.Errorf("keyword duplicate = %+v, want mdnAlreadySent with the keyword description", serr)
	}

	// The client clears $mdnsent - RFC 8621 gives the keyword no
	// protection - and retries. The server's issue record still refuses.
	c := call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/set",{"accountId":"Atest1","update":{%q:{"keywords/$mdnsent":null}}},"0"]]}`, emailId))
	var upd struct {
		Updated map[jmap.Id]json.RawMessage `json:"updated"`
	}
	json.Unmarshal(c.MethodResponses[0].Args, &upd)
	if _, ok := upd.Updated[emailId]; !ok {
		t.Fatalf("clearing $mdnsent failed: %s", c.MethodResponses[0].Args)
	}
	serr = notSentError(t, e.mdnSendCall(t, sampleMDN(emailId), sampleOnSuccess))
	if serr.Type != "mdnAlreadySent" || !strings.Contains(serr.Description, "server's record") {
		t.Errorf("cleared-keyword retry = %+v, want mdnAlreadySent citing the server's record", serr)
	}
}

// TestSendErrors walks every per-entry SetError.
func TestSendErrors(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(nil))

	entry := func(mdnJSON, onSuccess string) *jmap.SetError {
		t.Helper()
		return notSentError(t, e.mdnSendCall(t, mdnJSON, onSuccess))
	}
	mdnFor := func(id jmap.Id, mods string) string {
		s := fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}`, id)
		if mods != "" {
			s += "," + mods
		}
		return s + "}"
	}

	// notFound: unknown forEmailId.
	if serr := entry(mdnFor("Mnope", ""), sampleOnSuccess); serr.Type != "notFound" {
		t.Errorf("unknown forEmailId = %+v, want notFound", serr)
	}
	// notFound: no Disposition-Notification-To on the message.
	plain := e.importMsg(t, receivedMsg(map[string]string{"Disposition-Notification-To": "", "Message-ID": "<plain-1@example.org>"}))
	if serr := entry(mdnFor(plain, ""), sampleOnSuccess); serr.Type != "notFound" {
		t.Errorf("no DNT = %+v, want notFound", serr)
	}
	// forbiddenFrom: a finalRecipient the identity may not send as.
	if serr := entry(mdnFor(emailId, `"finalRecipient":"rfc822; other@example.net"`), sampleOnSuccess); serr.Type != "forbiddenFrom" {
		t.Errorf("foreign finalRecipient = %+v, want forbiddenFrom", serr)
	}
	// forbidden: automatic sending mode with a Return-Path that does not
	// match the notification address (RFC 8098 section 2.1).
	mismatched := e.importMsg(t, receivedMsg(map[string]string{"Return-Path": "<elsewhere@example.net>", "Message-ID": "<mismatch-1@example.org>"}))
	auto := fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"automatic-action","sendingMode":"mdn-sent-automatically","type":"displayed"}}`, mismatched)
	if serr := entry(auto, sampleOnSuccess); serr.Type != "forbidden" {
		t.Errorf("automatic send with mismatched Return-Path = %+v, want forbidden", serr)
	}
	// invalidProperties: a server-set property in the creation.
	if serr := entry(mdnFor(emailId, `"originalMessageId":"<x@example.org>"`), sampleOnSuccess); serr.Type != "invalidProperties" {
		t.Errorf("server-set property = %+v, want invalidProperties", serr)
	}
	// invalidProperties: no disposition.
	if serr := entry(fmt.Sprintf(`{"forEmailId":%q}`, emailId), sampleOnSuccess); serr.Type != "invalidProperties" {
		t.Errorf("missing disposition = %+v, want invalidProperties", serr)
	}
	// invalidProperties: an unknown disposition enum value (RFC 8098
	// section 3.2.6's vocabulary is closed).
	bad := fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"auto-action","sendingMode":"mdn-sent-manually","type":"displayed"}}`, emailId)
	if serr := entry(bad, sampleOnSuccess); serr.Type != "invalidProperties" {
		t.Errorf("bad enum = %+v, want invalidProperties", serr)
	}
	// invalidProperties: a header injection attempt through subject.
	if serr := entry(mdnFor(emailId, `"subject":"receipt\r\nBcc: victim@example.net"`), sampleOnSuccess); serr.Type != "invalidProperties" {
		t.Errorf("subject injection = %+v, want invalidProperties", serr)
	}
	// invalidProperties: the entry's onSuccessUpdateEmail patch does not
	// set $mdnsent (section 2.1 server check), here by setting it false.
	if serr := entry(mdnFor(emailId, ""), `{"#k1546": {"keywords/$mdnsent": false}}`); serr.Type != "invalidProperties" {
		t.Errorf("patch not setting $mdnsent = %+v, want invalidProperties", serr)
	}
	// invalidProperties: no patch for the entry at all.
	if serr := entry(mdnFor(emailId, ""), `{"#other": {"keywords/$mdnsent": true}}`); serr.Type != "invalidProperties" {
		t.Errorf("missing patch entry = %+v, want invalidProperties", serr)
	}
}

// TestSendCallErrors covers the call-level rejections of section 2.1.
func TestSendCallErrors(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(nil))

	// An unknown identityId rejects the call.
	r := call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:mdn"],"methodCalls":[
		["MDN/send",{"accountId":"Atest1","identityId":"Inope","send":{"k1546":%s},"onSuccessUpdateEmail":%s},"0"]]}`,
		sampleMDN(emailId), sampleOnSuccess))
	assertMethodError(t, r, "invalidArguments")

	// A non-empty send map with a null onSuccessUpdateEmail cannot set
	// $mdnsent, so the call is rejected.
	r = e.mdnSendCall(t, sampleMDN(emailId), "null")
	assertMethodError(t, r, "invalidArguments")
}

// TestSendOversizedOriginal includes an original too large to return
// whole: the third component degrades to the header block
// (text/rfc822-headers, RFC 6522 section 4) instead of the send
// failing, since the client has no reduced form to ask for itself.
func TestSendOversizedOriginal(t *testing.T) {
	e := setupSend(t, nil)
	raw := receivedMsg(nil) + strings.Repeat("padding line of original body\r\n", 50000)
	emailId := e.importMsg(t, raw)
	mdn := fmt.Sprintf(`{"forEmailId":%q,"includeOriginalMessage":true,"disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}`, emailId)
	r := e.mdnSendCall(t, mdn, sampleOnSuccess)
	var out struct {
		Sent map[jmap.Id]json.RawMessage `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Sent["k1546"] == nil {
		t.Fatalf("oversized original refused: %s", r.MethodResponses[0].Args)
	}
	sent := e.sentMDNRaw(t)
	if !strings.Contains(sent, "text/rfc822-headers") {
		t.Error("third component is not the header-block form")
	}
	if strings.Contains(sent, "padding line of original body") {
		t.Error("original body reached the wire despite the whole-message bound")
	}
	if !strings.Contains(sent, "Subject: World domination") {
		t.Error("original header block missing from the third component")
	}
}

// TestSendTooLarge pins the assembly backstop: an original whose header
// block alone outgrows the assembly bound cannot even be returned in
// reduced form, and the entry fails as tooLarge with the bound
// advertised.
func TestSendTooLarge(t *testing.T) {
	e := setupSend(t, nil)
	extras := map[string]string{}
	for i := 0; i < 3000; i++ {
		extras[fmt.Sprintf("X-Pad-%04d", i)] = strings.Repeat("p", 900)
	}
	emailId := e.importMsg(t, receivedMsg(extras))
	mdn := fmt.Sprintf(`{"forEmailId":%q,"includeOriginalMessage":true,"disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}`, emailId)
	serr := notSentError(t, e.mdnSendCall(t, mdn, sampleOnSuccess))
	if serr.Type != "tooLarge" || serr.MaxSize != 2<<20 {
		t.Errorf("oversized header block = %+v, want tooLarge with the assembly bound", serr)
	}
}

func assertMethodError(t *testing.T, r jmap.Response, errType string) {
	t.Helper()
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("response = %s %s", r.MethodResponses[0].Name, r.MethodResponses[0].Args)
	}
	var e jmap.MethodError
	json.Unmarshal(r.MethodResponses[0].Args, &e)
	if e.Type != errType {
		t.Errorf("error type = %q, want %q", e.Type, errType)
	}
}
