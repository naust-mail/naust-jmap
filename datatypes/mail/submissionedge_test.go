package mail

// EmailSubmission edge cases cited in the RFC text (RFC 8621 sections 7 +
// 7.5, RFC 4865, RFC 3461) that the main submission_test.go flows do not
// reach: header field multiplicity, wildcard identities at submission
// time, envelope SMTP parameters, FUTURERELEASE boundary values, group
// recipients, /changes + /queryChanges, and onSuccess entries that
// reference nothing in the call.

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// submitOne creates one submission from a raw create object and returns
// the full /set response.
func submitOne(t *testing.T, ts *httptest.Server, createObj string) *jmap.Response {
	t.Helper()
	return callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"s":%s}}`, testAccount, createObj), "0"))
}

// createdEcho digs the created echo for "s" out of a submission response,
// failing on rejection.
func createdEcho(t *testing.T, r *jmap.Response) map[string]any {
	t.Helper()
	created, ok := methodArgs(t, r, 0, "EmailSubmission/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", r.MethodResponses[0].Args)
	}
	return created["s"].(map[string]any)
}

// TestEmailSubmissionFieldMultiplicity: the section 7 derivation text
// rejects a message with "more than one Sender/From header field", a
// Sender must hold exactly one address (RFC 5322 section 3.6.2), a
// multi-address From WITH a Sender is legal and derives from the Sender,
// and invalidEmail lists ALL the invalid properties at once (7.5 SHOULD).
func TestEmailSubmissionFieldMultiplicity(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	mb := map[string]bool{drafts: true}
	date := "Date: Thu, 17 Jul 2026 10:00:00 +0000\r\n"

	// Two From fields.
	twoFrom := putEmail(t, db, store,
		"From: john@example.com\r\nFrom: also@example.com\r\nTo: j@x.example\r\n"+date+"\r\nb\r\n", mb, nil)
	r := submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, twoFrom))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidEmail" || e["properties"].([]any)[0] != "from" {
		t.Errorf("two From fields: %v", e)
	}
	// Two Sender fields.
	twoSender := putEmail(t, db, store,
		"From: john@example.com\r\nSender: a@example.com\r\nSender: b@example.com\r\nTo: j@x.example\r\n"+date+"\r\nb\r\n", mb, nil)
	r = submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, twoSender))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidEmail" || e["properties"].([]any)[0] != "sender" {
		t.Errorf("two Sender fields: %v", e)
	}
	// A Sender with two addresses.
	fatSender := putEmail(t, db, store,
		"From: john@example.com\r\nSender: a@example.com, b@example.com\r\nTo: j@x.example\r\n"+date+"\r\nb\r\n", mb, nil)
	r = submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, fatSender))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidEmail" || e["properties"].([]any)[0] != "sender" {
		t.Errorf("two-address Sender: %v", e)
	}
	// Everything wrong at once: no From, no Date -> both properties named.
	allBad := putEmail(t, db, store, "To: j@x.example\r\n\r\nb\r\n", mb, nil)
	r = submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, allBad))
	e := notCreatedErr(t, r, "s")
	props := e["properties"].([]any)
	if e["type"] != "invalidEmail" || len(props) != 2 || props[0] != "from" || props[1] != "sentAt" {
		t.Errorf("all-bad message: %v", e)
	}

	// Multi-address From with a Sender is legal (RFC 5322 3.6.2), and the
	// envelope derives from the Sender. A wildcard identity covers both
	// From addresses.
	wildcard := createIdentity(t, ts, "*@corp.example")
	multiFrom := putEmail(t, db, store,
		"From: dev@corp.example, lead@corp.example\r\nSender: dev@corp.example\r\nTo: j@x.example\r\n"+date+"\r\nb\r\n", mb, nil)
	r = submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, wildcard, multiFrom))
	env := createdEcho(t, r)["envelope"].(map[string]any)
	if env["mailFrom"].(map[string]any)["email"] != "dev@corp.example" {
		t.Errorf("multi-From with Sender derived mailFrom = %v", env["mailFrom"])
	}
}

// TestEmailSubmissionWildcardIdentity: a wildcard Identity at submission
// time - in-domain senders pass without substitution, an out-of-domain
// derived sender is forbiddenMailFrom (a pattern has no substitutable
// address), and an out-of-domain From is forbiddenFrom.
func TestEmailSubmissionWildcardIdentity(t *testing.T) {
	ts, db, store, policy := submissionServer(t)
	policy.Allow(testAccount, "mailer@service.example")
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	wildcard := createIdentity(t, ts, "*@corp.example")
	mb := map[string]bool{drafts: true}
	date := "Date: Thu, 17 Jul 2026 10:00:00 +0000\r\n"

	// In-domain From: derived mailFrom stays as-is.
	inDomain := putEmail(t, db, store,
		"From: dev@corp.example\r\nTo: j@x.example\r\n"+date+"\r\nb\r\n", mb, nil)
	r := submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, wildcard, inDomain))
	env := createdEcho(t, r)["envelope"].(map[string]any)
	if env["mailFrom"].(map[string]any)["email"] != "dev@corp.example" {
		t.Errorf("in-domain mailFrom = %v", env["mailFrom"])
	}

	// Out-of-domain derived sender (via Sender header): no substitution
	// target exists, so the create is refused.
	outSender := putEmail(t, db, store,
		"From: dev@corp.example\r\nSender: mailer@service.example\r\nTo: j@x.example\r\n"+date+"\r\nb\r\n", mb, nil)
	r = submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, wildcard, outSender))
	if e := notCreatedErr(t, r, "s"); e["type"] != "forbiddenMailFrom" {
		t.Errorf("out-of-domain derived sender: %v", e)
	}

	// Out-of-domain From with a supplied, policy-permitted envelope: the
	// From-vs-identity check refuses it.
	outFrom := putEmail(t, db, store,
		"From: john@example.com\r\nTo: j@x.example\r\n"+date+"\r\nb\r\n", mb, nil)
	r = submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q,
		"envelope":{"mailFrom":{"email":"dev@corp.example"},"rcptTo":[{"email":"j@x.example"}]}}`,
		wildcard, outFrom))
	if e := notCreatedErr(t, r, "s"); e["type"] != "forbiddenFrom" {
		t.Errorf("out-of-domain From: %v", e)
	}
}

// TestEmailSubmissionEnvelopeParameters: non-hold SMTP parameters (RFC
// 3461's RET/NOTIFY and a value-less parameter) survive into the stored
// envelope untouched - and an unchanged client envelope is NOT echoed -
// while malformed parameter names and non-printable values are refused
// (RFC 5321 4.1.2 esmtp-keyword; RFC 3461 section 4 printable US-ASCII).
func TestEmailSubmissionEnvelopeParameters(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	r := submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q,
		"envelope":{"mailFrom":{"email":"john@example.com","parameters":{"RET":"HDRS","BODY":null}},
		  "rcptTo":[{"email":"j@x.example","parameters":{"NOTIFY":"SUCCESS,FAILURE"}}]}}`,
		identityId, emailId))
	echo := createdEcho(t, r)
	if _, has := echo["envelope"]; has {
		t.Errorf("unchanged client envelope was echoed: %v", echo["envelope"])
	}
	subId := echo["id"].(string)
	r = callSub(t, ts, inv("EmailSubmission/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, testAccount, subId), "0"))
	env := methodArgs(t, r, 0, "EmailSubmission/get")["list"].([]any)[0].(map[string]any)["envelope"].(map[string]any)
	params := env["mailFrom"].(map[string]any)["parameters"].(map[string]any)
	if params["RET"] != "HDRS" {
		t.Errorf("mailFrom RET = %v", params)
	}
	if v, has := params["BODY"]; !has || v != nil {
		t.Errorf("value-less parameter: %v", params)
	}
	rcpt := env["rcptTo"].([]any)[0].(map[string]any)
	if rcpt["parameters"].(map[string]any)["NOTIFY"] != "SUCCESS,FAILURE" {
		t.Errorf("rcptTo NOTIFY = %v", rcpt)
	}

	// Malformed parameters are invalidProperties on the envelope. The
	// escaped values decode to a non-ASCII byte and a control character.
	for _, params := range []string{
		`{"BAD PARAM":"x"}`,
		`{"-LEADS":"x"}`,
		`{"RET":"caf\u00e9"}`,
		`{"RET":"a\u0001b"}`,
	} {
		r = submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q,
			"envelope":{"mailFrom":{"email":"john@example.com","parameters":%s},
			  "rcptTo":[{"email":"j@x.example"}]}}`, identityId, emailId, params))
		if e := notCreatedErr(t, r, "s"); e["type"] != "invalidProperties" {
			t.Errorf("parameters %s: %v", params, e)
		}
	}
}

// TestEmailSubmissionHoldEdges: FUTURERELEASE boundary values - a
// HOLDUNTIL already past releases immediately with sendAt as given, a
// HOLDFOR of 0 is below the RFC 4865 section 3 minimum, and HOLDUNTIL
// beyond maxDelayedSend is refused like its HOLDFOR twin.
func TestEmailSubmissionHoldEdges(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	hold := func(params string) *jmap.Response {
		return submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q,
			"envelope":{"mailFrom":{"email":"john@example.com","parameters":%s},
			  "rcptTo":[{"email":"j@x.example"}]}}`, identityId, emailId, params))
	}

	past := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	r := hold(fmt.Sprintf(`{"HOLDUNTIL":%q}`, past.Format(time.RFC3339)))
	if got := createdEcho(t, r)["sendAt"].(string); got != past.Format(time.RFC3339) {
		t.Errorf("past HOLDUNTIL sendAt = %v, want %v", got, past.Format(time.RFC3339))
	}

	r = hold(`{"HOLDFOR":"0"}`)
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidProperties" {
		t.Errorf("HOLDFOR 0: %v", e)
	}
	over := time.Now().UTC().Add(time.Duration(DefaultSubmissionLimits().MaxDelayedSend)*time.Second + time.Hour)
	r = hold(fmt.Sprintf(`{"HOLDUNTIL":%q}`, over.Format(time.RFC3339)))
	if e := notCreatedErr(t, r, "s"); e["type"] != "invalidProperties" {
		t.Errorf("over-limit HOLDUNTIL: %v", e)
	}
}

// TestEmailSubmissionGroupRecipients: rcptTo derivation flattens RFC 5322
// group syntax - section 7 wants "the email addresses" of To/Cc/Bcc,
// however they are written.
func TestEmailSubmissionGroupRecipients(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	msg := "From: john@example.com\r\n" +
		"To: Team:a@x.example,b@x.example;, c@x.example\r\n" +
		"Date: Thu, 17 Jul 2026 10:00:00 +0000\r\n\r\nb\r\n"
	emailId := putEmail(t, db, store, msg, map[string]bool{drafts: true}, nil)

	r := submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, emailId))
	var got []string
	for _, a := range createdEcho(t, r)["envelope"].(map[string]any)["rcptTo"].([]any) {
		got = append(got, a.(map[string]any)["email"].(string))
	}
	want := []string{"a@x.example", "b@x.example", "c@x.example"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("group rcptTo = %v, want %v", got, want)
	}
}

// TestEmailSubmissionChanges: /changes (7.2) tracks the submission
// lifecycle; /queryChanges (7.4) validates its arguments through the same
// sort semantics as /query and then answers cannotCalculateChanges (this
// server's /query always advertises canCalculateChanges: false).
func TestEmailSubmissionChanges(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	r := callSub(t, ts, inv("EmailSubmission/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[]}`, testAccount), "0"))
	state0 := methodArgs(t, r, 0, "EmailSubmission/get")["state"].(string)
	r = callSub(t, ts, inv("EmailSubmission/query",
		fmt.Sprintf(`{"accountId":%q}`, testAccount), "0"))
	q := methodArgs(t, r, 0, "EmailSubmission/query")
	if q["canCalculateChanges"] != false {
		t.Errorf("canCalculateChanges = %v", q["canCalculateChanges"])
	}
	queryState0 := q["queryState"].(string)

	r = submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, emailId))
	subId := createdEcho(t, r)["id"].(string)

	r = callSub(t, ts, inv("EmailSubmission/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, state0), "0"))
	ch := methodArgs(t, r, 0, "EmailSubmission/changes")
	if created := ch["created"].([]any); len(created) != 1 || created[0] != subId {
		t.Errorf("changes created = %v", ch)
	}
	state1 := ch["newState"].(string)

	// The "sentAt" sort alias is honored on /queryChanges exactly as on
	// /query: the arguments pass validation, then the section 5.6 answer
	// for an uncalculatable state applies.
	r = callSub(t, ts, inv("EmailSubmission/queryChanges",
		fmt.Sprintf(`{"accountId":%q,"sinceQueryState":%q,"sort":[{"property":"sentAt"}]}`,
			testAccount, queryState0), "0"))
	if args := methodArgs(t, r, 0, "error"); args["type"] != "cannotCalculateChanges" {
		t.Errorf("queryChanges = %v", args)
	}
	r = callSub(t, ts, inv("EmailSubmission/queryChanges",
		fmt.Sprintf(`{"accountId":%q,"sinceQueryState":%q,"sort":[{"property":"attempts"}]}`,
			testAccount, queryState0), "0"))
	if args := methodArgs(t, r, 0, "error"); args["type"] != "unsupportedSort" {
		t.Errorf("queryChanges internal sort = %v", args)
	}

	// Cancel then destroy: updated then destroyed in /changes.
	callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"undoStatus":"canceled"}}}`, testAccount, subId), "0"))
	r = callSub(t, ts, inv("EmailSubmission/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, state1), "0"))
	ch = methodArgs(t, r, 0, "EmailSubmission/changes")
	if updated := ch["updated"].([]any); len(updated) != 1 || updated[0] != subId {
		t.Errorf("changes updated = %v", ch)
	}
	callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"destroy":[%q]}`, testAccount, subId), "0"))
	r = callSub(t, ts, inv("EmailSubmission/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, ch["newState"].(string)), "0"))
	ch = methodArgs(t, r, 0, "EmailSubmission/changes")
	if destroyed := ch["destroyed"].([]any); len(destroyed) != 1 || destroyed[0] != subId {
		t.Errorf("changes destroyed = %v", ch)
	}
}

// TestEmailSubmissionOnSuccessUntouched: an onSuccess entry keyed by a
// plain id the call never touched requests nothing - no implicit
// Email/set, and the referenced records stay put.
func TestEmailSubmissionOnSuccessUntouched(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	r := submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, emailId))
	subId := createdEcho(t, r)["id"].(string)

	// A later call whose only item fails, with onSuccess entries naming
	// the earlier (untouched) submission: nothing succeeded for those
	// ids, so no implicit call runs and the Email survives.
	r = callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"update":{"Snope":{"undoStatus":"canceled"}},
		  "onSuccessUpdateEmail":{%q:{"keywords/$seen":true}},
		  "onSuccessDestroyEmail":[%q]}`, testAccount, subId, subId), "0"))
	if len(r.MethodResponses) != 1 {
		t.Fatalf("implicit set ran for untouched ids: %v", r.MethodResponses)
	}
	if _, has := methodArgs(t, r, 0, "EmailSubmission/set")["notUpdated"].(map[string]any)["Snope"]; !has {
		t.Fatalf("expected the decoy update to fail: %v", r.MethodResponses[0].Args)
	}
	got := emailGet(t, ts, emailId, `,"properties":["keywords"]`)
	if len(got["keywords"].(map[string]any)) != 0 {
		t.Errorf("untouched onSuccess patched the Email: %v", got["keywords"])
	}
}
