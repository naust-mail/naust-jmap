package runtime

// Processor.ImplicitSet runs a type's /set as an implicit continuation of an
// in-flight call: the mechanism behind /copy's onSuccessDestroyOriginal
// (RFC 8620 section 5.4) and EmailSubmission/set's onSuccessUpdateEmail /
// onSuccessDestroyEmail (RFC 8621 section 7.5). The tests here exercise it
// through a custom method the way a datatype module would.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naust-mail/naust-jmap/core/internal/authtest"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// relayServer registers TestNote plus a custom method, TestNote/relay, under
// its own capability. The relay forwards its raw arguments to an implicit
// TestNote/set via ImplicitSet, appended after its own reply - the shape of
// a real onSuccess* extension. Aro is writable by jane but read-only to
// john, who authenticates the test requests.
func relayServer(t *testing.T, core jmap.CoreCapabilities) *httptest.Server {
	t.Helper()
	a := authtest.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	a.AddUser("jane@example.com", "secret2", "Ajane")
	a.AddAccess("john@example.com", "Aro", auth.Access{Name: "shared", ReadOnly: true})
	a.AddAccess("jane@example.com", "Aro", auth.Access{Name: "shared"})
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	if err := RegisterStandardType(p, db, testNoteType(), core); err != nil {
		t.Fatal(err)
	}
	p.Register("TestNote/relay", "urn:example:relay", func(ctx context.Context, call *Call) []jmap.Invocation {
		out := reply("TestNote/relay", call.CallID, map[string]bool{"ok": true})
		return append(out, p.ImplicitSet(ctx, "TestNote", json.RawMessage(call.Args), call)...)
	})
	srv, err := NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"urn:example:testnote", "urn:example:relay"} {
		if err := srv.RegisterCapability(c, struct{}{}, struct{}{}); err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// callAPIUsing is callAPI with an explicit capability opt-in list.
func callAPIUsing(t *testing.T, ts *httptest.Server, using []string, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"using": using, "methodCalls": calls})
	if err != nil {
		t.Fatal(err)
	}
	resp := post(t, ts, string(body), "application/json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out jmap.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// TestImplicitSetContinuation: one client call yields two responses under
// one call id (5.4's "the output of this is added to the responses as
// normal"), and the continuation is NOT capability-gated - the request opts
// in to the relay capability only, so a direct TestNote/set is
// unknownMethod, yet the implicit one runs (the section 7.5 situation: a
// submission-only client still gets its mandatory implicit Email/set).
func TestImplicitSetContinuation(t *testing.T) {
	ts := relayServer(t, DefaultCoreCapabilities())
	using := []string{jmap.CoreCapability, "urn:example:relay"}

	r := callAPIUsing(t, ts, using, inv("TestNote/set",
		`{"accountId":"Atest1","create":{"n1":{"subject":"direct"}}}`, "0"))
	var e struct {
		Type string `json:"type"`
	}
	json.Unmarshal(r.MethodResponses[0].Args, &e)
	if r.MethodResponses[0].Name != "error" || e.Type != jmap.ErrUnknownMethod {
		t.Fatalf("direct set without capability: %s %s", r.MethodResponses[0].Name, r.MethodResponses[0].Args)
	}

	r = callAPIUsing(t, ts, using, inv("TestNote/relay",
		`{"accountId":"Atest1","create":{"n1":{"subject":"hello"}}}`, "0"))
	if len(r.MethodResponses) != 2 {
		t.Fatalf("%d responses, want 2", len(r.MethodResponses))
	}
	for i, mr := range r.MethodResponses {
		if mr.CallID != "0" {
			t.Fatalf("response %d call id %q, want \"0\"", i, mr.CallID)
		}
	}
	methodArgs(t, r, 0, "TestNote/relay")
	set := methodArgs(t, r, 1, "TestNote/set")
	created, ok := set["created"].(map[string]any)
	if !ok {
		t.Fatalf("implicit create failed: %v", set)
	}
	id := created["n1"].(map[string]any)["id"].(string)

	// The record is real: readable through the ordinary front door.
	got := getOne(t, ts, "Atest1", id)
	if got["subject"] != "hello" {
		t.Fatalf("stored record: %v", got)
	}
}

// TestImplicitSetInheritsCreatedIds: the continuation shares the request-
// wide creation-id map (section 5.3), so a "#creationId" minted by an
// earlier call in the same request resolves inside the implicit set.
func TestImplicitSetInheritsCreatedIds(t *testing.T) {
	ts := relayServer(t, DefaultCoreCapabilities())
	using := []string{jmap.CoreCapability, "urn:example:testnote", "urn:example:relay"}

	r := callAPIUsing(t, ts, using,
		inv("TestNote/set", `{"accountId":"Atest1","create":{"c1":{"subject":"doomed"}}}`, "0"),
		inv("TestNote/relay", `{"accountId":"Atest1","destroy":["#c1"]}`, "1"))
	if len(r.MethodResponses) != 3 {
		t.Fatalf("%d responses, want 3", len(r.MethodResponses))
	}
	created := methodArgs(t, r, 0, "TestNote/set")["created"].(map[string]any)
	realId := created["c1"].(map[string]any)["id"].(string)
	set := methodArgs(t, r, 2, "TestNote/set")
	destroyed, ok := set["destroyed"].([]any)
	if !ok || len(destroyed) != 1 || destroyed[0] != realId {
		t.Fatalf("implicit destroy of #c1: %v", set)
	}
}

// TestImplicitSetInheritsIdentity: the continuation runs as the original
// caller, so the account checks of the derived /set still apply - john's
// relay against his read-only account is accountReadOnly, same call id.
func TestImplicitSetInheritsIdentity(t *testing.T) {
	ts := relayServer(t, DefaultCoreCapabilities())
	using := []string{jmap.CoreCapability, "urn:example:relay"}

	r := callAPIUsing(t, ts, using, inv("TestNote/relay",
		`{"accountId":"Aro","create":{"n1":{"subject":"nope"}}}`, "0"))
	if len(r.MethodResponses) != 2 {
		t.Fatalf("%d responses, want 2", len(r.MethodResponses))
	}
	got := r.MethodResponses[1]
	var e struct {
		Type string `json:"type"`
	}
	json.Unmarshal(got.Args, &e)
	if got.Name != "error" || e.Type != jmap.ErrAccountReadOnly || got.CallID != "0" {
		t.Fatalf("implicit set on read-only account: %s %s (call id %q)", got.Name, got.Args, got.CallID)
	}
}

// TestImplicitSetErrors: a target type with no registered /set and
// unmarshalable arguments both answer as a serverFail error invocation
// under the original call id, never a panic.
func TestImplicitSetErrors(t *testing.T) {
	p := NewProcessor()
	orig := &Call{Name: "Thing/frob", CallID: "c7"}

	check := func(what string, out []jmap.Invocation) {
		t.Helper()
		if len(out) != 1 || out[0].Name != "error" || out[0].CallID != "c7" {
			t.Fatalf("%s: %v", what, out)
		}
		var e struct {
			Type string `json:"type"`
		}
		json.Unmarshal(out[0].Args, &e)
		if e.Type != jmap.ErrServerFail {
			t.Fatalf("%s: error type %q, want serverFail", what, e.Type)
		}
	}
	check("unregistered type", p.ImplicitSet(context.Background(), "Nonexistent", map[string]any{}, orig))
	check("unmarshalable args", p.ImplicitSet(context.Background(), "Nonexistent", make(chan int), orig))
}

// TestCopyImplicitDestroyUnchanged: the /copy refactor onto ImplicitSet is
// behavior-preserving for the one pre-existing consumer - the section 5.4
// two-responses-one-callid flow (already covered in copy_test.go) also
// still resolves destroyFromIfInState correctly.
func TestCopyImplicitDestroyUnchanged(t *testing.T) {
	ts := copyServer(t, DefaultCoreCapabilities())
	orig := createNote(t, ts, `{"subject":"movable","kind":"memo"}`)

	// destroyFromIfInState passes through as the implicit set's ifInState:
	// a stale value aborts the destroy with stateMismatch while the copy
	// stands (5.4).
	r := callAPI(t, ts, inv("TestNote/copy", fmt.Sprintf(
		`{"fromAccountId":"Atest1","accountId":"Ateam","create":{"k1":{"id":%q}},"onSuccessDestroyOriginal":true,"destroyFromIfInState":"stale"}`,
		orig), "0"))
	if len(r.MethodResponses) != 2 {
		t.Fatalf("%d responses, want 2", len(r.MethodResponses))
	}
	methodArgs(t, r, 0, "TestNote/copy")
	got := r.MethodResponses[1]
	var e struct {
		Type string `json:"type"`
	}
	json.Unmarshal(got.Args, &e)
	if got.Name != "error" || e.Type != jmap.ErrStateMismatch || got.CallID != "0" {
		t.Fatalf("stale destroyFromIfInState: %s %s", got.Name, got.Args)
	}
	// The copy stood; the original survived the aborted destroy.
	getOne(t, ts, "Atest1", orig)
}
