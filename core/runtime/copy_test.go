package runtime

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/naust-mail/naust-jmap/core/internal/authtest"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// copyServer is noteServer plus the account topology /copy needs:
// john's personal Atest1, a shared writable Ateam, and Aro, which jane
// can write but john can only read.
func copyServer(t *testing.T, core jmap.CoreCapabilities) *httptest.Server {
	t.Helper()
	a := authtest.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	a.AddUser("jane@example.com", "secret2", "Ajane")
	a.AddAccess("john@example.com", "Ateam", auth.Access{Name: "team"})
	a.AddAccess("john@example.com", "Aro", auth.Access{Name: "shared", ReadOnly: true})
	a.AddAccess("jane@example.com", "Aro", auth.Access{Name: "shared"})
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	if err := RegisterStandardType(p, db, testNoteType(), core); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability("urn:example:testnote", struct{}{}, struct{}{}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// getOne fetches one record by id from an account, failing the test if
// it is not exactly found.
func getOne(t *testing.T, ts *httptest.Server, acct string, id string) map[string]any {
	t.Helper()
	r := callAPI(t, ts, inv("TestNote/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q]}`, acct, id), "0"))
	g := methodArgs(t, r, 0, "TestNote/get")
	list := g["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("get %s in %s: list %v, notFound %v", id, acct, list, g["notFound"])
	}
	return list[0].(map[string]any)
}

// TestCopyMovesBetweenAccounts is the section 5.4 story (and the
// spec's Todo/copy example): copy to another account with
// onSuccessDestroyOriginal, giving two responses under one call id.
func TestCopyMovesBetweenAccounts(t *testing.T) {
	ts := copyServer(t, DefaultCoreCapabilities())
	orig := createNote(t, ts, `{"subject":"hello","kind":"memo","flagged":true}`)

	r := callAPI(t, ts, inv("TestNote/copy", fmt.Sprintf(
		`{"fromAccountId":"Atest1","accountId":"Ateam","create":{"k5122":{"id":%q}},"onSuccessDestroyOriginal":true}`,
		orig), "0"))
	// The implicit Foo/set produces a second response to the single
	// method call, both with the same method call id (5.4).
	if len(r.MethodResponses) != 2 {
		t.Fatalf("%d responses, want 2", len(r.MethodResponses))
	}
	for i, mr := range r.MethodResponses {
		if mr.CallID != "0" {
			t.Fatalf("response %d call id %q", i, mr.CallID)
		}
	}
	args := methodArgs(t, r, 0, "TestNote/copy")
	if args["fromAccountId"] != "Atest1" || args["accountId"] != "Ateam" {
		t.Fatalf("account echo: %v", args)
	}
	if args["oldState"] != "0" || args["newState"] != "1" {
		t.Fatalf("states: old=%v new=%v", args["oldState"], args["newState"])
	}
	echo := args["created"].(map[string]any)["k5122"].(map[string]any)
	newId := echo["id"].(string)
	if newId == "" || newId == orig {
		t.Fatalf("copy id %q (original %q)", newId, orig)
	}
	// The created echo is what the server set: the new id and the
	// server-set revision; copied values are not echoed back (5.4).
	if echo["revision"] != float64(1) || len(echo) != 2 {
		t.Fatalf("created echo: %v", echo)
	}
	if v, has := args["notCreated"]; !has || v != nil {
		t.Fatalf("notCreated: %v (%v)", v, has)
	}

	set := methodArgs(t, r, 1, "TestNote/set")
	if set["accountId"] != "Atest1" {
		t.Fatalf("implicit set account: %v", set)
	}
	destroyed := set["destroyed"].([]any)
	if len(destroyed) != 1 || destroyed[0] != orig {
		t.Fatalf("implicit destroy: %v", set)
	}

	// The copy carries every property, immutables included; the
	// original is gone.
	got := getOne(t, ts, "Ateam", newId)
	if got["subject"] != "hello" || got["kind"] != "memo" || got["flagged"] != true {
		t.Fatalf("copied record: %v", got)
	}
	r = callAPI(t, ts, inv("TestNote/get",
		fmt.Sprintf(`{"accountId":"Atest1","ids":[%q]}`, orig), "0"))
	g := methodArgs(t, r, 0, "TestNote/get")
	if nf := g["notFound"].([]any); len(nf) != 1 || nf[0] != orig {
		t.Fatalf("original survived: %v", g)
	}
}

// TestCopyPropertyOverride: properties included in the create object
// are used instead of the original's current values (5.4); without
// onSuccessDestroyOriginal the original stays.
func TestCopyPropertyOverride(t *testing.T) {
	ts := copyServer(t, DefaultCoreCapabilities())
	orig := createNote(t, ts, `{"subject":"orig","body":"b1"}`)

	r := callAPI(t, ts, inv("TestNote/copy", fmt.Sprintf(
		`{"fromAccountId":"Atest1","accountId":"Ateam","create":{"c":{"id":%q,"subject":"replaced"}}}`,
		orig), "0"))
	if len(r.MethodResponses) != 1 {
		t.Fatalf("%d responses, want 1 (no implicit set)", len(r.MethodResponses))
	}
	args := methodArgs(t, r, 0, "TestNote/copy")
	newId := args["created"].(map[string]any)["c"].(map[string]any)["id"].(string)

	got := getOne(t, ts, "Ateam", newId)
	if got["subject"] != "replaced" || got["body"] != "b1" {
		t.Fatalf("override result: %v", got)
	}
	getOne(t, ts, "Atest1", orig)
}

// TestCopyValidation: each record copy is an atomic unit that fails
// individually with a standard SetError (5.4).
func TestCopyValidation(t *testing.T) {
	ts := copyServer(t, DefaultCoreCapabilities())
	orig := createNote(t, ts, `{"subject":"src"}`)

	for _, tc := range []struct {
		name, create, wantType, wantProp string
	}{
		{"missing id", `{"subject":"x"}`, jmap.SetErrInvalidProperties, "id"},
		{"unresolved id reference", `{"id":"#nope"}`, jmap.SetErrInvalidProperties, "id"},
		{"nonexistent original", `{"id":"Nmissing"}`, jmap.SetErrNotFound, ""},
		{"server-set override", fmt.Sprintf(`{"id":%q,"revision":5}`, orig), jmap.SetErrInvalidProperties, "revision"},
		{"unknown property", fmt.Sprintf(`{"id":%q,"bogus":1}`, orig), jmap.SetErrInvalidProperties, "bogus"},
		{"wrong kind", fmt.Sprintf(`{"id":%q,"flagged":"yes"}`, orig), jmap.SetErrInvalidProperties, "flagged"},
	} {
		r := callAPI(t, ts, inv("TestNote/copy", fmt.Sprintf(
			`{"fromAccountId":"Atest1","accountId":"Ateam","create":{"c":%s}}`, tc.create), "0"))
		args := methodArgs(t, r, 0, "TestNote/copy")
		// Nothing was copied: created is null, not an empty map (5.4).
		if v, has := args["created"]; !has || v != nil {
			t.Errorf("%s: created = %v (%v)", tc.name, v, has)
		}
		wantSetError(t, args, "notCreated", "c", tc.wantType, tc.wantProp)
	}
}

// TestCopyMethodErrors covers the argument-level rejections: the 5.4
// specific errors plus the standard account checks.
func TestCopyMethodErrors(t *testing.T) {
	core := DefaultCoreCapabilities()
	core.MaxObjectsInSet = 1
	ts := copyServer(t, core)
	orig := createNote(t, ts, `{"subject":"src"}`)

	for _, tc := range []struct{ name, argsJSON, want string }{
		{"unknown from account",
			`{"fromAccountId":"Abogus","accountId":"Ateam","create":{}}`, jmap.ErrFromAccountNotFound},
		{"unknown target account",
			`{"fromAccountId":"Atest1","accountId":"Abogus","create":{}}`, jmap.ErrAccountNotFound},
		{"same account",
			`{"fromAccountId":"Atest1","accountId":"Atest1","create":{}}`, jmap.ErrInvalidArguments},
		{"missing fromAccountId",
			`{"accountId":"Ateam","create":{}}`, jmap.ErrInvalidArguments},
		{"read-only target",
			`{"fromAccountId":"Atest1","accountId":"Aro","create":{}}`, jmap.ErrAccountReadOnly},
		{"too many creates", fmt.Sprintf(
			`{"fromAccountId":"Atest1","accountId":"Ateam","create":{"a":{"id":%q},"b":{"id":%q}}}`,
			orig, orig), jmap.ErrRequestTooLarge},
	} {
		r := callAPI(t, ts, inv("TestNote/copy", tc.argsJSON, "0"))
		e := methodArgs(t, r, 0, "error")
		if e["type"] != tc.want {
			t.Errorf("%s: %v, want %s", tc.name, e["type"], tc.want)
		}
	}

	// An account the caller cannot reach at all is fromAccountNotFound
	// for them, even though it exists.
	r := callAPIAs(t, ts, "jane@example.com", "secret2", inv("TestNote/copy",
		`{"fromAccountId":"Atest1","accountId":"Ajane","create":{}}`, "0"))
	e := methodArgs(t, r, 0, "error")
	if e["type"] != jmap.ErrFromAccountNotFound {
		t.Errorf("inaccessible from account: %v", e["type"])
	}
}

// TestCopyStateMismatch: ifFromInState guards the from account,
// ifInState the target, destroyFromIfInState the implicit destroy -
// and the last can fail while the copy stands (5.4: the phases are not
// atomic).
func TestCopyStateMismatch(t *testing.T) {
	ts := copyServer(t, DefaultCoreCapabilities())
	orig := createNote(t, ts, `{"subject":"src"}`)

	for _, tc := range []struct{ name, argsJSON string }{
		{"ifFromInState", fmt.Sprintf(
			`{"fromAccountId":"Atest1","ifFromInState":"999","accountId":"Ateam","create":{"c":{"id":%q}}}`, orig)},
		{"ifInState", fmt.Sprintf(
			`{"fromAccountId":"Atest1","accountId":"Ateam","ifInState":"999","create":{"c":{"id":%q}}}`, orig)},
	} {
		r := callAPI(t, ts, inv("TestNote/copy", tc.argsJSON, "0"))
		e := methodArgs(t, r, 0, "error")
		if e["type"] != jmap.ErrStateMismatch {
			t.Errorf("%s: %v, want stateMismatch", tc.name, e["type"])
		}
	}
	// Nothing landed in the target account.
	r := callAPI(t, ts, inv("TestNote/get", `{"accountId":"Ateam","ids":null}`, "0"))
	if list := methodArgs(t, r, 0, "TestNote/get")["list"].([]any); len(list) != 0 {
		t.Fatalf("aborted copies landed: %v", list)
	}

	// With matching states the same call succeeds; a stale
	// destroyFromIfInState aborts only the implicit set.
	r = callAPI(t, ts, inv("TestNote/copy", fmt.Sprintf(
		`{"fromAccountId":"Atest1","ifFromInState":"1","accountId":"Ateam","ifInState":"0","create":{"c":{"id":%q}},"onSuccessDestroyOriginal":true,"destroyFromIfInState":"999"}`,
		orig), "0"))
	args := methodArgs(t, r, 0, "TestNote/copy")
	if _, ok := args["created"].(map[string]any)["c"]; !ok {
		t.Fatalf("copy failed: %v", args)
	}
	e := methodArgs(t, r, 1, "error")
	if e["type"] != jmap.ErrStateMismatch {
		t.Fatalf("implicit set: %v, want stateMismatch", e["type"])
	}
	getOne(t, ts, "Atest1", orig)
}

// TestCopyCreationIdInterplay: creation ids are request-wide (3.7) -
// a copy can name a record made by an earlier /set as its source, one
// copy can reference another's creation id in an Id property, and the
// implicit destroy handles two copies of one original once.
func TestCopyCreationIdInterplay(t *testing.T) {
	ts := copyServer(t, DefaultCoreCapabilities())

	r := callAPI(t, ts,
		inv("TestNote/set", `{"accountId":"Atest1","create":{"x":{"subject":"tree root"}}}`, "0"),
		inv("TestNote/copy",
			`{"fromAccountId":"Atest1","accountId":"Ateam","create":{"cA":{"id":"#x"},"cB":{"id":"#x","parentId":"#cA"}},"onSuccessDestroyOriginal":true}`,
			"1"))
	if len(r.MethodResponses) != 3 {
		t.Fatalf("%d responses, want 3", len(r.MethodResponses))
	}
	orig := methodArgs(t, r, 0, "TestNote/set")["created"].(map[string]any)["x"].(map[string]any)["id"].(string)
	args := methodArgs(t, r, 1, "TestNote/copy")
	created, ok := args["created"].(map[string]any)
	if !ok || len(created) != 2 {
		t.Fatalf("copies: %v", args)
	}
	idA := created["cA"].(map[string]any)["id"].(string)
	idB := created["cB"].(map[string]any)["id"].(string)

	// cB's overlaid parentId resolved to cA's id in the target account.
	got := getOne(t, ts, "Ateam", idB)
	if got["parentId"] != idA {
		t.Fatalf("parentId %v, want %s", got["parentId"], idA)
	}

	// One original, copied twice, destroyed exactly once.
	set := methodArgs(t, r, 2, "TestNote/set")
	destroyed, _ := set["destroyed"].([]any)
	if len(destroyed) != 1 || destroyed[0] != orig {
		t.Fatalf("implicit destroy: %v", set)
	}
	if nd, has := set["notDestroyed"]; has && nd != nil {
		t.Fatalf("duplicate destroy attempted: %v", nd)
	}
}

// TestCopyFromReadOnlyAccount: reading from an account the caller may
// not write is fine, but the implicit destroy then fails - the spec's
// copy-succeeded-original-not-destroyed case (5.4).
func TestCopyFromReadOnlyAccount(t *testing.T) {
	ts := copyServer(t, DefaultCoreCapabilities())
	r := callAPIAs(t, ts, "jane@example.com", "secret2", inv("TestNote/set",
		`{"accountId":"Aro","create":{"c":{"subject":"shared note"}}}`, "0"))
	orig := methodArgs(t, r, 0, "TestNote/set")["created"].(map[string]any)["c"].(map[string]any)["id"].(string)

	r = callAPI(t, ts, inv("TestNote/copy", fmt.Sprintf(
		`{"fromAccountId":"Aro","accountId":"Atest1","create":{"c":{"id":%q}},"onSuccessDestroyOriginal":true}`,
		orig), "0"))
	args := methodArgs(t, r, 0, "TestNote/copy")
	newId, ok := args["created"].(map[string]any)["c"].(map[string]any)["id"].(string)
	if !ok || newId == "" {
		t.Fatalf("copy from read-only source failed: %v", args)
	}
	e := methodArgs(t, r, 1, "error")
	if e["type"] != jmap.ErrAccountReadOnly {
		t.Fatalf("implicit destroy: %v, want accountReadOnly", e["type"])
	}

	// The copy exists; the original survived.
	getOne(t, ts, "Atest1", newId)
	r = callAPIAs(t, ts, "jane@example.com", "secret2", inv("TestNote/get",
		fmt.Sprintf(`{"accountId":"Aro","ids":[%q]}`, orig), "0"))
	if list := methodArgs(t, r, 0, "TestNote/get")["list"].([]any); len(list) != 1 {
		t.Fatalf("original destroyed despite read-only access: %v", list)
	}
}

// TestCopyEmptyCreate: created and notCreated are both null when the
// create map is empty (5.4).
func TestCopyEmptyCreate(t *testing.T) {
	ts := copyServer(t, DefaultCoreCapabilities())
	r := callAPI(t, ts, inv("TestNote/copy",
		`{"fromAccountId":"Atest1","accountId":"Ateam","create":{},"onSuccessDestroyOriginal":true}`, "0"))
	if len(r.MethodResponses) != 1 {
		t.Fatalf("%d responses, want 1 (nothing to destroy)", len(r.MethodResponses))
	}
	var resp struct {
		Created    json.RawMessage `json:"created"`
		NotCreated json.RawMessage `json:"notCreated"`
	}
	if err := json.Unmarshal(r.MethodResponses[0].Args, &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.Created) != "null" || string(resp.NotCreated) != "null" {
		t.Fatalf("created %s, notCreated %s, want null/null", resp.Created, resp.NotCreated)
	}
}
