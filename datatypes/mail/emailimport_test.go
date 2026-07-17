package mail

// Email/import tests (RFC 8621 section 4.8): a happy-path import produces an
// Email with an assigned Thread and the given mailbox/keywords, ifInState
// guards the account state, a missing blob or empty mailboxIds is a per-Email
// invalidProperties SetError, and an omitted receivedAt defaults to the most
// recent Received header.

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
)

// uploadBlob stores raw as an uploaded blob and returns its blobId, mimicking
// the standard upload step Email/import and Email/parse consume.
func uploadBlob(t *testing.T, db *objectdb.DB, store blob.Store, raw string) string {
	t.Helper()
	return string(storeAndRecord(t, db, store, raw, testReceivedAt))
}

// doImport runs one Email/import and returns the response (or error) args.
func doImport(t *testing.T, ts *httptest.Server, argsJSON string) map[string]any {
	t.Helper()
	r := callMail(t, ts, inv("Email/import", argsJSON, "0"))
	name := "Email/import"
	if r.MethodResponses[0].Name == "error" {
		name = "error"
	}
	return methodArgs(t, r, 0, name)
}

func TestEmailImportBasic(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	blobID := uploadBlob(t, db, store, simpleMessage)

	args := fmt.Sprintf(`{"accountId":%q,"emails":{"e1":{"blobId":%q,`+
		`"mailboxIds":{%q:true},"keywords":{"$seen":true}}}}`, testAccount, blobID, inbox)
	resp := doImport(t, ts, args)

	created, ok := resp["created"].(map[string]any)
	if !ok {
		t.Fatalf("no created: %v", resp)
	}
	e1, ok := created["e1"].(map[string]any)
	if !ok {
		t.Fatalf("e1 not created: %v", resp)
	}
	// The created report carries id, blobId, threadId, size (section 4.8).
	if e1["blobId"] != blobID {
		t.Errorf("blobId = %v, want %q", e1["blobId"], blobID)
	}
	if e1["size"] != float64(len(simpleMessage)) {
		t.Errorf("size = %v, want %d", e1["size"], len(simpleMessage))
	}
	if tid, _ := e1["threadId"].(string); tid == "" {
		t.Errorf("threadId not assigned: %v", e1["threadId"])
	}
	if resp["oldState"] == resp["newState"] {
		t.Errorf("state should advance: old=%v new=%v", resp["oldState"], resp["newState"])
	}

	// The stored Email reflects the imported metadata.
	id := e1["id"].(string)
	obj := emailGet(t, ts, id, "")
	if obj["subject"] != "Dinner on Thursday?" {
		t.Errorf("subject = %v", obj["subject"])
	}
	if mb := obj["mailboxIds"].(map[string]any); mb[inbox] != true || len(mb) != 1 {
		t.Errorf("mailboxIds = %v, want {%s:true}", mb, inbox)
	}
	if kw := obj["keywords"].(map[string]any); kw["$seen"] != true || len(kw) != 1 {
		t.Errorf("keywords = %v, want {$seen:true}", kw)
	}
}

func TestEmailImportIfInStateMismatch(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	blobID := uploadBlob(t, db, store, simpleMessage)

	args := fmt.Sprintf(`{"accountId":%q,"ifInState":"wrong","emails":{"e1":`+
		`{"blobId":%q,"mailboxIds":{%q:true}}}}`, testAccount, blobID, inbox)
	resp := doImport(t, ts, args)
	if resp["type"] != "stateMismatch" {
		t.Errorf("type = %v, want stateMismatch", resp["type"])
	}
}

func TestEmailImportBlobNotFound(t *testing.T) {
	ts, _, _ := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)

	args := fmt.Sprintf(`{"accountId":%q,"emails":{"e1":{"blobId":"Gmissing",`+
		`"mailboxIds":{%q:true}}}}`, testAccount, inbox)
	resp := doImport(t, ts, args)
	nc, ok := resp["notCreated"].(map[string]any)
	if !ok {
		t.Fatalf("no notCreated: %v", resp)
	}
	wantInvalidProp(t, nc["e1"].(map[string]any), "blobId")
}

func TestEmailImportEmptyMailboxIds(t *testing.T) {
	ts, db, store := emailServer(t)
	blobID := uploadBlob(t, db, store, simpleMessage)

	args := fmt.Sprintf(`{"accountId":%q,"emails":{"e1":{"blobId":%q,`+
		`"mailboxIds":{}}}}`, testAccount, blobID)
	resp := doImport(t, ts, args)
	nc, ok := resp["notCreated"].(map[string]any)
	if !ok {
		t.Fatalf("no notCreated: %v", resp)
	}
	wantInvalidProp(t, nc["e1"].(map[string]any), "mailboxIds")
}

// receivedHeaderMessage carries a Received header whose date should become the
// import's receivedAt when the client omits one (section 4.8).
const receivedHeaderMessage = "Received: from a.example by b.example; " +
	"Wed, 03 Mar 2021 09:00:00 +0000\r\n" +
	"From: a@example.com\r\n" +
	"Subject: dated\r\n" +
	"Message-ID: <r1@example.com>\r\n" +
	"\r\n" +
	"body\r\n"

func TestEmailImportReceivedAtFromHeader(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	blobID := uploadBlob(t, db, store, receivedHeaderMessage)

	args := fmt.Sprintf(`{"accountId":%q,"emails":{"e1":{"blobId":%q,`+
		`"mailboxIds":{%q:true}}}}`, testAccount, blobID, inbox)
	resp := doImport(t, ts, args)
	id := resp["created"].(map[string]any)["e1"].(map[string]any)["id"].(string)

	obj := emailGet(t, ts, id, `,"properties":["receivedAt"]`)
	if got, _ := obj["receivedAt"].(string); got != "2021-03-03T09:00:00Z" {
		t.Errorf("receivedAt = %v, want the Received header date", obj["receivedAt"])
	}
}

// TestEmailImportEAI covers the section 4.8 MUST that Email/import supports
// EAI (RFC 6532) headers: an internationalized From address is stored intact.
func TestEmailImportEAI(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	blobID := uploadBlob(t, db, store, eaiMessage)

	args := fmt.Sprintf(`{"accountId":%q,"emails":{"e1":{"blobId":%q,`+
		`"mailboxIds":{%q:true}}}}`, testAccount, blobID, inbox)
	resp := doImport(t, ts, args)
	id := resp["created"].(map[string]any)["e1"].(map[string]any)["id"].(string)

	obj := emailGet(t, ts, id, `,"properties":["from"]`)
	from := obj["from"].([]any)
	if len(from) != 1 || from[0].(map[string]any)["email"] != "jörg@münchen.example" {
		t.Errorf("from = %v, want the EAI address preserved", obj["from"])
	}
}
