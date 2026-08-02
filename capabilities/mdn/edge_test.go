package mdn

// Edge and hostile cases beyond the samples: account errors, the RFC
// 8098 section 2.1 automatic-send refusals in every trigger, mixed
// success and failure in one call, correlation ambiguity, header
// injection through every client-controlled string, forged $mdnsent
// patch shapes, and blobs built to be mistaken for MDNs.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// TestSendUnknownAccount pins the unknown-account rejection. RFC 8620
// section 3.6.2 defines accountNotFound for an accountId that does not
// exist, and the runtime applies it uniformly; RFC 9007's prose names
// invalidArguments, but the more specific standard error wins.
func TestSendUnknownAccount(t *testing.T) {
	e := setupSend(t, nil)
	r := call(t, e.ts, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:mdn"],"methodCalls":[
		["MDN/send",{"accountId":"Anope","identityId":"I1","send":{},"onSuccessUpdateEmail":null},"0"],
		["MDN/parse",{"accountId":"Anope","blobIds":[]},"1"]]}`)
	assertMethodError(t, r, "accountNotFound")
	if r.MethodResponses[1].Name != "error" {
		t.Fatalf("MDN/parse = %s", r.MethodResponses[1].Name)
	}
}

// TestSendAutomaticRefusals walks every automatic-send trigger of RFC
// 8098 section 2.1: no Return-Path, more than one notification
// address, and more than one Return-Path field all refuse an
// mdn-sent-automatically MDN, while the matching single-address case
// goes through.
func TestSendAutomaticRefusals(t *testing.T) {
	e := setupSend(t, nil)
	auto := func(id jmap.Id) string {
		return fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"automatic-action","sendingMode":"mdn-sent-automatically","type":"processed"}}`, id)
	}

	noRP := e.importMsg(t, receivedMsg(map[string]string{"Return-Path": "", "Message-ID": "<no-rp@example.org>"}))
	if serr := notSentError(t, e.mdnSendCall(t, auto(noRP), sampleOnSuccess)); serr.Type != "forbidden" {
		t.Errorf("no Return-Path = %+v, want forbidden", serr)
	}
	multiDNT := e.importMsg(t, receivedMsg(map[string]string{
		"Disposition-Notification-To": "joe@example.com, second@example.net",
		"Message-ID":                  "<multi-dnt@example.org>",
	}))
	if serr := notSentError(t, e.mdnSendCall(t, auto(multiDNT), sampleOnSuccess)); serr.Type != "forbidden" {
		t.Errorf("multiple DNT addresses = %+v, want forbidden", serr)
	}
	ok := e.importMsg(t, receivedMsg(map[string]string{"Message-ID": "<auto-ok@example.org>"}))
	r := e.mdnSendCall(t, auto(ok), sampleOnSuccess)
	var out struct {
		Sent map[jmap.Id]json.RawMessage `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Sent["k1546"] == nil {
		t.Errorf("matching automatic send failed: %s", r.MethodResponses[0].Args)
	}
}

// TestSendMultipleDNTManual sends manually for a message whose
// notification request names two addresses: the user's consent covers
// the send (RFC 8098 section 2.1), and the MDN goes to the first.
func TestSendMultipleDNTManual(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(map[string]string{
		"Disposition-Notification-To": "first@example.com, second@example.net",
	}))
	mdnJSON := fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}`, emailId)
	r := e.mdnSendCall(t, mdnJSON, sampleOnSuccess)
	var out struct {
		Sent map[jmap.Id]json.RawMessage `json:"sent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Sent["k1546"] == nil {
		t.Fatalf("manual multi-DNT send failed: %s", r.MethodResponses[0].Args)
	}
	if _, rcptTo := e.submissionEnvelope(t); len(rcptTo) != 1 || rcptTo[0].Email != "first@example.com" {
		t.Errorf("rcptTo = %+v, want the first notification address", rcptTo)
	}
}

// TestSendMixedEntries submits one good and one bad creation in a
// single call: the good one sends, the bad one gets its SetError, and
// the implicit Email/set touches only the good one's message.
func TestSendMixedEntries(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(nil))
	r := call(t, e.ts, fmt.Sprintf(`{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:mdn"],"methodCalls":[
		["MDN/send",{"accountId":"Atest1","identityId":%q,"send":{
			"good":{"forEmailId":%q,"disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}},
			"bad":{"forEmailId":"Mnope","disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}},
			"onSuccessUpdateEmail":{"#good":{"keywords/$mdnsent":true},"#bad":{"keywords/$mdnsent":true}}},"0"]]}`,
		e.identity, emailId))
	var out struct {
		Sent    map[jmap.Id]json.RawMessage `json:"sent"`
		NotSent map[jmap.Id]*jmap.SetError  `json:"notSent"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	if out.Sent["good"] == nil || out.NotSent["bad"] == nil || out.NotSent["bad"].Type != "notFound" {
		t.Fatalf("mixed call = %s", r.MethodResponses[0].Args)
	}
	if len(r.MethodResponses) != 2 || r.MethodResponses[1].Name != "Email/set" {
		t.Fatalf("continuation missing: %d responses", len(r.MethodResponses))
	}
	var set struct {
		Updated map[jmap.Id]json.RawMessage `json:"updated"`
	}
	json.Unmarshal(r.MethodResponses[1].Args, &set)
	if len(set.Updated) != 1 {
		t.Errorf("implicit set updated %d emails, want only the sent one: %s", len(set.Updated), r.MethodResponses[1].Args)
	}
	if _, ok := set.Updated[emailId]; !ok {
		t.Errorf("implicit set missed %s: %s", emailId, r.MethodResponses[1].Args)
	}
}

// TestParseAmbiguousCorrelation submits two messages sharing one
// Message-ID: the MDN referencing it identifies nothing, so forEmailId
// is null (RFC 9007 section 2.2 blesses exactly this).
func TestParseAmbiguousCorrelation(t *testing.T) {
	e := setupSend(t, nil)
	e.submitOriginal(t)
	e.submitOriginal(t)
	blobId := e.uploadBlob(t, "message/rfc822", sampleMDNWire(t))
	r := parseCall(t, e, fmt.Sprintf(`[%q]`, blobId))
	var out struct {
		Parsed map[jmap.Id]map[string]json.RawMessage `json:"parsed"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	got := out.Parsed[blobId]
	if got == nil {
		t.Fatalf("not parsed: %s", r.MethodResponses[0].Args)
	}
	if string(got["forEmailId"]) != "null" {
		t.Errorf("forEmailId = %s, want null on an ambiguous Message-ID", got["forEmailId"])
	}
}

// TestSendInjection drives a line break through every client string
// that lands in a header position; each attempt dies as a per-entry
// SetError, never reaching the wire.
func TestSendInjection(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(nil))
	mdnWith := func(mods string) string {
		return fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"},%s}`, emailId, mods)
	}
	cases := []struct {
		name, mods, wantType string
	}{
		{"reportingUA", `"reportingUA":"ua\r\nX-Evil: 1"`, "invalidProperties"},
		{"extension name", `"extensionFields":{"X-E\r\nvil":"v"}`, "invalidProperties"},
		{"extension value", `"extensionFields":{"X-Evil":"v\r\nInjected: 1"}`, "invalidProperties"},
		// An extension field may not restate a defined notification
		// field (RFC 8098 section 3.3 vs the section 3.1 grammar).
		{"extension shadows Disposition", `"extensionFields":{"Disposition":"manual-action/mdn-sent-manually; deleted"}`, "invalidProperties"},
		// An injected finalRecipient fails validation at the property,
		// before the identity allow-check ever sees it.
		{"finalRecipient", `"finalRecipient":"rfc822; john@example.com\r\nX-Evil: 1"`, "invalidProperties"},
	}
	for _, c := range cases {
		if serr := notSentError(t, e.mdnSendCall(t, mdnWith(c.mods), sampleOnSuccess)); serr.Type != c.wantType {
			t.Errorf("%s = %+v, want %s", c.name, serr, c.wantType)
		}
	}
}

// TestSendForgedPatches tries onSuccessUpdateEmail shapes that look
// close to setting $mdnsent but do not: the section 2.1 server check
// must reject each ($mdnsent is lowercase-exact per section 1.2).
func TestSendForgedPatches(t *testing.T) {
	e := setupSend(t, nil)
	emailId := e.importMsg(t, receivedMsg(nil))
	mdnJSON := fmt.Sprintf(`{"forEmailId":%q,"disposition":{"actionMode":"manual-action","sendingMode":"mdn-sent-manually","type":"displayed"}}`, emailId)
	for _, patch := range []string{
		`{"#k1546":{"keywords/$MDNSENT":true}}`,
		`{"#k1546":{"keywords/$mdnsent":"true"}}`,
		`{"#k1546":{"keywords/$mdnsent":1}}`,
		`{"#k1546":{"somethingelse":true}}`,
		`{"#k1546":{"keywords":{"$MDNSENT":true}}}`,
		`{"#k1546":{"keywords":{}}}`,
		`{"#k1546":true}`,
		`{"k1546":{"keywords/$mdnsent":true}}`,
	} {
		if serr := notSentError(t, e.mdnSendCall(t, mdnJSON, patch)); serr.Type != "invalidProperties" {
			t.Errorf("patch %s = %+v, want invalidProperties", patch, serr)
		}
	}
}

// TestParseHostileBlobs uploads blobs built to be mistaken for MDNs:
// each is notParsable, never an error and never parsed.
func TestParseHostileBlobs(t *testing.T) {
	e := setupSend(t, nil)
	sample := sampleMDNWire(t)
	blobs := map[string]string{
		// The right parts under the wrong container type (RFC 6522: the
		// container must be multipart/report).
		"wrong container": strings.Replace(sample, "multipart/report; report-type=disposition-notification;", "multipart/mixed;", 1),
		// Truncated mid-headers: no machine part survives.
		"truncated": sample[:100],
		// A delivery status report, not a disposition notification.
		"dsn": "Content-Type: multipart/report; report-type=delivery-status; boundary=DD\r\n" +
			"\r\n--DD\r\nContent-Type: text/plain\r\n\r\nfailed\r\n" +
			"\r\n--DD\r\nContent-Type: message/delivery-status\r\n\r\n" +
			"Reporting-MTA: dns; mx.example.net\r\n\r\n" +
			"Final-Recipient: rfc822; joe@example.com\r\nAction: failed\r\n" +
			"\r\n--DD--\r\n",
		// A notification whose Disposition uses a word outside the
		// closed RFC 8098 section 3.2.6 vocabulary.
		"invented disposition": strings.Replace(sample, "manual-action/mdn-sent-manually; displayed", "manual-action/mdn-sent-manually; memorized", 1),
		"empty":                "",
	}
	for name, raw := range blobs {
		blobId := e.uploadBlob(t, "message/rfc822", raw)
		r := parseCall(t, e, fmt.Sprintf(`[%q]`, blobId))
		var out struct {
			Parsed      json.RawMessage `json:"parsed"`
			NotParsable []jmap.Id       `json:"notParsable"`
		}
		json.Unmarshal(r.MethodResponses[0].Args, &out)
		if string(out.Parsed) != "null" || len(out.NotParsable) != 1 {
			t.Errorf("%s: %s", name, r.MethodResponses[0].Args)
		}
	}
}

// TestParseHeadersOnlyOriginal recognizes a returned original in the
// text/rfc822-headers form (RFC 6522 section 4) as a third component.
func TestParseHeadersOnlyOriginal(t *testing.T) {
	e := setupSend(t, nil)
	raw := "From: gw@example.net\r\nTo: john@example.com\r\nSubject: receipt\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/report; report-type=disposition-notification; boundary=HH\r\n" +
		"\r\n--HH\r\nContent-Type: text/plain\r\n\r\nDisplayed.\r\n" +
		"\r\n--HH\r\nContent-Type: message/disposition-notification\r\n\r\n" +
		"Final-Recipient: rfc822; john@example.net\r\n" +
		"Disposition: manual-action/mdn-sent-manually; displayed\r\n" +
		"\r\n--HH\r\nContent-Type: text/rfc822-headers\r\n\r\n" +
		"Subject: the original\r\n" +
		"\r\n--HH--\r\n"
	blobId := e.uploadBlob(t, "message/rfc822", raw)
	r := parseCall(t, e, fmt.Sprintf(`[%q]`, blobId))
	var out struct {
		Parsed map[jmap.Id]map[string]json.RawMessage `json:"parsed"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &out)
	got := out.Parsed[blobId]
	if got == nil {
		t.Fatalf("not parsed: %s", r.MethodResponses[0].Args)
	}
	if string(got["includeOriginalMessage"]) != "true" {
		t.Errorf("includeOriginalMessage = %s, want true for a headers-only original", got["includeOriginalMessage"])
	}
}
