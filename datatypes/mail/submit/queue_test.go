package submit

// Tests for the Queue's Message-ID correlation (RFC 8098 section 3.2.5's
// Original-Message-ID answered from the indexed messageId snapshot): a
// single submission resolves to its Email, an unknown id resolves to
// nothing, and a Message-ID two submissions share identifies nothing.

import (
	"context"
	"fmt"
	"testing"
)

func TestEmailIDForMessageID(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	q := newSubmissionQueue(db, store)
	ctx := context.Background()

	submit := func(emailId string, cid string) {
		t.Helper()
		r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
			`{"accountId":%q,"create":{%q:{"identityId":%q,"emailId":%q}}}`,
			testAccount, cid, identityId, emailId), "0"))
		if _, ok := methodArgs(t, r, 0, "EmailSubmission/set")["created"].(map[string]any)[cid]; !ok {
			t.Fatalf("submission create failed: %v", r.MethodResponses[0].Args)
		}
	}

	emailId := putEmail(t, db, store, sendableMsg(nil),
		map[string]bool{drafts: true}, map[string]bool{"$draft": true})
	submit(emailId, "s1")

	id, ok, err := q.EmailIDForMessageID(ctx, testAccount, "m1@example.com")
	if err != nil || !ok || string(id) != emailId {
		t.Fatalf("EmailIDForMessageID = (%q, %v, %v), want (%q, true, nil)", id, ok, err, emailId)
	}

	if _, ok, err := q.EmailIDForMessageID(ctx, testAccount, "unknown@example.com"); err != nil || ok {
		t.Fatalf("unknown Message-ID = (ok=%v, err=%v), want a miss", ok, err)
	}

	// A second submission whose message reuses the same Message-ID makes
	// the id ambiguous: it identifies nothing.
	emailId2 := putEmail(t, db, store, sendableMsg(map[string]string{"X-Copy": "2"}),
		map[string]bool{drafts: true}, map[string]bool{"$draft": true})
	submit(emailId2, "s2")

	if _, ok, err := q.EmailIDForMessageID(ctx, testAccount, "m1@example.com"); err != nil || ok {
		t.Fatalf("ambiguous Message-ID = (ok=%v, err=%v), want a miss", ok, err)
	}
}
