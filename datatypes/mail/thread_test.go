package mail

// Thread tests: the RFC 8621 section 3.1.1 Thread/get shape, the
// section 3 assignment algorithm (shared message-id AND equal base
// subject), the no-merge rule, emailIds ordering, and
// Thread/changes.

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// threadMsg builds a minimal RFC 5322 message with a subject and the
// given extra headers (Message-ID, In-Reply-To, References).
func threadMsg(subject string, headers map[string]string) string {
	h := "From: a@example.com\r\nSubject: " + subject + "\r\n"
	for k, v := range headers {
		h += k + ": " + v + "\r\n"
	}
	return h + "\r\nbody\r\n"
}

func threadOf(t *testing.T, ts *httptest.Server, id string) string {
	t.Helper()
	return emailGet(t, ts, id, `,"properties":["threadId"]`)["threadId"].(string)
}

func threadGet(t *testing.T, ts *httptest.Server, ids ...string) map[string]any {
	t.Helper()
	quoted := ""
	for i, id := range ids {
		if i > 0 {
			quoted += ","
		}
		quoted += fmt.Sprintf("%q", id)
	}
	r := callMail(t, ts, inv("Thread/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%s]}`, testAccount, quoted), "0"))
	return methodArgs(t, r, 0, "Thread/get")
}

var mbInbox = map[string]bool{"MBinbox": true}

// TestThreadGetRFCShape mirrors the RFC 8621 section 3.1.1 example: a
// Thread/get for two Thread ids returns one multi-Email Thread and one
// single-Email Thread, each with emailIds oldest-first, and an empty
// notFound. It also checks the notFound path for an unknown id.
func TestThreadGetRFCShape(t *testing.T) {
	ts, db, store := emailServer(t)
	// A two-Email Thread (a reply), like the RFC's ["eaa623","f782cbb"].
	early := time.Date(2021, 3, 3, 10, 0, 0, 0, time.UTC)
	late := time.Date(2021, 3, 3, 11, 0, 0, 0, time.UTC)
	a := putEmailAt(t, db, store, threadMsg("Party", map[string]string{"Message-ID": "<a@x>"}), mbInbox, nil, early)
	b := putEmailAt(t, db, store, threadMsg("Re: Party", map[string]string{"Message-ID": "<b@x>", "In-Reply-To": "<a@x>"}), mbInbox, nil, late)
	// A single-Email Thread, like the RFC's ["82cf7bb"].
	c := putEmail(t, db, store, threadMsg("Standalone", map[string]string{"Message-ID": "<c@x>"}), mbInbox, nil)

	ta, tc := threadOf(t, ts, a), threadOf(t, ts, c)
	if ta != threadOf(t, ts, b) {
		t.Fatal("reply did not join the original's Thread")
	}

	res := threadGet(t, ts, ta, tc, "Tmissing")
	list := res["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("Thread/get list: %v", list)
	}
	byId := map[string][]any{}
	for _, o := range list {
		m := o.(map[string]any)
		byId[m["id"].(string)] = m["emailIds"].([]any)
	}
	// Oldest first: a before b (a has the earlier receivedAt).
	if ids := byId[ta]; len(ids) != 2 || ids[0] != a || ids[1] != b {
		t.Errorf("Thread %s emailIds = %v, want [%s %s]", ta, ids, a, b)
	}
	if ids := byId[tc]; len(ids) != 1 || ids[0] != c {
		t.Errorf("Thread %s emailIds = %v, want [%s]", tc, ids, c)
	}
	if nf := res["notFound"].([]any); len(nf) != 1 || nf[0] != "Tmissing" {
		t.Errorf("notFound = %v, want [Tmissing]", nf)
	}
}

// TestThreadingConditions checks the section 3 suggested algorithm: two
// Emails thread only when they BOTH share a message-id and have the same
// base subject. A changed subject splits (the spec's stated intent); a
// shared subject with no shared message-id does not merge unrelated mail.
func TestThreadingConditions(t *testing.T) {
	ts, db, store := emailServer(t)
	a := putEmail(t, db, store, threadMsg("Hello", map[string]string{"Message-ID": "<a@x>"}), mbInbox, nil)
	// Shared message-id + same base subject -> same Thread.
	reply := putEmail(t, db, store, threadMsg("Re: Hello", map[string]string{"Message-ID": "<b@x>", "In-Reply-To": "<a@x>"}), mbInbox, nil)
	// Shared message-id but a different subject -> new Thread (condition 2).
	renamed := putEmail(t, db, store, threadMsg("Unrelated topic", map[string]string{"Message-ID": "<c@x>", "In-Reply-To": "<a@x>"}), mbInbox, nil)
	// Same base subject but no shared message-id -> new Thread (condition 1).
	namesake := putEmail(t, db, store, threadMsg("Hello", map[string]string{"Message-ID": "<d@x>"}), mbInbox, nil)

	ta := threadOf(t, ts, a)
	if threadOf(t, ts, reply) != ta {
		t.Error("shared message-id + same subject should thread together")
	}
	if threadOf(t, ts, renamed) == ta {
		t.Error("a changed subject should start a new Thread")
	}
	if threadOf(t, ts, namesake) == ta {
		t.Error("same subject without a shared message-id should not thread")
	}
}

// TestThreadNoMerge covers the out-of-order case from section 3: a later
// Email whose References span two existing Threads joins one of them, and
// the Threads are NOT merged (threadId is immutable).
func TestThreadNoMerge(t *testing.T) {
	ts, db, store := emailServer(t)
	a := putEmail(t, db, store, threadMsg("Topic", map[string]string{"Message-ID": "<a@x>"}), mbInbox, nil)
	// No shared message-id with a -> its own Thread.
	b := putEmail(t, db, store, threadMsg("Topic", map[string]string{"Message-ID": "<b@x>"}), mbInbox, nil)
	// References both a and b: a merging server would unite all three.
	c := putEmail(t, db, store, threadMsg("Topic", map[string]string{"Message-ID": "<c@x>", "References": "<a@x> <b@x>"}), mbInbox, nil)

	ta, tb, tc := threadOf(t, ts, a), threadOf(t, ts, b), threadOf(t, ts, c)
	if ta == tb {
		t.Fatal("independent Emails must not share a Thread")
	}
	if tc != ta && tc != tb {
		t.Fatalf("linking Email joined neither existing Thread: %s vs %s/%s", tc, ta, tb)
	}
}

// TestThreadEmailIdsOrdering checks emailIds is sorted by receivedAt
// oldest first even when Emails arrive out of order (section 3).
func TestThreadEmailIdsOrdering(t *testing.T) {
	ts, db, store := emailServer(t)
	early := time.Date(2021, 1, 1, 9, 0, 0, 0, time.UTC)
	late := time.Date(2021, 1, 2, 9, 0, 0, 0, time.UTC)
	// The later Email arrives first.
	b := putEmailAt(t, db, store, threadMsg("Re: T", map[string]string{"Message-ID": "<b@x>", "In-Reply-To": "<a@x>"}), mbInbox, nil, late)
	a := putEmailAt(t, db, store, threadMsg("T", map[string]string{"Message-ID": "<a@x>"}), mbInbox, nil, early)

	ids := threadGet(t, ts, threadOf(t, ts, a))["list"].([]any)[0].(map[string]any)["emailIds"].([]any)
	if len(ids) != 2 || ids[0] != a || ids[1] != b {
		t.Errorf("emailIds = %v, want oldest-first [%s %s]", ids, a, b)
	}
}

// TestThreadChanges checks Thread/changes reports a Thread created, then
// updated as an Email joins, then destroyed as its last Email is removed.
func TestThreadChanges(t *testing.T) {
	ts, db, store := emailServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	mb := map[string]bool{inbox: true}

	s0 := threadState(t, ts)
	a := putEmail(t, db, store, threadMsg("Hello", map[string]string{"Message-ID": "<a@x>"}), mb, nil)
	tid := threadOf(t, ts, a)
	if ch := threadChanges(t, ts, s0); !hasStr(ch["created"].([]any), tid) {
		t.Fatalf("Thread not reported created: %v", ch)
	}

	s1 := threadState(t, ts)
	b := putEmail(t, db, store, threadMsg("Re: Hello", map[string]string{"Message-ID": "<b@x>", "In-Reply-To": "<a@x>"}), mb, nil)
	if ch := threadChanges(t, ts, s1); !hasStr(ch["updated"].([]any), tid) {
		t.Fatalf("Thread not reported updated when Email joined: %v", ch)
	}

	s2 := threadState(t, ts)
	callMail(t, ts, inv("Email/set", fmt.Sprintf(`{"accountId":%q,"destroy":[%q,%q]}`, testAccount, a, b), "0"))
	if ch := threadChanges(t, ts, s2); !hasStr(ch["destroyed"].([]any), tid) {
		t.Fatalf("Thread not reported destroyed when last Email left: %v", ch)
	}
}

func threadState(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	// A Thread/get with no ids still returns the current state string.
	r := callMail(t, ts, inv("Thread/get", fmt.Sprintf(`{"accountId":%q,"ids":[]}`, testAccount), "0"))
	return methodArgs(t, r, 0, "Thread/get")["state"].(string)
}

func threadChanges(t *testing.T, ts *httptest.Server, since string) map[string]any {
	t.Helper()
	r := callMail(t, ts, inv("Thread/changes",
		fmt.Sprintf(`{"accountId":%q,"sinceState":%q}`, testAccount, since), "0"))
	return methodArgs(t, r, 0, "Thread/changes")
}

func hasStr(list []any, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
