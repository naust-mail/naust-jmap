package mail

// RFC 8621 section 7.5.1 worked example, pinned as a conformance fixture:
// the request and both response shapes the spec documents, asserted field
// for field. Server-set ids differ from the RFC's literal values (they are
// generated), so the fixture checks structure and the RFC's stated
// invariants - two responses sharing the call id, the oldState/newState
// relationship, the null-valued implicit-update entry - rather than opaque
// id strings.

import (
	"fmt"
	"testing"
)

// TestConformanceSubmission751 is the section 7.5.1 example: a saved draft
// is sent, and on success the implicit Email/set moves it out of Drafts
// into Sent and clears $draft. The response carries two method responses
// with the same call id (RFC: "both have the same method call id as they
// are due to the same call in the request"), and the submission's state
// advanced while the Email's state advanced too.
func TestConformanceSubmission751(t *testing.T) {
	ts, db, store, _ := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	sent := createMailbox(t, ts, `{"name":"Sent"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil),
		map[string]bool{drafts: true}, map[string]bool{"$draft": true})

	// The request mirrors the RFC's create k1490 with the derived-envelope
	// form (mailFrom/rcptTo come from the message) plus the exact
	// onSuccessUpdateEmail patch of the example.
	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,
		  "create":{"k1490":{"identityId":%q,"emailId":%q}},
		  "onSuccessUpdateEmail":{"#k1490":{
		    "mailboxIds/%s":null,
		    "mailboxIds/%s":true,
		    "keywords/$draft":null}}}`,
		testAccount, identityId, emailId, drafts, sent), "0"))

	// Two responses, both call id "0" (the implicit Email/set shares it).
	if len(r.MethodResponses) != 2 {
		t.Fatalf("want 2 method responses (submission + implicit set), got %d: %v",
			len(r.MethodResponses), r.MethodResponses)
	}
	if r.MethodResponses[0].Name != "EmailSubmission/set" || r.MethodResponses[1].Name != "Email/set" {
		t.Fatalf("response names = %q, %q", r.MethodResponses[0].Name, r.MethodResponses[1].Name)
	}
	if r.MethodResponses[0].CallID != "0" || r.MethodResponses[1].CallID != "0" {
		t.Fatalf("call ids = %q, %q, want both 0", r.MethodResponses[0].CallID, r.MethodResponses[1].CallID)
	}

	// EmailSubmission/set: created.k1490.id present, and newState advanced
	// from oldState (the RFC shows distinct old/new states).
	sub := methodArgs(t, r, 0, "EmailSubmission/set")
	oldState, _ := sub["oldState"].(string)
	newState, _ := sub["newState"].(string)
	if newState == "" || newState == oldState {
		t.Errorf("submission state did not advance: old=%q new=%q", oldState, newState)
	}
	created, ok := sub["created"].(map[string]any)["k1490"].(map[string]any)
	if !ok {
		t.Fatalf("k1490 not created: %v", sub)
	}
	if id, _ := created["id"].(string); id == "" {
		t.Errorf("created id missing: %v", created)
	}

	// Email/set: the RFC's response has updated[emailId] == null and its
	// own advanced state.
	es := methodArgs(t, r, 1, "Email/set")
	eOld, _ := es["oldState"].(string)
	eNew, _ := es["newState"].(string)
	if eNew == "" || eNew == eOld {
		t.Errorf("email state did not advance: old=%q new=%q", eOld, eNew)
	}
	updated, ok := es["updated"].(map[string]any)
	if !ok {
		t.Fatalf("Email/set has no updated map: %v", es)
	}
	v, present := updated[emailId]
	if !present {
		t.Fatalf("updated does not mention %s: %v", emailId, updated)
	}
	if v != nil {
		t.Errorf("updated[%s] = %v, RFC shows null", emailId, v)
	}
}

// TestConformanceSubmission751Forbidden is the second half of the 7.5.1
// example: sending rights are gone, so the create is rejected with
// forbiddenToSend and NO state change (oldState == newState, since nothing
// was created). The description is the policy's, meant for display.
func TestConformanceSubmission751Forbidden(t *testing.T) {
	ts, db, store, policy := submissionServer(t)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	policy.denySend = true
	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"k1490":{"identityId":%q,"emailId":%q}}}`,
		testAccount, identityId, emailId), "0"))

	// A rejected create is a single response: no implicit Email/set runs
	// (onSuccess only fires for successful creations).
	if len(r.MethodResponses) != 1 {
		t.Fatalf("forbidden create should be one response, got %d: %v",
			len(r.MethodResponses), r.MethodResponses)
	}
	args := methodArgs(t, r, 0, "EmailSubmission/set")
	if old, _ := args["oldState"].(string); old != args["newState"] {
		t.Errorf("state changed on a rejected create: old=%v new=%v", args["oldState"], args["newState"])
	}
	nc, ok := args["notCreated"].(map[string]any)["k1490"].(map[string]any)
	if !ok {
		t.Fatalf("k1490 not in notCreated: %v", args)
	}
	if nc["type"] != "forbiddenToSend" {
		t.Errorf("error type = %v, want forbiddenToSend", nc["type"])
	}
	if desc, _ := nc["description"].(string); desc == "" {
		t.Errorf("forbiddenToSend has no description (RFC: intended for display): %v", nc)
	}
}
