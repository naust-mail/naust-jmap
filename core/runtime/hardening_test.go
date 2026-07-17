package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// TestApplyPatchNonAdjacentOverlap guards the set-based overlap check:
// "a" is a JSON-Pointer prefix of "a/b", but the two sort non-adjacent
// (separated by "a!"), so a pairwise-adjacent-only check would miss the
// RFC 8620 section 5.3 violation. It must still be invalidPatch.
func TestApplyPatchNonAdjacentOverlap(t *testing.T) {
	typ := testNoteType()
	current := objectdb.Object{"id": json.RawMessage(`"A1"`), "subject": json.RawMessage(`"s"`)}
	patch := map[string]json.RawMessage{
		"a":   json.RawMessage(`1`),
		"a!":  json.RawMessage(`2`),
		"a/b": json.RawMessage(`3`),
	}
	_, serr := applyPatch(typ, current, patch, nil)
	if serr == nil || serr.Type != jmap.SetErrInvalidPatch {
		t.Fatalf("expected invalidPatch for non-adjacent overlapping pointers, got %v", serr)
	}
}

// TestFilterNodeCap: a filter tree with more than tuning.MaxFilterNodes nodes is
// rejected as unsupportedFilter before any evaluation.
func TestFilterNodeCap(t *testing.T) {
	typ := testNoteType()
	var b strings.Builder
	b.WriteString(`{"operator":"AND","conditions":[`)
	for i := 0; i <= tuning.MaxFilterNodes; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"subject":"x"}`)
	}
	b.WriteString(`]}`)

	_, errType, _ := parseFilter(typ, nil, json.RawMessage(b.String()))
	if errType != jmap.ErrUnsupportedFilter {
		t.Fatalf("expected unsupportedFilter for oversized filter, got %q", errType)
	}

	// A modest filter is unaffected.
	if _, errType, _ := parseFilter(typ, nil, json.RawMessage(`{"subject":"x"}`)); errType != "" {
		t.Fatalf("modest filter rejected: %q", errType)
	}
}

// TestRequestedPropertiesCap: a Foo/get naming more than
// tuning.MaxRequestedProperties properties is invalidArguments.
func TestRequestedPropertiesCap(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	var b strings.Builder
	b.WriteString(`{"accountId":"Atest1","ids":["x"],"properties":[`)
	for i := 0; i <= tuning.MaxRequestedProperties; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"subject"`)
	}
	b.WriteString(`]}`)
	r := callAPI(t, ts, inv("TestNote/get", b.String(), "0"))
	if r.MethodResponses[0].Name != "error" || methodArgs(t, r, 0, "error")["type"] != jmap.ErrInvalidArguments {
		t.Fatalf("oversized properties should be invalidArguments: %v", r.MethodResponses[0])
	}
}

// TestPanicValueNotLeaked: a panicking method fails alone with a generic
// serverFail; the panic value (which can carry internal state) is never
// returned to the client.
func TestPanicValueNotLeaked(t *testing.T) {
	p := NewProcessor()
	const secret = "SECRET-internal-abc123"
	p.Register("Boom/now", jmap.CoreCapability, func(_ context.Context, _ *Call) []jmap.Invocation {
		panic(secret)
	})
	req, _ := jmap.ParseRequest([]byte(`{
		"using": ["urn:ietf:params:jmap:core"],
		"methodCalls": [["Boom/now", {}, "c1"]]
	}`))
	resp := p.Process(context.Background(), req, nil, "s")
	if bytes.Contains(resp.MethodResponses[0].Args, []byte(secret)) {
		t.Fatalf("panic value leaked to client: %s", resp.MethodResponses[0].Args)
	}
	var e jmap.MethodError
	if err := json.Unmarshal(resp.MethodResponses[0].Args, &e); err != nil {
		t.Fatal(err)
	}
	if e.Type != jmap.ErrServerFail || e.Description != "internal error" {
		t.Fatalf("want generic serverFail, got %+v", e)
	}
}

// TestInternalPropertyHidden covers the descriptor.Internal primitive: an
// internal property (a runtime-maintained index, like Email.threadKeys) is
// stored and indexed like any other, but invisible to the protocol - never
// returned or requestable in /get, and not settable in /set.
func TestInternalPropertyHidden(t *testing.T) {
	typ := &descriptor.Type{
		Name:       "TestInternal",
		Capability: "urn:example:testinternal",
		Properties: map[string]descriptor.Property{
			"subject": {Kind: descriptor.KindString},
			"secret":  {Kind: descriptor.KindArray, ServerSet: true, Immutable: true, SetIndexed: true, Internal: true},
		},
	}
	a := auth.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	core := DefaultCoreCapabilities()
	if err := RegisterStandardType(p, db, typ, core); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability("urn:example:testinternal", struct{}{}, struct{}{}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// A datatype maintains the internal index directly (as insertEmail does
	// for threadKeys), so a stored record really does carry the value.
	var id jmap.Id
	if _, err := db.Update(context.Background(), "Atest1", func(u *objectdb.Update) error {
		created, e := u.Create("TestInternal", objectdb.Object{
			"subject": json.RawMessage(`"hi"`),
			"secret":  json.RawMessage(`["k1","k2"]`),
		})
		id = created
		return e
	}); err != nil {
		t.Fatal(err)
	}

	call := func(name, args string) *jmap.Response {
		body, _ := json.Marshal(map[string]any{
			"using":       []string{jmap.CoreCapability, "urn:example:testinternal"},
			"methodCalls": []jmap.Invocation{inv(name, args, "0")},
		})
		resp := post(t, ts, string(body), "application/json")
		defer resp.Body.Close()
		var out jmap.Response
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return &out
	}

	// All-properties get (the type has no default list) must omit it.
	obj := methodArgs(t, call("TestInternal/get", fmt.Sprintf(`{"accountId":"Atest1","ids":[%q]}`, id)), 0, "TestInternal/get")["list"].([]any)[0].(map[string]any)
	if _, has := obj["secret"]; has {
		t.Fatalf("internal property leaked in all-properties get: %v", obj)
	}
	if obj["subject"] != "hi" {
		t.Fatalf("subject missing: %v", obj)
	}

	// Explicitly requesting it is invalidArguments (unknown property).
	r := call("TestInternal/get", fmt.Sprintf(`{"accountId":"Atest1","ids":[%q],"properties":["secret"]}`, id))
	if r.MethodResponses[0].Name != "error" || methodArgs(t, r, 0, "error")["type"] != jmap.ErrInvalidArguments {
		t.Fatalf("requesting internal property should be invalidArguments: %v", r.MethodResponses[0])
	}

	// Creating with it is invalidProperties.
	set := methodArgs(t, call("TestInternal/set", `{"accountId":"Atest1","create":{"c":{"subject":"x","secret":["z"]}}}`), 0, "TestInternal/set")
	nc, ok := set["notCreated"].(map[string]any)
	if !ok {
		t.Fatalf("create with internal property should fail: %v", set)
	}
	if se := nc["c"].(map[string]any); se["type"] != jmap.SetErrInvalidProperties {
		t.Fatalf("want invalidProperties, got %v", se)
	}
}
