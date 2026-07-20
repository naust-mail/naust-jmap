package mail

// Email/set create end to end (RFC 8621 section 4.6): the realistic
// draft flow - compose, read back, revise metadata, destroy - plus the
// wire-level error surfaces (blobNotFound with its notFound list,
// invalidProperties from the generator, metadata rejections from the
// seam) and the created response contract.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// TestEmailCreateDraftFlow: create a draft with both body alternatives
// and an uploaded attachment, read it back, flip its keywords, destroy it.
func TestEmailCreateDraftFlow(t *testing.T) {
	ts, db, store := emailServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts","role":"drafts"}`)
	attBlob := uploadBlob(t, db, store, "attached bytes")

	r := callMail(t, ts, inv("Email/set", fmt.Sprintf(`{"accountId":%q,"create":{"d1":{
		"mailboxIds":{%q:true},
		"keywords":{"$draft":true,"$seen":true},
		"from":[{"name":"Jøhn","email":"john@example.com"}],
		"to":[{"name":null,"email":"jane@example.com"}],
		"subject":"Working draft",
		"textBody":[{"partId":"t1","type":"text/plain"}],
		"htmlBody":[{"partId":"h1","type":"text/html"}],
		"attachments":[{"blobId":%q,"type":"application/octet-stream","name":"data.bin"}],
		"bodyValues":{"t1":{"value":"plain body\n"},"h1":{"value":"<p>html body</p>"}}
	}}}`, testAccount, drafts, attBlob), "0"))
	created, ok := methodArgs(t, r, 0, "Email/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", r.MethodResponses[0].Args)
	}
	echo := created["d1"].(map[string]any)
	// The created response is the section 4.6 contract: id, blobId,
	// threadId, size.
	for _, prop := range []string{"id", "blobId", "threadId", "size"} {
		if _, has := echo[prop]; !has {
			t.Fatalf("created echo missing %s: %v", prop, echo)
		}
	}
	if len(echo) != 4 {
		t.Fatalf("created echo has extra properties: %v", echo)
	}
	id := echo["id"].(string)

	// Read back: the stored fast properties and the on-demand body values
	// come from the generated message.
	r = callMail(t, ts, inv("Email/get", fmt.Sprintf(
		`{"accountId":%q,"ids":[%q],"properties":["from","subject","keywords","mailboxIds",
		"hasAttachment","preview","textBody","bodyValues","attachments"],
		"fetchTextBodyValues":true}`, testAccount, id), "0"))
	got := methodArgs(t, r, 0, "Email/get")["list"].([]any)[0].(map[string]any)
	from := got["from"].([]any)[0].(map[string]any)
	if from["name"] != "Jøhn" || from["email"] != "john@example.com" {
		t.Errorf("from = %v", from)
	}
	if got["subject"] != "Working draft" {
		t.Errorf("subject = %v", got["subject"])
	}
	if kw := got["keywords"].(map[string]any); kw["$draft"] != true || kw["$seen"] != true {
		t.Errorf("keywords = %v", kw)
	}
	if mb := got["mailboxIds"].(map[string]any); mb[drafts] != true || len(mb) != 1 {
		t.Errorf("mailboxIds = %v", mb)
	}
	if got["hasAttachment"] != true {
		t.Error("hasAttachment = false")
	}
	if !strings.Contains(got["preview"].(string), "plain body") {
		t.Errorf("preview = %q", got["preview"])
	}
	tb := got["textBody"].([]any)
	if len(tb) != 1 {
		t.Fatalf("textBody = %v", tb)
	}
	partID := tb[0].(map[string]any)["partId"].(string)
	bv := got["bodyValues"].(map[string]any)[partID].(map[string]any)
	if bv["value"] != "plain body\n" {
		t.Errorf("body value = %q", bv["value"])
	}
	atts := got["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["name"] != "data.bin" {
		t.Fatalf("attachments = %v", atts)
	}
	// Content addressing: the attachment part's blobId is the uploaded
	// blob's id (same decoded octets, same id).
	if attID := atts[0].(map[string]any)["blobId"]; attID != attBlob {
		t.Errorf("attachment blobId = %v, want %s", attID, attBlob)
	}

	// The Drafts counter moved; revising keywords moves unread, and the
	// draft destroys cleanly.
	if n := readCounters(t, ts, drafts)["totalEmails"]; n != float64(1) {
		t.Errorf("drafts totalEmails = %v", n)
	}
	r = callMail(t, ts, inv("Email/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"keywords/$seen":null}}}`, testAccount, id), "0"))
	if _, ok := methodArgs(t, r, 0, "Email/set")["updated"].(map[string]any); !ok {
		t.Fatalf("update failed: %v", r.MethodResponses[0].Args)
	}
	r = callMail(t, ts, inv("Email/set", fmt.Sprintf(
		`{"accountId":%q,"destroy":[%q]}`, testAccount, id), "0"))
	if d := methodArgs(t, r, 0, "Email/set")["destroyed"].([]any); len(d) != 1 {
		t.Fatalf("destroy failed: %v", r.MethodResponses[0].Args)
	}
	if n := readCounters(t, ts, drafts)["totalEmails"]; n != float64(0) {
		t.Errorf("drafts totalEmails after destroy = %v", n)
	}
}

// TestEmailCreateErrors: the wire-level rejection surfaces.
func TestEmailCreateErrors(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	_ = db
	_ = store

	set := func(create string) map[string]any {
		t.Helper()
		r := callMail(t, ts, inv("Email/set",
			fmt.Sprintf(`{"accountId":%q,"create":{"c":%s}}`, testAccount, create), "0"))
		args := methodArgs(t, r, 0, "Email/set")
		nc, _ := args["notCreated"].(map[string]any)
		if nc == nil {
			t.Fatalf("create accepted: %v", args)
		}
		return nc["c"].(map[string]any)
	}

	// blobNotFound lists every missing blob (section 4.6).
	se := set(fmt.Sprintf(`{"mailboxIds":{%q:true},
		"attachments":[{"blobId":"Gnope1"},{"blobId":"Gnope2"}]}`, inbox))
	if se["type"] != "blobNotFound" {
		t.Fatalf("want blobNotFound, got %v", se)
	}
	nf := se["notFound"].([]any)
	if len(nf) != 2 {
		t.Fatalf("notFound = %v", nf)
	}

	// Generator rejections surface as invalidProperties with the property.
	se = set(fmt.Sprintf(`{"mailboxIds":{%q:true},"headers":[]}`, inbox))
	if se["type"] != "invalidProperties" || se["properties"].([]any)[0] != "headers" {
		t.Fatalf("want invalidProperties on headers, got %v", se)
	}

	// Metadata rejections come from the seam at commit.
	se = set(`{"mailboxIds":{"Mnope":true},"subject":"x"}`)
	if se["type"] != "invalidProperties" || se["properties"].([]any)[0] != "mailboxIds" {
		t.Fatalf("want invalidProperties on mailboxIds, got %v", se)
	}
	se = set(`{"subject":"x"}`)
	if se["type"] != "invalidProperties" {
		t.Fatalf("want invalidProperties for missing mailboxIds, got %v", se)
	}

	// receivedAt must be a date when present.
	se = set(fmt.Sprintf(`{"mailboxIds":{%q:true},"receivedAt":"yesterday"}`, inbox))
	if se["type"] != "invalidProperties" || se["properties"].([]any)[0] != "receivedAt" {
		t.Fatalf("want invalidProperties on receivedAt, got %v", se)
	}

	// The attachment size cap is enforced as tooLarge.
	tiny := DefaultAccountCapability()
	tiny.MaxSizeAttachmentsPerEmail = 8
	ts2, db2, store2 := newEmailServer(t, tiny)
	inbox2 := createMailbox(t, ts2, `{"name":"Inbox","role":"inbox"}`)
	big := uploadBlob(t, db2, store2, "way more than eight bytes")
	r := callMail(t, ts2, inv("Email/set", fmt.Sprintf(`{"accountId":%q,"create":{"c":{
		"mailboxIds":{%q:true},"attachments":[{"blobId":%q}]}}}`, testAccount, inbox2, big), "0"))
	se2 := methodArgs(t, r, 0, "Email/set")["notCreated"].(map[string]any)["c"].(map[string]any)
	if se2["type"] != "tooLarge" {
		t.Fatalf("want tooLarge, got %v", se2)
	}
}

// TestEmailCreateSetsReceivedAtAndThreads: a client-set receivedAt is
// stored, the created Email threads with an existing message it
// references, and its creation id is usable later in the same call.
func TestEmailCreateSetsReceivedAtAndThreads(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	orig := putEmail(t, db, store,
		"Message-ID: <orig@example.com>\r\nSubject: Topic\r\nFrom: a@example.com\r\n\r\noriginal\r\n",
		map[string]bool{inbox: true}, nil)

	r := callMail(t, ts, inv("Email/set", fmt.Sprintf(`{"accountId":%q,"create":{"d1":{
		"mailboxIds":{%q:true},
		"receivedAt":"2021-03-04T12:00:00Z",
		"subject":"Re: Topic",
		"inReplyTo":["orig@example.com"],
		"references":["orig@example.com"]}}}`, testAccount, inbox), "0"))
	created, ok := methodArgs(t, r, 0, "Email/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", r.MethodResponses[0].Args)
	}
	echo := created["d1"].(map[string]any)
	id, newThread := echo["id"].(string), echo["threadId"].(string)

	r = callMail(t, ts, inv("Email/get", fmt.Sprintf(
		`{"accountId":%q,"ids":[%q],"properties":["receivedAt","threadId"]}`, testAccount, id), "0"))
	got := methodArgs(t, r, 0, "Email/get")["list"].([]any)[0].(map[string]any)
	if got["receivedAt"] != "2021-03-04T12:00:00Z" {
		t.Errorf("receivedAt = %v", got["receivedAt"])
	}
	if got["threadId"] != newThread {
		t.Errorf("threadId = %v vs echo %v", got["threadId"], newThread)
	}

	// The reply joined the original's thread (RFC 8621 section 3).
	r = callMail(t, ts, inv("Email/get", fmt.Sprintf(
		`{"accountId":%q,"ids":[%q],"properties":["threadId"]}`, testAccount, orig), "0"))
	origThread := methodArgs(t, r, 0, "Email/get")["list"].([]any)[0].(map[string]any)["threadId"].(string)
	if origThread != newThread {
		t.Errorf("reply thread %s != original thread %s", newThread, origThread)
	}

	// The creation id is registered: a later invocation in the same
	// request can reference "#d2" (RFC 8620 section 5.3).
	r = callMail(t, ts,
		inv("Email/set", fmt.Sprintf(`{"accountId":%q,"create":{"d2":{
			"mailboxIds":{%q:true},"subject":"throwaway"}}}`, testAccount, inbox), "0"),
		inv("Email/set", fmt.Sprintf(`{"accountId":%q,"destroy":["#d2"]}`, testAccount), "1"))
	real2 := methodArgs(t, r, 0, "Email/set")["created"].(map[string]any)["d2"].(map[string]any)["id"].(string)
	if d := methodArgs(t, r, 1, "Email/set")["destroyed"].([]any); len(d) != 1 || d[0] != real2 {
		t.Fatalf("destroy by creation id: %v", r.MethodResponses[1].Args)
	}
}

// TestEmailCreateStateMismatch: ifInState still guards a /set whose
// creates were prepared before the commit opened.
func TestEmailCreateStateMismatch(t *testing.T) {
	ts, _, _ := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	r := callMail(t, ts, inv("Email/set", fmt.Sprintf(`{"accountId":%q,"ifInState":"bogus",
		"create":{"c":{"mailboxIds":{%q:true},"subject":"x"}}}`, testAccount, inbox), "0"))
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("want error, got %v", r.MethodResponses[0])
	}
	if args := methodArgs(t, r, 0, "error"); args["type"] != string(jmap.ErrStateMismatch) {
		t.Fatalf("want stateMismatch, got %v", args)
	}
}
