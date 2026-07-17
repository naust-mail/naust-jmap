package mail

// EmailDelivery push-type tests (RFC 8621 section 1.5): the state string
// changes when a new Email is added, and only then.

import (
	"context"
	"fmt"
	"testing"
)

// TestEmailDeliveryStateContract is the RFC 8621 section 1.5.1 example:
// adding a new Email changes the EmailDelivery state; marking it read and
// destroying it change the Email state but leave EmailDelivery untouched.
func TestEmailDeliveryStateContract(t *testing.T) {
	ts, db, store := emailServer(t)
	ctx := context.Background()

	state := func(typeName string) string {
		s, err := db.TypeState(ctx, testAccount, typeName)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// A type never written is "0" (RFC 8620 section 5.1 state semantics).
	if got := state(TypeEmailDelivery); got != "0" {
		t.Fatalf("initial EmailDelivery state = %q, want 0", got)
	}

	// New mail arrives: EmailDelivery MUST change.
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	id := putEmail(t, db, store, simpleMessage, map[string]bool{inbox: true}, nil)
	deliveredState := state(TypeEmailDelivery)
	emailStateAfterAdd := state(TypeEmail)
	if deliveredState == "0" {
		t.Fatal("EmailDelivery state did not change when a new Email was added")
	}

	// The user marks it $seen: Email state changes, EmailDelivery does NOT
	// (section 1.5 "SHOULD NOT change ... if one is marked as read").
	callMail(t, ts, inv("Email/set",
		fmt.Sprintf(`{"accountId":%q,"update":{%q:{"keywords/$seen":true}}}`, testAccount, id), "0"))
	if got := state(TypeEmailDelivery); got != deliveredState {
		t.Fatalf("EmailDelivery moved on mark-read: %q -> %q", deliveredState, got)
	}
	if state(TypeEmail) == emailStateAfterAdd {
		t.Fatal("Email state did not change on mark-read (test precondition broken)")
	}

	// The user destroys it: again Email changes, EmailDelivery does not
	// (section 1.5 "or deleted").
	callMail(t, ts, inv("Email/set",
		fmt.Sprintf(`{"accountId":%q,"destroy":[%q]}`, testAccount, id), "0"))
	if got := state(TypeEmailDelivery); got != deliveredState {
		t.Fatalf("EmailDelivery moved on destroy: %q -> %q", deliveredState, got)
	}
}

// TestEmailDeliveryIsMethodlessPushType: EmailDelivery is a known push type
// (so a client can subscribe with types=EmailDelivery) but has no methods -
// "There are no methods to act on this type" (section 1.5).
func TestEmailDeliveryIsMethodlessPushType(t *testing.T) {
	ts, db, _ := emailServer(t)

	// Known to the object DB, so EventSource types= and the initial-state
	// push can name it.
	found := false
	for _, n := range db.TypeNames() {
		if n == TypeEmailDelivery {
			found = true
		}
	}
	if !found {
		t.Fatal("EmailDelivery not in TypeNames; it cannot be a push type")
	}
	if _, err := db.TypeState(context.Background(), testAccount, TypeEmailDelivery); err != nil {
		t.Fatalf("EmailDelivery has no readable state: %v", err)
	}

	// No method acts on it: EmailDelivery/get is unknown.
	r := callMail(t, ts, inv("EmailDelivery/get",
		fmt.Sprintf(`{"accountId":%q,"ids":null}`, testAccount), "0"))
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("EmailDelivery/get answered %s, want error", r.MethodResponses[0].Name)
	}
	if got := methodArgs(t, r, 0, "error")["type"]; got != "unknownMethod" {
		t.Fatalf("EmailDelivery/get error type = %v, want unknownMethod", got)
	}
}
