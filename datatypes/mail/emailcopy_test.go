package mail

// Email/copy tests (RFC 8621 section 4.7 over RFC 8620 section 5.4): the
// cross-account move story, the restricted property set, defaulting from
// the original, and the ifInState matrix.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

const copyTarget = "Atarget"

// copyEmailServer is the Email server wiring plus a second mail account
// for john - the copy target. Mirrors newEmailServer; kept separate so the
// shared harness stays single-account.
func copyEmailServer(t *testing.T) (*httptest.Server, *objectdb.DB, blob.Store) {
	t.Helper()
	a := newStaticAuth()
	a.AddUser("john@example.com", "secret", testAccount)
	a.AddAccess("john@example.com", copyTarget, auth.Access{Name: "target"})
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(memory.New())
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	if err := RegisterMailbox(p, db, core); err != nil {
		t.Fatal(err)
	}
	if err := RegisterThread(p, db, core); err != nil {
		t.Fatal(err)
	}
	if err := RegisterEmail(p, db, store, core, DefaultAccountCapability(), nil); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(CapabilityURI, struct{}{}, DefaultAccountCapability()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, db, store
}

// mailboxIn creates one mailbox in the given account and returns its id.
func mailboxIn(t *testing.T, ts *httptest.Server, acct, name string) string {
	t.Helper()
	r := callMail(t, ts, inv("Mailbox/set",
		fmt.Sprintf(`{"accountId":%q,"create":{"c":{"name":%q}}}`, acct, name), "0"))
	args := methodArgs(t, r, 0, "Mailbox/set")
	created, ok := args["created"].(map[string]any)
	if !ok {
		t.Fatalf("mailbox create failed: %v", args)
	}
	return created["c"].(map[string]any)["id"].(string)
}

// getEmailIn fetches one Email by id from the given account, failing the
// test unless it is exactly found.
func getEmailIn(t *testing.T, ts *httptest.Server, acct, id string) map[string]any {
	t.Helper()
	r := callMail(t, ts, inv("Email/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, acct, id), "0"))
	g := methodArgs(t, r, 0, "Email/get")
	list, ok := g["list"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("get %s in %s: list %v, notFound %v", id, acct, g["list"], g["notFound"])
	}
	return list[0].(map[string]any)
}

// TestEmailCopyMovesBetweenAccounts is the section 4.7 story: copy to
// another account with onSuccessDestroyOriginal - two responses under one
// call id, the created echo carrying exactly id/blobId/threadId/size, the
// message re-threaded and counted in the target, the original destroyed
// (with its counters unwound) in the from account.
func TestEmailCopyMovesBetweenAccounts(t *testing.T) {
	ts, db, store := copyEmailServer(t)
	srcMbx := mailboxIn(t, ts, testAccount, "Src")
	dstMbx := mailboxIn(t, ts, copyTarget, "Dst")
	orig := putEmail(t, db, store, simpleMessage, map[string]bool{srcMbx: true}, map[string]bool{"$seen": true})
	origBlob := emailGet(t, ts, orig, "")["blobId"].(string)

	r := callMail(t, ts, inv("Email/copy", fmt.Sprintf(
		`{"fromAccountId":%q,"accountId":%q,"create":{"k1":{"id":%q,"mailboxIds":{%q:true}}},"onSuccessDestroyOriginal":true}`,
		testAccount, copyTarget, orig, dstMbx), "0"))
	// The implicit Email/set produces a second response to the single
	// method call, both with the same call id (RFC 8620 section 5.4).
	if len(r.MethodResponses) != 2 {
		t.Fatalf("%d responses, want 2", len(r.MethodResponses))
	}
	for i, mr := range r.MethodResponses {
		if mr.CallID != "0" {
			t.Fatalf("response %d call id %q", i, mr.CallID)
		}
	}
	args := methodArgs(t, r, 0, "Email/copy")
	if args["fromAccountId"] != testAccount || args["accountId"] != copyTarget {
		t.Fatalf("account echo: %v", args)
	}
	echo := args["created"].(map[string]any)["k1"].(map[string]any)
	// The created response contains the id, blobId, threadId, and size
	// properties of the new object (4.7) - and nothing else.
	if len(echo) != 4 {
		t.Fatalf("created echo has %d properties: %v", len(echo), echo)
	}
	newId := echo["id"].(string)
	if newId == "" || newId == orig {
		t.Fatalf("copy id %q (original %q)", newId, orig)
	}
	// Content addressing keeps the blobId across accounts.
	if echo["blobId"] != origBlob {
		t.Fatalf("copy blobId %v, original %v", echo["blobId"], origBlob)
	}
	if echo["size"].(float64) != float64(len(simpleMessage)) {
		t.Fatalf("copy size %v, want %d", echo["size"], len(simpleMessage))
	}
	set := methodArgs(t, r, 1, "Email/set")
	if set["accountId"] != testAccount {
		t.Fatalf("implicit set account: %v", set)
	}
	if d := set["destroyed"].([]any); len(d) != 1 || d[0] != orig {
		t.Fatalf("implicit destroy: %v", set)
	}

	// The copy is a real Email in the target: re-parsed (subject present),
	// re-threaded there (echo threadId is the stored one), keywords and
	// receivedAt inherited from the original.
	got := getEmailIn(t, ts, copyTarget, newId)
	if got["subject"] != "Dinner on Thursday?" {
		t.Fatalf("copied subject: %v", got["subject"])
	}
	if got["threadId"] != echo["threadId"] {
		t.Fatalf("threadId: stored %v, echo %v", got["threadId"], echo["threadId"])
	}
	if kw := got["keywords"].(map[string]any); kw["$seen"] != true {
		t.Fatalf("inherited keywords: %v", kw)
	}
	if got["receivedAt"] != "2021-03-04T12:00:00Z" {
		t.Fatalf("inherited receivedAt: %v", got["receivedAt"])
	}
	// The original is gone; both accounts' Mailbox counters reflect the
	// move.
	r = callMail(t, ts, inv("Email/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, testAccount, orig), "0"))
	if nf := methodArgs(t, r, 0, "Email/get")["notFound"].([]any); len(nf) != 1 {
		t.Fatalf("original survived: %v", nf)
	}
	for _, c := range []struct {
		acct, mbx string
		total     float64
	}{{testAccount, srcMbx, 0}, {copyTarget, dstMbx, 1}} {
		r := callMail(t, ts, inv("Mailbox/get",
			fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, c.acct, c.mbx), "0"))
		mb := methodArgs(t, r, 0, "Mailbox/get")["list"].([]any)[0].(map[string]any)
		if mb["totalEmails"] != c.total {
			t.Fatalf("%s totalEmails %v, want %v", c.mbx, mb["totalEmails"], c.total)
		}
	}
}

// TestEmailCopyRestrictedProperties: only mailboxIds, keywords, and
// receivedAt may be set during the copy (4.7); the id is required; a
// missing original is notFound; and inherited from-account mailboxIds do
// not exist in the target, so a copy that omits mailboxIds is rejected by
// validation - each as a per-item SetError.
func TestEmailCopyRestrictedProperties(t *testing.T) {
	ts, db, store := copyEmailServer(t)
	srcMbx := mailboxIn(t, ts, testAccount, "Src")
	mailboxIn(t, ts, copyTarget, "Dst")
	orig := putEmail(t, db, store, simpleMessage, map[string]bool{srcMbx: true}, nil)

	r := callMail(t, ts, inv("Email/copy", fmt.Sprintf(
		`{"fromAccountId":%q,"accountId":%q,"create":{`+
			`"bad1":{"id":%q,"subject":"nope"},`+
			`"bad2":{"mailboxIds":{"x":true}},`+
			`"bad3":{"id":"Enope"},`+
			`"bad4":{"id":%q}}}`,
		testAccount, copyTarget, orig, orig), "0"))
	args := methodArgs(t, r, 0, "Email/copy")
	if v, has := args["created"]; !has || v != nil {
		t.Fatalf("created: %v", args)
	}
	nc := args["notCreated"].(map[string]any)
	wantInvalidProp(t, nc["bad1"].(map[string]any), "subject")
	wantInvalidProp(t, nc["bad2"].(map[string]any), "id")
	if typ := nc["bad3"].(map[string]any)["type"]; typ != "notFound" {
		t.Fatalf("bad3: %v", nc["bad3"])
	}
	wantInvalidProp(t, nc["bad4"].(map[string]any), "mailboxIds")
}

// TestEmailCopyStateMatrix: ifFromInState and ifInState mismatches abort
// the whole method with stateMismatch; a destroyFromIfInState mismatch
// aborts only the implicit destroy, leaving the copy standing (5.4).
func TestEmailCopyStateMatrix(t *testing.T) {
	ts, db, store := copyEmailServer(t)
	srcMbx := mailboxIn(t, ts, testAccount, "Src")
	dstMbx := mailboxIn(t, ts, copyTarget, "Dst")
	orig := putEmail(t, db, store, simpleMessage, map[string]bool{srcMbx: true}, nil)

	errType := func(r *jmap.Response, i int) string {
		if r.MethodResponses[i].Name != "error" {
			return ""
		}
		var e struct {
			Type string `json:"type"`
		}
		json.Unmarshal(r.MethodResponses[i].Args, &e)
		return e.Type
	}
	copyArgs := func(extra string) string {
		return fmt.Sprintf(
			`{"fromAccountId":%q,"accountId":%q,"create":{"k1":{"id":%q,"mailboxIds":{%q:true}}}%s}`,
			testAccount, copyTarget, orig, dstMbx, extra)
	}

	for _, cond := range []string{`,"ifFromInState":"stale"`, `,"ifInState":"stale"`} {
		r := callMail(t, ts, inv("Email/copy", copyArgs(cond), "0"))
		if len(r.MethodResponses) != 1 || errType(r, 0) != jmap.ErrStateMismatch {
			t.Fatalf("%s: %v", cond, r.MethodResponses)
		}
	}

	r := callMail(t, ts, inv("Email/copy",
		copyArgs(`,"onSuccessDestroyOriginal":true,"destroyFromIfInState":"stale"`), "0"))
	if len(r.MethodResponses) != 2 {
		t.Fatalf("%d responses, want 2", len(r.MethodResponses))
	}
	created := methodArgs(t, r, 0, "Email/copy")["created"].(map[string]any)
	newId := created["k1"].(map[string]any)["id"].(string)
	if errType(r, 1) != jmap.ErrStateMismatch {
		t.Fatalf("implicit destroy: %v", r.MethodResponses[1])
	}
	// The copy stands in the target; the original survived the aborted
	// destroy.
	getEmailIn(t, ts, copyTarget, newId)
	getEmailIn(t, ts, testAccount, orig)
}
