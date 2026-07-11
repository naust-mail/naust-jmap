package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// The first example of RFC 8620 section 3.7: Foo/get takes its ids from
// the /created list of a prior Foo/changes response.
func TestBackrefSpecExampleFooChanges(t *testing.T) {
	p := NewProcessor()
	p.Register("Foo/changes", jmap.CoreCapability, func(_ context.Context, call *Call) []jmap.Invocation {
		return []jmap.Invocation{{Name: "Foo/changes", Args: json.RawMessage(`{
			"accountId": "A1",
			"oldState": "abcdef",
			"newState": "123456",
			"hasMoreChanges": false,
			"created": [ "f1", "f4" ],
			"updated": [],
			"destroyed": []
		}`), CallID: call.CallID}}
	})
	var gotIds []jmap.Id
	p.Register("Foo/get", jmap.CoreCapability, func(_ context.Context, call *Call) []jmap.Invocation {
		var args struct {
			AccountID jmap.Id   `json:"accountId"`
			Ids       []jmap.Id `json:"ids"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			t.Fatalf("Foo/get args: %v", err)
		}
		gotIds = args.Ids
		return []jmap.Invocation{{Name: "Foo/get", Args: json.RawMessage(`{}`), CallID: call.CallID}}
	})

	req, err := jmap.ParseRequest([]byte(`{
		"using": ["urn:ietf:params:jmap:core"],
		"methodCalls": [
			[ "Foo/changes", { "accountId": "A1", "sinceState": "abcdef" }, "t0" ],
			[ "Foo/get", {
				"accountId": "A1",
				"#ids": { "resultOf": "t0", "name": "Foo/changes", "path": "/created" }
			}, "t1" ]
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	resp := p.Process(context.Background(), req, nil, "s0")
	if len(resp.MethodResponses) != 2 {
		t.Fatalf("got %d responses: %+v", len(resp.MethodResponses), resp.MethodResponses)
	}
	if len(gotIds) != 2 || gotIds[0] != "f1" || gotIds[1] != "f4" {
		t.Errorf("resolved ids = %v, want [f1 f4]", gotIds)
	}
}

// The second example of section 3.7: /list/*/emailIds maps through the
// array and flattens the per-item arrays into one.
func TestEvalPointerStarFlattening(t *testing.T) {
	threadGet := json.RawMessage(`{
		"accountId": "A1",
		"state": "123456",
		"list": [
			{ "id": "trd194", "emailIds": [ "msg1020", "msg1021", "msg1023" ] },
			{ "id": "trd114", "emailIds": [ "msg201", "msg223" ] }
		],
		"notFound": []
	}`)
	got, err := evalPointer(threadGet, "/list/*/emailIds")
	if err != nil {
		t.Fatal(err)
	}
	want := `["msg1020","msg1021","msg1023","msg201","msg223"]`
	if string(got) != want {
		t.Errorf("got %s want %s", got, want)
	}

	// Non-flattening star: /list/*/id maps scalars.
	got, err = evalPointer(threadGet, "/list/*/id")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `["trd194","trd114"]` {
		t.Errorf("got %s", got)
	}
}

func TestEvalPointerEscapesAndIndexes(t *testing.T) {
	doc := json.RawMessage(`{"a/b": {"m~n": [10, 20, 30]}}`)
	got, err := evalPointer(doc, "/a~1b/m~0n/1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "20" {
		t.Errorf("got %s want 20", got)
	}
	for _, path := range []string{"/missing", "/a~1b/m~0n/9", "/a~1b/m~0n/x", "no-slash", "/a~1b/m~0n/1/deeper"} {
		if _, err := evalPointer(doc, path); err == nil {
			t.Errorf("evalPointer(%q) succeeded, want error", path)
		}
	}
}

func TestBackrefErrors(t *testing.T) {
	p := NewProcessor()
	run := func(callsJSON string) *jmap.Response {
		req, err := jmap.ParseRequest([]byte(`{"using": ["urn:ietf:params:jmap:core"], "methodCalls": ` + callsJSON + `}`))
		if err != nil {
			t.Fatal(err)
		}
		return p.Process(context.Background(), req, nil, "s0")
	}
	errType := func(resp *jmap.Response, i int) string {
		if resp.MethodResponses[i].Name != "error" {
			t.Fatalf("response %d is %q, want error", i, resp.MethodResponses[i].Name)
		}
		var e jmap.MethodError
		if err := json.Unmarshal(resp.MethodResponses[i].Args, &e); err != nil {
			t.Fatal(err)
		}
		return e.Type
	}

	// Unresolvable reference (no such call id).
	resp := run(`[[ "Core/echo", {"#x": {"resultOf": "nope", "name": "Core/echo", "path": "/x"}}, "c1" ]]`)
	if got := errType(resp, 0); got != jmap.ErrInvalidResultReference {
		t.Errorf("unresolvable ref -> %s", got)
	}
	// Response name mismatch.
	resp = run(`[
		[ "Core/echo", {"x": 1}, "c1" ],
		[ "Core/echo", {"#x": {"resultOf": "c1", "name": "Wrong/name", "path": "/x"}}, "c2" ]
	]`)
	if got := errType(resp, 1); got != jmap.ErrInvalidResultReference {
		t.Errorf("name mismatch -> %s", got)
	}
	// Both foo and #foo present.
	resp = run(`[
		[ "Core/echo", {"x": 1}, "c1" ],
		[ "Core/echo", {"x": 2, "#x": {"resultOf": "c1", "name": "Core/echo", "path": "/x"}}, "c2" ]
	]`)
	if got := errType(resp, 1); got != jmap.ErrInvalidArguments {
		t.Errorf("both forms -> %s", got)
	}
	// A failed method must not stop later calls (3.6.2).
	resp = run(`[
		[ "No/such", {}, "c1" ],
		[ "Core/echo", {"ok": true}, "c2" ]
	]`)
	if got := errType(resp, 0); got != jmap.ErrUnknownMethod {
		t.Errorf("unknown method -> %s", got)
	}
	if resp.MethodResponses[1].Name != "Core/echo" {
		t.Errorf("processing stopped after method error")
	}
}
