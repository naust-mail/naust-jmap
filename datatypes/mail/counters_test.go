package mail

// Counter tests: the four Mailbox counters (RFC 8621 section 2.1) under
// create, mark-read, and the trash-aware unreadThreads worked example
// from section 2.1.1, plus the section 2.5 destroy preconditions and the
// onDestroyRemoveEmails cascade.

import (
	"fmt"
	"net/http/httptest"
	"testing"
)

// readCounters reads a Mailbox's four count properties.
func readCounters(t *testing.T, ts *httptest.Server, id string) map[string]any {
	t.Helper()
	r := callMail(t, ts, inv("Mailbox/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["totalEmails","unreadEmails","totalThreads","unreadThreads"]}`, testAccount, id), "0"))
	list := methodArgs(t, r, 0, "Mailbox/get")["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Mailbox/get %s: %v", id, list)
	}
	return list[0].(map[string]any)
}

// wantCounters asserts all four counters at once (JSON numbers decode to
// float64).
func wantCounters(t *testing.T, label string, m map[string]any, total, unread, totalT, unreadT float64) {
	t.Helper()
	got := [4]any{m["totalEmails"], m["unreadEmails"], m["totalThreads"], m["unreadThreads"]}
	want := [4]float64{total, unread, totalT, unreadT}
	for i, name := range mailboxCounters {
		if got[i] != want[i] {
			t.Errorf("%s: %s = %v, want %v (all: %v)", label, name, got[i], want[i], got)
		}
	}
}

// TestCounterBasics: an unread Email raises all four counters; marking it
// $seen clears the two unread counters and leaves the totals.
func TestCounterBasics(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	id := putEmail(t, db, store, threadMsg("Hi", map[string]string{"Message-ID": "<a@x>"}), map[string]bool{inbox: true}, nil)
	wantCounters(t, "after create", readCounters(t, ts, inbox), 1, 1, 1, 1)

	callMail(t, ts, inv("Email/set",
		fmt.Sprintf(`{"accountId":%q,"update":{%q:{"keywords/$seen":true}}}`, testAccount, id), "0"))
	wantCounters(t, "after mark read", readCounters(t, ts, inbox), 1, 0, 1, 0)
}

// TestCounterTrashExample reproduces the RFC 8621 section 2.1.1 worked
// example: a single Thread of two Emails, an unread one in the trash and
// a read one in the inbox. unreadThreads is 1 for the trash and 0 for the
// inbox; each Mailbox holds one Email and one Thread.
func TestCounterTrashExample(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	trash := createMailbox(t, ts, `{"name":"Trash","role":"trash"}`)

	// A: read, in the inbox.
	a := putEmail(t, db, store, threadMsg("Chat", map[string]string{"Message-ID": "<a@x>"}),
		map[string]bool{inbox: true}, map[string]bool{"$seen": true})
	// B: unread, in the trash, same Thread as A (references it).
	b := putEmail(t, db, store, threadMsg("Re: Chat", map[string]string{"Message-ID": "<b@x>", "References": "<a@x>"}),
		map[string]bool{trash: true}, nil)
	if threadOf(t, ts, a) != threadOf(t, ts, b) {
		t.Fatal("the two Emails should be in one Thread")
	}

	wantCounters(t, "inbox", readCounters(t, ts, inbox), 1, 0, 1, 0)
	wantCounters(t, "trash", readCounters(t, ts, trash), 1, 1, 1, 1)
}

// TestMailboxHasEmail: destroying a Mailbox that still holds Email is
// rejected unless onDestroyRemoveEmails is set (section 2.5).
func TestMailboxHasEmail(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	putEmail(t, db, store, threadMsg("Hi", map[string]string{"Message-ID": "<a@x>"}), map[string]bool{inbox: true}, nil)

	se := setError(t, ts,
		fmt.Sprintf(`{"accountId":%q,"destroy":[%q]}`, testAccount, inbox), "notDestroyed", inbox)
	if se["type"] != "mailboxHasEmail" {
		t.Fatalf("want mailboxHasEmail, got %v", se)
	}
}

// TestOnDestroyRemoveEmails: the cascade removes the Mailbox from every
// Email it held; an Email left in no Mailbox is destroyed, one still in
// another Mailbox survives there (section 2.5).
func TestOnDestroyRemoveEmails(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	archive := createMailbox(t, ts, `{"name":"Archive","role":"archive"}`)

	shared := putEmail(t, db, store, threadMsg("Kept", map[string]string{"Message-ID": "<a@x>"}),
		map[string]bool{inbox: true, archive: true}, nil)
	only := putEmail(t, db, store, threadMsg("Gone", map[string]string{"Message-ID": "<b@x>"}),
		map[string]bool{inbox: true}, nil)

	r := callMail(t, ts, inv("Mailbox/set",
		fmt.Sprintf(`{"accountId":%q,"destroy":[%q],"onDestroyRemoveEmails":true}`, testAccount, inbox), "0"))
	if d := methodArgs(t, r, 0, "Mailbox/set")["destroyed"].([]any); len(d) != 1 || d[0] != inbox {
		t.Fatalf("inbox not destroyed: %v", d)
	}

	// The inbox is gone.
	if nf := methodArgs(t, callMail(t, ts, inv("Mailbox/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, testAccount, inbox), "0")), 0, "Mailbox/get")["notFound"].([]any); len(nf) != 1 {
		t.Fatalf("inbox still present: %v", nf)
	}
	// The Email in two Mailboxes survives, now only in archive.
	mb := emailGet(t, ts, shared, `,"properties":["mailboxIds"]`)["mailboxIds"].(map[string]any)
	if len(mb) != 1 || mb[archive] != true {
		t.Errorf("shared Email mailboxIds = %v, want {%s}", mb, archive)
	}
	// The Email that was only in the inbox is destroyed.
	if nf := methodArgs(t, callMail(t, ts, inv("Email/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, testAccount, only), "0")), 0, "Email/get")["notFound"].([]any); len(nf) != 1 || nf[0] != only {
		t.Errorf("inbox-only Email not destroyed: %v", nf)
	}
	// Archive still holds the surviving Email and its Thread.
	wantCounters(t, "archive", readCounters(t, ts, archive), 1, 1, 1, 1)
}
