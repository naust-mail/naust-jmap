package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/internal/authtest"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// TestNote is the boring baseline datatype: one property of each interesting
// attribute so every RFC 8620 section 5.3 rule has a target.
func testNoteType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "TestNote",
		Capability: "urn:example:testnote",
		Properties: map[string]descriptor.Property{
			"subject":  {Kind: descriptor.KindString, Indexed: true},
			"body":     {Kind: descriptor.KindString, Default: json.RawMessage(`""`)},
			"kind":     {Kind: descriptor.KindString, Immutable: true},
			"revision": {Kind: descriptor.KindUnsignedInt, ServerSet: true, Default: json.RawMessage(`1`)},
			"parentId": {Kind: descriptor.KindId},
			"flagged":  {Kind: descriptor.KindBool, Indexed: true, Default: json.RawMessage(`false`)},
			"labels":   {Kind: descriptor.KindObject},
			"tags":     {Kind: descriptor.KindArray},
		},
	}
}

func noteServer(t *testing.T, core jmap.CoreCapabilities) *httptest.Server {
	t.Helper()
	a := authtest.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
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

func inv(name, args, callID string) jmap.Invocation {
	return jmap.Invocation{Name: name, Args: json.RawMessage(args), CallID: callID}
}

func callAPI(t *testing.T, ts *httptest.Server, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	req := map[string]any{
		"using":       []string{jmap.CoreCapability, "urn:example:testnote"},
		"methodCalls": calls,
	}
	body, err := json.Marshal(req)
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

// methodArgs asserts response invocation i has the wanted name and
// returns its decoded arguments.
func methodArgs(t *testing.T, r *jmap.Response, i int, wantName string) map[string]any {
	t.Helper()
	if i >= len(r.MethodResponses) {
		t.Fatalf("no method response %d (have %d)", i, len(r.MethodResponses))
	}
	got := r.MethodResponses[i]
	if got.Name != wantName {
		t.Fatalf("response %d is %s (%s), want %s", i, got.Name, got.Args, wantName)
	}
	var m map[string]any
	if err := json.Unmarshal(got.Args, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// createNote makes one note and returns its server id.
func createNote(t *testing.T, ts *httptest.Server, props string) string {
	t.Helper()
	r := callAPI(t, ts, inv("TestNote/set",
		fmt.Sprintf(`{"accountId":"Atest1","create":{"c":%s}}`, props), "0"))
	args := methodArgs(t, r, 0, "TestNote/set")
	created, ok := args["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", args)
	}
	return created["c"].(map[string]any)["id"].(string)
}

func TestSetCreateDefaultsAndServerSet(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	r := callAPI(t, ts, inv("TestNote/set",
		`{"accountId":"Atest1","create":{"n1":{"subject":"hello"}}}`, "0"))
	args := methodArgs(t, r, 0, "TestNote/set")

	if args["oldState"] != "0" || args["newState"] != "1" {
		t.Fatalf("states: old=%v new=%v", args["oldState"], args["newState"])
	}
	created := args["created"].(map[string]any)["n1"].(map[string]any)
	// Everything the client did not send comes back (5.3): id, defaults,
	// server-set - but not the sent subject.
	if created["id"] == "" || created["body"] != "" || created["flagged"] != false || created["revision"] != float64(1) {
		t.Fatalf("created props: %v", created)
	}
	if _, has := created["subject"]; has {
		t.Fatalf("created echoed a client-sent property: %v", created)
	}

	// /get returns the full stored object.
	r = callAPI(t, ts, inv("TestNote/get",
		fmt.Sprintf(`{"accountId":"Atest1","ids":[%q]}`, created["id"]), "0"))
	got := methodArgs(t, r, 0, "TestNote/get")
	list := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list: %v", list)
	}
	obj := list[0].(map[string]any)
	if obj["subject"] != "hello" || obj["body"] != "" || obj["revision"] != float64(1) {
		t.Fatalf("stored object: %v", obj)
	}
	if got["state"] != "1" {
		t.Fatalf("get state: %v", got["state"])
	}
}

func TestSetCreateRejections(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	// One good create among the bad: rejected records must not abort the
	// method (5.3), and the good one commits.
	r := callAPI(t, ts, inv("TestNote/set", `{"accountId":"Atest1","create":{
		"good":     {"subject":"ok"},
		"srvset":   {"subject":"x","revision":9},
		"unknown":  {"subject":"x","nope":true},
		"badkind":  {"subject":42},
		"withid":   {"id":"A123","subject":"x"},
		"notobj":   17
	}}`, "0"))
	args := methodArgs(t, r, 0, "TestNote/set")

	created := args["created"].(map[string]any)
	if _, ok := created["good"]; !ok || len(created) != 1 {
		t.Fatalf("created: %v", created)
	}
	nc := args["notCreated"].(map[string]any)
	wantProps := map[string][]string{
		"srvset": {"revision"}, "unknown": {"nope"}, "badkind": {"subject"}, "withid": {"id"},
	}
	for cid, want := range wantProps {
		se := nc[cid].(map[string]any)
		if se["type"] != "invalidProperties" {
			t.Fatalf("%s: %v", cid, se)
		}
		props := se["properties"].([]any)
		if len(props) != len(want) || props[0] != want[0] {
			t.Fatalf("%s properties: %v, want %v", cid, props, want)
		}
	}
	if nc["notobj"].(map[string]any)["type"] != "invalidProperties" {
		t.Fatalf("notobj: %v", nc["notobj"])
	}
	// One successful create: exactly one state bump.
	if args["newState"] != "1" {
		t.Fatalf("newState: %v", args["newState"])
	}
}

func TestSetCreationIdReferences(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	// "a" sorts before "z" but references it: the server must order the
	// creates so the referenced record exists first (5.3).
	r := callAPI(t, ts, inv("TestNote/set", `{"accountId":"Atest1","create":{
		"a": {"subject":"child","parentId":"#z"},
		"z": {"subject":"parent"}
	}}`, "0"))
	args := methodArgs(t, r, 0, "TestNote/set")
	created := args["created"].(map[string]any)
	if len(created) != 2 {
		t.Fatalf("created: %v", args)
	}
	parentId := created["z"].(map[string]any)["id"].(string)
	childId := created["a"].(map[string]any)["id"].(string)

	// The reference resolved to the parent's real id.
	r = callAPI(t, ts, inv("TestNote/get",
		fmt.Sprintf(`{"accountId":"Atest1","ids":[%q]}`, childId), "0"))
	obj := methodArgs(t, r, 0, "TestNote/get")["list"].([]any)[0].(map[string]any)
	if obj["parentId"] != parentId {
		t.Fatalf("parentId = %v, want %v", obj["parentId"], parentId)
	}

	// A reference that never resolves is invalidProperties (5.3).
	r = callAPI(t, ts, inv("TestNote/set",
		`{"accountId":"Atest1","create":{"x":{"parentId":"#ghost"}}}`, "0"))
	nc := methodArgs(t, r, 0, "TestNote/set")["notCreated"].(map[string]any)
	se := nc["x"].(map[string]any)
	if se["type"] != "invalidProperties" || se["properties"].([]any)[0] != "parentId" {
		t.Fatalf("unresolved ref: %v", se)
	}
}

func TestSetCreationIdsAcrossCalls(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	// The creation-id map is request-wide (5.3): create in call 0, patch
	// via "#n" in call 1, destroy via "#n" in call 2.
	r := callAPI(t, ts,
		inv("TestNote/set", `{"accountId":"Atest1","create":{"n":{"subject":"one"}}}`, "0"),
		inv("TestNote/set", `{"accountId":"Atest1","update":{"#n":{"subject":"two"}}}`, "1"),
		inv("TestNote/set", `{"accountId":"Atest1","destroy":["#n"]}`, "2"),
	)
	id := methodArgs(t, r, 0, "TestNote/set")["created"].(map[string]any)["n"].(map[string]any)["id"].(string)
	updated := methodArgs(t, r, 1, "TestNote/set")["updated"].(map[string]any)
	if v, ok := updated[id]; !ok || v != nil {
		t.Fatalf("updated: %v", updated)
	}
	destroyed := methodArgs(t, r, 2, "TestNote/set")["destroyed"].([]any)
	if len(destroyed) != 1 || destroyed[0] != id {
		t.Fatalf("destroyed: %v", destroyed)
	}
}

func TestGetSemantics(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	id1 := createNote(t, ts, `{"subject":"a"}`)
	id2 := createNote(t, ts, `{"subject":"b"}`)

	t.Run("null ids returns all", func(t *testing.T) {
		r := callAPI(t, ts, inv("TestNote/get", `{"accountId":"Atest1"}`, "0"))
		args := methodArgs(t, r, 0, "TestNote/get")
		if len(args["list"].([]any)) != 2 || len(args["notFound"].([]any)) != 0 {
			t.Fatalf("%v", args)
		}
	})
	t.Run("duplicate ids collapse and missing ids land in notFound", func(t *testing.T) {
		r := callAPI(t, ts, inv("TestNote/get",
			fmt.Sprintf(`{"accountId":"Atest1","ids":[%q,%q,"Amissing"]}`, id1, id1), "0"))
		args := methodArgs(t, r, 0, "TestNote/get")
		if len(args["list"].([]any)) != 1 {
			t.Fatalf("list: %v", args["list"])
		}
		nf := args["notFound"].([]any)
		if len(nf) != 1 || nf[0] != "Amissing" {
			t.Fatalf("notFound: %v", nf)
		}
	})
	t.Run("properties selection always includes id", func(t *testing.T) {
		r := callAPI(t, ts, inv("TestNote/get",
			fmt.Sprintf(`{"accountId":"Atest1","ids":[%q],"properties":["subject"]}`, id2), "0"))
		obj := methodArgs(t, r, 0, "TestNote/get")["list"].([]any)[0].(map[string]any)
		if obj["id"] != id2 || obj["subject"] != "b" || len(obj) != 2 {
			t.Fatalf("projected object: %v", obj)
		}
	})
	t.Run("invalid property rejects the call", func(t *testing.T) {
		r := callAPI(t, ts, inv("TestNote/get",
			`{"accountId":"Atest1","properties":["nope"]}`, "0"))
		if methodArgs(t, r, 0, "error")["type"] != "invalidArguments" {
			t.Fatal("want invalidArguments")
		}
	})
	t.Run("unknown account", func(t *testing.T) {
		r := callAPI(t, ts, inv("TestNote/get", `{"accountId":"Aother"}`, "0"))
		if methodArgs(t, r, 0, "error")["type"] != "accountNotFound" {
			t.Fatal("want accountNotFound")
		}
	})
}

func TestGetRequestTooLarge(t *testing.T) {
	core := DefaultCoreCapabilities()
	core.MaxObjectsInGet = 2
	ts := noteServer(t, core)
	for i := 0; i < 3; i++ {
		createNote(t, ts, fmt.Sprintf(`{"subject":"n%d"}`, i))
	}
	// Explicit ids over the limit.
	r := callAPI(t, ts, inv("TestNote/get",
		`{"accountId":"Atest1","ids":["A1","A2","A3"]}`, "0"))
	if methodArgs(t, r, 0, "error")["type"] != "requestTooLarge" {
		t.Fatal("want requestTooLarge for explicit ids")
	}
	// ids null with more records than maxObjectsInGet (5.1).
	r = callAPI(t, ts, inv("TestNote/get", `{"accountId":"Atest1"}`, "0"))
	if methodArgs(t, r, 0, "error")["type"] != "requestTooLarge" {
		t.Fatal("want requestTooLarge for null ids")
	}
}

func TestSetUpdateSemantics(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	parent := createNote(t, ts, `{"subject":"p"}`)
	id := createNote(t, ts, fmt.Sprintf(`{"subject":"s","body":"b","kind":"memo","parentId":%q}`, parent))

	patch := func(p string) map[string]any {
		r := callAPI(t, ts, inv("TestNote/set",
			fmt.Sprintf(`{"accountId":"Atest1","update":{%q:%s}}`, id, p), "0"))
		return methodArgs(t, r, 0, "TestNote/set")
	}
	wantInvalid := func(t *testing.T, args map[string]any, props ...string) {
		t.Helper()
		se := args["notUpdated"].(map[string]any)[id].(map[string]any)
		if se["type"] != "invalidProperties" {
			t.Fatalf("want invalidProperties: %v", se)
		}
		got := se["properties"].([]any)
		if len(got) != len(props) {
			t.Fatalf("properties: %v, want %v", got, props)
		}
		for i := range props {
			if got[i] != props[i] {
				t.Fatalf("properties: %v, want %v", got, props)
			}
		}
	}

	t.Run("simple patch", func(t *testing.T) {
		args := patch(`{"subject":"s2"}`)
		if v, ok := args["updated"].(map[string]any)[id]; !ok || v != nil {
			t.Fatalf("updated: %v", args)
		}
	})
	t.Run("null restores default or removes", func(t *testing.T) {
		patch(`{"body":null,"parentId":null}`)
		r := callAPI(t, ts, inv("TestNote/get",
			fmt.Sprintf(`{"accountId":"Atest1","ids":[%q]}`, id), "0"))
		obj := methodArgs(t, r, 0, "TestNote/get")["list"].([]any)[0].(map[string]any)
		if obj["body"] != "" { // body has default ""
			t.Fatalf("body: %v", obj["body"])
		}
		if _, has := obj["parentId"]; has { // parentId has no default
			t.Fatalf("parentId not removed: %v", obj)
		}
	})
	t.Run("immutable and server-set accepted only when identical", func(t *testing.T) {
		if _, ok := patch(`{"kind":"memo","revision":1}`)["updated"].(map[string]any)[id]; !ok {
			t.Fatal("identical values must be accepted")
		}
		wantInvalid(t, patch(`{"kind":"other"}`), "kind")
		wantInvalid(t, patch(`{"revision":2}`), "revision")
		wantInvalid(t, patch(`{"id":"Aforged"}`), "id")
	})
	t.Run("whole object back is a valid patch", func(t *testing.T) {
		r := callAPI(t, ts, inv("TestNote/get",
			fmt.Sprintf(`{"accountId":"Atest1","ids":[%q]}`, id), "0"))
		obj := methodArgs(t, r, 0, "TestNote/get")["list"].([]any)[0].(map[string]any)
		whole, _ := json.Marshal(obj)
		args := patch(string(whole))
		if _, ok := args["updated"].(map[string]any)[id]; !ok {
			t.Fatalf("whole-object patch rejected: %v", args)
		}
	})
	t.Run("unknown property", func(t *testing.T) {
		wantInvalid(t, patch(`{"nope":1}`), "nope")
	})
	t.Run("invalidPatch on pointer into non-object", func(t *testing.T) {
		se := patch(`{"subject/x":"v"}`)["notUpdated"].(map[string]any)[id].(map[string]any)
		if se["type"] != "invalidPatch" {
			t.Fatalf("%v", se)
		}
	})
	t.Run("invalidPatch on prefix overlap", func(t *testing.T) {
		se := patch(`{"subject":"a","subject/x":"v"}`)["notUpdated"].(map[string]any)[id].(map[string]any)
		if se["type"] != "invalidPatch" {
			t.Fatalf("%v", se)
		}
	})
	t.Run("notFound", func(t *testing.T) {
		r := callAPI(t, ts, inv("TestNote/set",
			`{"accountId":"Atest1","update":{"Amissing":{"subject":"x"}}}`, "0"))
		se := methodArgs(t, r, 0, "TestNote/set")["notUpdated"].(map[string]any)["Amissing"].(map[string]any)
		if se["type"] != "notFound" {
			t.Fatalf("%v", se)
		}
	})
}

// TestCompositeKindsAndMemberPatch covers KindObject/KindArray storage
// and RFC 8620 section 5.3 PatchObject member evaluation ({name}/{key}).
func TestCompositeKindsAndMemberPatch(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())

	getLabels := func(id string) map[string]any {
		r := callAPI(t, ts, inv("TestNote/get",
			fmt.Sprintf(`{"accountId":"Atest1","ids":[%q],"properties":["labels","tags"]}`, id), "0"))
		return methodArgs(t, r, 0, "TestNote/get")["list"].([]any)[0].(map[string]any)
	}
	id := createNote(t, ts, `{"subject":"s","labels":{"a":true},"tags":["x","y"]}`)

	t.Run("composite values stored and returned", func(t *testing.T) {
		obj := getLabels(id)
		if lbl := obj["labels"].(map[string]any); lbl["a"] != true || len(lbl) != 1 {
			t.Fatalf("labels: %v", obj["labels"])
		}
		if tags := obj["tags"].([]any); len(tags) != 2 || tags[0] != "x" {
			t.Fatalf("tags: %v", obj["tags"])
		}
	})

	patch := func(p string) map[string]any {
		r := callAPI(t, ts, inv("TestNote/set",
			fmt.Sprintf(`{"accountId":"Atest1","update":{%q:%s}}`, id, p), "0"))
		return methodArgs(t, r, 0, "TestNote/set")
	}

	t.Run("member add and remove accumulate", func(t *testing.T) {
		if _, ok := patch(`{"labels/b":true,"labels/c":true}`)["updated"].(map[string]any)[id]; !ok {
			t.Fatal("member add rejected")
		}
		lbl := getLabels(id)["labels"].(map[string]any)
		if lbl["a"] != true || lbl["b"] != true || lbl["c"] != true || len(lbl) != 3 {
			t.Fatalf("after add: %v", lbl)
		}
		patch(`{"labels/a":null}`) // null removes the member
		lbl = getLabels(id)["labels"].(map[string]any)
		if _, has := lbl["a"]; has || len(lbl) != 2 {
			t.Fatalf("after remove: %v", lbl)
		}
	})

	t.Run("whole-object replacement", func(t *testing.T) {
		patch(`{"labels":{"only":true}}`)
		lbl := getLabels(id)["labels"].(map[string]any)
		if lbl["only"] != true || len(lbl) != 1 {
			t.Fatalf("replace: %v", lbl)
		}
	})

	t.Run("non-object value rejected", func(t *testing.T) {
		se := patch(`{"labels":[1,2]}`)["notUpdated"].(map[string]any)[id].(map[string]any)
		if se["type"] != "invalidProperties" {
			t.Fatalf("%v", se)
		}
	})

	t.Run("array kind rejects non-array", func(t *testing.T) {
		se := patch(`{"tags":{"a":true}}`)["notUpdated"].(map[string]any)[id].(map[string]any)
		if se["type"] != "invalidProperties" {
			t.Fatalf("%v", se)
		}
	})

	t.Run("deep pointer is invalidPatch", func(t *testing.T) {
		se := patch(`{"labels/a/b":true}`)["notUpdated"].(map[string]any)[id].(map[string]any)
		if se["type"] != "invalidPatch" {
			t.Fatalf("%v", se)
		}
	})

	t.Run("member pointer into a non-object kind is invalidPatch", func(t *testing.T) {
		se := patch(`{"tags/0":"z"}`)["notUpdated"].(map[string]any)[id].(map[string]any)
		if se["type"] != "invalidPatch" {
			t.Fatalf("%v", se)
		}
	})
}

func TestSetDestroyAndIfInState(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	id := createNote(t, ts, `{"subject":"a"}`)

	// Stale ifInState aborts the whole method (5.3 stateMismatch).
	r := callAPI(t, ts, inv("TestNote/set",
		fmt.Sprintf(`{"accountId":"Atest1","ifInState":"0","destroy":[%q]}`, id), "0"))
	if methodArgs(t, r, 0, "error")["type"] != "stateMismatch" {
		t.Fatal("want stateMismatch")
	}

	// Correct ifInState applies.
	r = callAPI(t, ts, inv("TestNote/set",
		fmt.Sprintf(`{"accountId":"Atest1","ifInState":"1","destroy":[%q]}`, id), "0"))
	args := methodArgs(t, r, 0, "TestNote/set")
	if d := args["destroyed"].([]any); len(d) != 1 || d[0] != id {
		t.Fatalf("destroyed: %v", args)
	}

	// Destroying again: notFound.
	r = callAPI(t, ts, inv("TestNote/set",
		fmt.Sprintf(`{"accountId":"Atest1","destroy":[%q]}`, id), "0"))
	se := methodArgs(t, r, 0, "TestNote/set")["notDestroyed"].(map[string]any)[id].(map[string]any)
	if se["type"] != "notFound" {
		t.Fatalf("%v", se)
	}
}

func TestSetRequestTooLarge(t *testing.T) {
	core := DefaultCoreCapabilities()
	core.MaxObjectsInSet = 2
	ts := noteServer(t, core)
	r := callAPI(t, ts, inv("TestNote/set",
		`{"accountId":"Atest1","create":{"a":{},"b":{}},"destroy":["Ax"]}`, "0"))
	if methodArgs(t, r, 0, "error")["type"] != "requestTooLarge" {
		t.Fatal("want requestTooLarge")
	}
}

func TestSetOnReadOnlyAccount(t *testing.T) {
	core := DefaultCoreCapabilities()
	a := roAuth{}
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

	r := callAPI(t, ts, inv("TestNote/set",
		`{"accountId":"Atest1","create":{"a":{}}}`, "0"))
	if methodArgs(t, r, 0, "error")["type"] != "accountReadOnly" {
		t.Fatal("want accountReadOnly")
	}
	// Reads still work.
	r = callAPI(t, ts, inv("TestNote/get", `{"accountId":"Atest1"}`, "0"))
	methodArgs(t, r, 0, "TestNote/get")
}

// roAuth grants read-only access to Atest1 regardless of credentials.
type roAuth struct{}

func (roAuth) Authenticate(*http.Request) (*auth.Identity, error) {
	return &auth.Identity{
		Username: "ro@example.com",
		Accounts: map[jmap.Id]auth.Access{"Atest1": {Name: "ro", ReadOnly: true}},
		Primary:  "Atest1",
	}, nil
}

func TestChangesEndToEnd(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	id1 := createNote(t, ts, `{"subject":"a"}`) // state 1
	id2 := createNote(t, ts, `{"subject":"b"}`) // state 2
	callAPI(t, ts, inv("TestNote/set",
		fmt.Sprintf(`{"accountId":"Atest1","update":{%q:{"subject":"a2"}},"destroy":[%q]}`, id1, id2), "0")) // state 3

	r := callAPI(t, ts, inv("TestNote/changes",
		`{"accountId":"Atest1","sinceState":"1"}`, "0"))
	args := methodArgs(t, r, 0, "TestNote/changes")
	if args["oldState"] != "1" || args["newState"] != "3" || args["hasMoreChanges"] != false {
		t.Fatalf("%v", args)
	}
	// id2: created then destroyed since state 1 -> omitted entirely; id1
	// updated (5.2 coalescing).
	if c := args["created"].([]any); len(c) != 0 {
		t.Fatalf("created: %v", c)
	}
	if u := args["updated"].([]any); len(u) != 1 || u[0] != id1 {
		t.Fatalf("updated: %v", u)
	}
	if d := args["destroyed"].([]any); len(d) != 0 {
		t.Fatalf("destroyed: %v", d)
	}

	t.Run("maxChanges must be positive", func(t *testing.T) {
		r := callAPI(t, ts, inv("TestNote/changes",
			`{"accountId":"Atest1","sinceState":"1","maxChanges":0}`, "0"))
		if methodArgs(t, r, 0, "error")["type"] != "invalidArguments" {
			t.Fatal("want invalidArguments")
		}
	})
	t.Run("unknown state", func(t *testing.T) {
		r := callAPI(t, ts, inv("TestNote/changes",
			`{"accountId":"Atest1","sinceState":"bogus"}`, "0"))
		if methodArgs(t, r, 0, "error")["type"] != "cannotCalculateChanges" {
			t.Fatal("want cannotCalculateChanges")
		}
	})
	t.Run("paging via hasMoreChanges", func(t *testing.T) {
		// 3 more creates, then walk from state 3 with maxChanges 2.
		var ids []string
		for i := 0; i < 3; i++ {
			ids = append(ids, createNote(t, ts, fmt.Sprintf(`{"subject":"p%d"}`, i)))
		}
		state := "3"
		var got []any
		for {
			r := callAPI(t, ts, inv("TestNote/changes",
				fmt.Sprintf(`{"accountId":"Atest1","sinceState":%q,"maxChanges":2}`, state), "0"))
			args := methodArgs(t, r, 0, "TestNote/changes")
			got = append(got, args["created"].([]any)...)
			state = args["newState"].(string)
			if args["hasMoreChanges"] != true {
				break
			}
		}
		if len(got) != 3 {
			t.Fatalf("paged created: %v, want %v", got, ids)
		}
	})
}

// TestChangesOmittedMaxChangesDefaults: when the client omits maxChanges the
// server applies its own default cap and pages the rest, rather than
// returning the whole change log at once. RFC 8620 section 5.2: "If not given
// by the client, the server may choose how many to return", and a maxChanges
// "set automatically by the server" MUST still bound the response. The
// default is shrunk here so a handful of records drives the boundary.
func TestChangesOmittedMaxChangesDefaults(t *testing.T) {
	orig := tuning.DefaultMaxChanges
	tuning.DefaultMaxChanges = 2
	t.Cleanup(func() { tuning.DefaultMaxChanges = orig })

	ts := noteServer(t, DefaultCoreCapabilities())
	for i := 0; i < 5; i++ {
		createNote(t, ts, fmt.Sprintf(`{"subject":"n%d"}`, i)) // states 1..5
	}

	seen := map[string]bool{}
	state := "0"
	pages := 0
	for {
		r := callAPI(t, ts, inv("TestNote/changes",
			fmt.Sprintf(`{"accountId":"Atest1","sinceState":%q}`, state), "0"))
		args := methodArgs(t, r, 0, "TestNote/changes")
		c := args["created"].([]any)
		// MUST NOT return more than the (auto-set) limit across all three
		// arrays. These are pure creates, so counting created suffices, but
		// assert the full sum to mirror the spec wording.
		n := len(c) + len(args["updated"].([]any)) + len(args["destroyed"].([]any))
		if n > tuning.DefaultMaxChanges {
			t.Fatalf("page returned %d ids, exceeds the auto-set cap %d", n, tuning.DefaultMaxChanges)
		}
		for _, id := range c {
			seen[id.(string)] = true
		}
		state = args["newState"].(string)
		pages++
		if args["hasMoreChanges"] != true {
			break
		}
		if pages > 10 {
			t.Fatal("paging never reported hasMoreChanges=false")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("collected %d distinct created ids across pages, want 5", len(seen))
	}
	if state != "5" {
		t.Fatalf("final newState = %q, want the current server state 5", state)
	}
	if pages < 3 {
		t.Fatalf("5 changes under a cap of 2 must take at least 3 pages, took %d", pages)
	}
}

// TestDefaultMaxChangesExceedsMaxObjectsInSet locks the invariant behind the
// default: a single page of change ids must not exceed what one following
// Foo/get (bounded by maxObjectsInSet) could ask for at once.
func TestDefaultMaxChangesExceedsMaxObjectsInSet(t *testing.T) {
	if int64(tuning.DefaultMaxChanges) <= DefaultCoreCapabilities().MaxObjectsInSet {
		t.Fatalf("tuning.DefaultMaxChanges %d must exceed maxObjectsInSet %d",
			tuning.DefaultMaxChanges, DefaultCoreCapabilities().MaxObjectsInSet)
	}
}
