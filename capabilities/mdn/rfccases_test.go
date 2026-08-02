package mdn

// Cases the RFCs themselves spell out, mirrored as directly as the
// protocol allows: the RFC 9007 section 3.2 exchange, and the RFC 8098
// section 2.1 rules with no test elsewhere - the MDN-for-an-MDN
// refusal, the exactly-one-field rule, the address comparison's case
// rules, and the section 3 requirement that a generated MDN carries
// its own Message-ID - plus the section 2.2 suppression on a
// Disposition-Notification-Options field.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// TestAskForMDNSample runs the RFC 9007 section 3.2 exchange: the
// notification request travels as an ordinary header on an Email/set
// draft (header:Disposition-Notification-To:asText), read back in the
// same form.
func TestAskForMDNSample(t *testing.T) {
	e := setupSend(t, nil)
	r := call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/set",{"accountId":"Atest1","create":{"k2657":{
			"mailboxIds":{%q:true},
			"keywords":{"$seen":true,"$draft":true},
			"from":[{"name":"Joe Bloggs","email":"joe@example.com"}],
			"to":[{"name":"John","email":"john@example.com"}],
			"header:Disposition-Notification-To:asText":"joe@example.com",
			"subject":"World domination"
		}}},"0"]]}`, e.inbox))
	var set struct {
		Created map[string]struct {
			Id jmap.Id `json:"id"`
		} `json:"created"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &set)
	if set.Created["k2657"].Id == "" {
		t.Fatalf("draft not created: %s %s", r.MethodResponses[0].Name, r.MethodResponses[0].Args)
	}
	r = call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/get",{"accountId":"Atest1","ids":[%q],
			"properties":["header:Disposition-Notification-To:asText"]},"0"]]}`, set.Created["k2657"].Id))
	var got struct {
		List []map[string]string `json:"list"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &got)
	if len(got.List) != 1 || got.List[0]["header:Disposition-Notification-To:asText"] != "joe@example.com" {
		t.Errorf("header read-back = %s", r.MethodResponses[0].Args)
	}
}

// TestSendForMDNRefused answers a message that is itself a disposition
// notification: refused (RFC 8098 section 2.1: an MDN MUST NOT be
// generated in response to an MDN).
func TestSendForMDNRefused(t *testing.T) {
	e := setupSend(t, nil)
	mdnMsg := e.importMsg(t, receivedMsg(map[string]string{
		"Content-Type": "multipart/report; report-type=disposition-notification; boundary=ZZ",
		"Message-ID":   "<mdn-msg-1@example.org>",
	}))
	serr := notSentError(t, e.mdnSendCall(t, sampleMDN(mdnMsg), sampleOnSuccess))
	if serr.Type != "notFound" {
		t.Errorf("MDN for an MDN = %+v, want notFound", serr)
	}
}

// TestSendDuplicateDNTField refuses a message carrying the field
// twice: Disposition-Notification-To appears at most once (RFC 8098
// section 2.1), so a doubled field is no valid request in either
// sending mode.
func TestSendDuplicateDNTField(t *testing.T) {
	e := setupSend(t, nil)
	raw := strings.Replace(receivedMsg(map[string]string{"Message-ID": "<dnt-dup-1@example.org>"}),
		"MIME-Version:", "Disposition-Notification-To: second@example.net\r\nMIME-Version:", 1)
	emailId := e.importMsg(t, raw)
	serr := notSentError(t, e.mdnSendCall(t, sampleMDN(emailId), sampleOnSuccess))
	if serr.Type != "notFound" {
		t.Errorf("doubled Disposition-Notification-To = %+v, want notFound", serr)
	}
}

// TestSendNotificationOptions refuses a message carrying a
// Disposition-Notification-Options field (RFC 8098 section 2.2): a
// required-importance parameter that is not understood suppresses the
// MDN, no parameters are understood, and sending despite optional-only
// parameters is a MAY - so presence suppresses in both sending modes,
// contents never interpreted.
func TestSendNotificationOptions(t *testing.T) {
	e := setupSend(t, nil)
	for _, tc := range []struct {
		name, options string
	}{
		{"required", "signed-receipt-protocol=required,pkcs7-signature"},
		{"optional-only", "signed-receipt-protocol=optional,pkcs7-signature"},
		{"malformed", "not a parameter list"},
	} {
		emailId := e.importMsg(t, receivedMsg(map[string]string{
			"Disposition-Notification-Options": tc.options,
			"Message-ID":                       fmt.Sprintf("<dno-%s-1@example.org>", tc.name),
		}))
		serr := notSentError(t, e.mdnSendCall(t, sampleMDN(emailId), sampleOnSuccess))
		if serr.Type != "forbidden" {
			t.Errorf("%s Disposition-Notification-Options = %+v, want forbidden", tc.name, serr)
		}
	}
}

// TestSendAutomaticAddressCase compares the notification address to
// the Return-Path address the way RFC 8098 section 2.1 prescribes:
// the local part case-sensitively, the domain case-insensitively.
func TestSendAutomaticAddressCase(t *testing.T) {
	e := setupSend(t, nil)
	auto := func(id jmap.Id) jmap.Response {
		m := fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"automatic-action","sendingMode":"mdn-sent-automatically","type":"processed"}}`, id)
		return e.mdnSendCall(t, m, sampleOnSuccess)
	}

	localCase := e.importMsg(t, receivedMsg(map[string]string{
		"Disposition-Notification-To": "<Joe@example.com>",
		"Message-ID":                  "<case-local-1@example.org>",
	}))
	if serr := notSentError(t, auto(localCase)); serr.Type != "forbidden" {
		t.Errorf("local-part case mismatch = %+v, want forbidden", serr)
	}

	domainCase := e.importMsg(t, receivedMsg(map[string]string{
		"Disposition-Notification-To": "<joe@EXAMPLE.COM>",
		"Message-ID":                  "<case-domain-1@example.org>",
	}))
	r := auto(domainCase)
	var out struct {
		Sent map[jmap.Id]json.RawMessage `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Sent["k1546"] == nil {
		t.Errorf("domain case mismatch refused: %s", r.MethodResponses[0].Args)
	}
}

// TestSendMessageIDDiffers: a generated MDN carries its own Message-ID,
// different from the original message's (RFC 8098 section 3).
func TestSendMessageIDDiffers(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(nil))
	r := e.mdnSendCall(t, sampleMDN(emailId), sampleOnSuccess)
	var out struct {
		Sent map[jmap.Id]json.RawMessage `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Sent["k1546"] == nil {
		t.Fatalf("send failed: %s", r.MethodResponses[0].Args)
	}
	top, _, _ := strings.Cut(e.sentMDNRaw(t), "\r\n\r\n")
	var msgID string
	for _, line := range strings.Split(top, "\r\n") {
		if v, ok := strings.CutPrefix(line, "Message-ID:"); ok {
			msgID = strings.TrimSpace(v)
		}
	}
	if msgID == "" {
		t.Fatal("generated MDN has no Message-ID")
	}
	if msgID == "<199509192301.23456@example.org>" {
		t.Error("generated MDN reuses the original message's Message-ID")
	}
}
