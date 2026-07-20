package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/internal/authtest"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// Gadget is the extensions test datatype: stored subject/body plus a
// static computed property ("shadow", echoing subject) and a dynamic
// computed grammar ("tag:*", echoing the name's suffix) - the same
// shapes RFC 8621 needs for Email's body properties and header:*.
func gadgetType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "Gadget",
		Capability: "urn:example:gadget",
		Properties: map[string]descriptor.Property{
			"subject": {Kind: descriptor.KindString, Indexed: true},
			"body":    {Kind: descriptor.KindString, Default: json.RawMessage(`""`)},
		},
	}
}

type gadgetComputed struct {
	failResolve bool
	lastExtra   map[string]json.RawMessage
	lastNames   []string
}

func (c *gadgetComputed) Accepts(name string) bool {
	return name == "shadow" || strings.HasPrefix(name, "tag:")
}

func (c *gadgetComputed) Resolve(_ context.Context, _ jmap.Id, stored objectdb.Object, names []string, extra map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if c.failResolve {
		return nil, errors.New("resolver failure")
	}
	c.lastExtra = extra
	c.lastNames = names
	out := make(map[string]json.RawMessage, len(names))
	for _, name := range names {
		switch {
		case name == "shadow":
			if v, has := stored["subject"]; has {
				out["shadow"] = v
			}
		case strings.HasPrefix(name, "tag:"):
			v, _ := json.Marshal(strings.TrimPrefix(name, "tag:"))
			out[name] = v
		}
	}
	return out, nil
}

func gadgetServer(t *testing.T, ext *Extensions) *httptest.Server {
	t.Helper()
	return gadgetServerType(t, gadgetType(), ext)
}

// gadgetServerType is gadgetServer with a custom descriptor, for tests
// whose semantics need extra properties (an internal one, say).
func gadgetServerType(t *testing.T, typ *descriptor.Type, ext *Extensions) *httptest.Server {
	t.Helper()
	a := authtest.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	if err := RegisterStandardTypeExt(p, db, typ, DefaultCoreCapabilities(), ext); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(a, p, "https://jmap.example.com", DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability("urn:example:gadget", struct{}{}, struct{}{}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func callGadget(t *testing.T, ts *httptest.Server, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	req := map[string]any{
		"using":       []string{jmap.CoreCapability, "urn:example:gadget"},
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

func createGadget(t *testing.T, ts *httptest.Server, props string) string {
	t.Helper()
	r := callGadget(t, ts, inv("Gadget/set",
		fmt.Sprintf(`{"accountId":"Atest1","create":{"c":%s}}`, props), "0"))
	args := methodArgs(t, r, 0, "Gadget/set")
	created, ok := args["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", args)
	}
	return created["c"].(map[string]any)["id"].(string)
}

func TestExtComputedGet(t *testing.T) {
	comp := &gadgetComputed{}
	ts := gadgetServer(t, &Extensions{Computed: comp})
	id := createGadget(t, ts, `{"subject":"hello"}`)

	// Computed names resolve per record; a duplicate name is requested
	// once; stored and computed properties mix freely.
	r := callGadget(t, ts, inv("Gadget/get", fmt.Sprintf(
		`{"accountId":"Atest1","ids":[%q],"properties":["subject","shadow","tag:x","tag:x"]}`, id), "0"))
	args := methodArgs(t, r, 0, "Gadget/get")
	obj := args["list"].([]any)[0].(map[string]any)
	if obj["subject"] != "hello" || obj["shadow"] != "hello" || obj["tag:x"] != "x" {
		t.Fatalf("object: %v", obj)
	}
	if len(comp.lastNames) != 2 {
		t.Fatalf("resolver got names %v, want deduped 2", comp.lastNames)
	}
	if _, has := obj["body"]; has {
		t.Fatalf("unrequested stored property returned: %v", obj)
	}

	// An unknown name is still invalidArguments (5.1): Accepts gates it.
	r = callGadget(t, ts, inv("Gadget/get", fmt.Sprintf(
		`{"accountId":"Atest1","ids":[%q],"properties":["nope"]}`, id), "0"))
	if methodArgs(t, r, 0, "error")["type"] != "invalidArguments" {
		t.Fatal("unknown property was not rejected")
	}

	// properties null without a default override: stored only, no
	// computed properties sneak in.
	r = callGadget(t, ts, inv("Gadget/get", fmt.Sprintf(
		`{"accountId":"Atest1","ids":[%q],"properties":null}`, id), "0"))
	obj = methodArgs(t, r, 0, "Gadget/get")["list"].([]any)[0].(map[string]any)
	if _, has := obj["shadow"]; has {
		t.Fatalf("computed property in a properties:null response: %v", obj)
	}
	if obj["subject"] != "hello" || obj["body"] != "" {
		t.Fatalf("stored properties missing: %v", obj)
	}
}

func TestExtDefaultGetProperties(t *testing.T) {
	comp := &gadgetComputed{}
	ts := gadgetServer(t, &Extensions{
		Computed:             comp,
		DefaultGetProperties: []string{"subject", "shadow"},
	})
	id := createGadget(t, ts, `{"subject":"hi","body":"text"}`)

	// Omitted and explicit null both get the default list (RFC 8621
	// section 4.2: "omitted or null").
	for _, args := range []string{
		fmt.Sprintf(`{"accountId":"Atest1","ids":[%q]}`, id),
		fmt.Sprintf(`{"accountId":"Atest1","ids":[%q],"properties":null}`, id),
	} {
		r := callGadget(t, ts, inv("Gadget/get", args, "0"))
		obj := methodArgs(t, r, 0, "Gadget/get")["list"].([]any)[0].(map[string]any)
		if obj["id"] != id || obj["subject"] != "hi" || obj["shadow"] != "hi" {
			t.Fatalf("default properties: %v", obj)
		}
		if _, has := obj["body"]; has {
			t.Fatalf("property outside the default list returned: %v", obj)
		}
	}

	// An explicit list still wins over the default.
	r := callGadget(t, ts, inv("Gadget/get", fmt.Sprintf(
		`{"accountId":"Atest1","ids":[%q],"properties":["body"]}`, id), "0"))
	obj := methodArgs(t, r, 0, "Gadget/get")["list"].([]any)[0].(map[string]any)
	if obj["body"] != "text" {
		t.Fatalf("explicit properties: %v", obj)
	}
	if _, has := obj["shadow"]; has {
		t.Fatalf("default list applied despite explicit properties: %v", obj)
	}
}

func TestExtGetExtraArgs(t *testing.T) {
	comp := &gadgetComputed{}
	ts := gadgetServer(t, &Extensions{
		Computed: comp,
		ExtraArgs: map[string]MethodArgs{
			"get": {
				Names: []string{"verbose"},
				Check: func(extra map[string]json.RawMessage) error {
					if raw, has := extra["verbose"]; has {
						var b bool
						if json.Unmarshal(raw, &b) != nil {
							return errors.New("verbose must be a boolean")
						}
					}
					return nil
				},
			},
		},
	})
	id := createGadget(t, ts, `{"subject":"s"}`)

	// A declared extra argument is accepted and reaches the resolver.
	r := callGadget(t, ts, inv("Gadget/get", fmt.Sprintf(
		`{"accountId":"Atest1","ids":[%q],"properties":["shadow"],"verbose":true}`, id), "0"))
	methodArgs(t, r, 0, "Gadget/get")
	if string(comp.lastExtra["verbose"]) != "true" {
		t.Fatalf("resolver extra: %v", comp.lastExtra)
	}

	// Check failing is invalidArguments.
	r = callGadget(t, ts, inv("Gadget/get", fmt.Sprintf(
		`{"accountId":"Atest1","ids":[%q],"verbose":"nope"}`, id), "0"))
	if methodArgs(t, r, 0, "error")["type"] != "invalidArguments" {
		t.Fatal("failing Check was not invalidArguments")
	}

	// An undeclared argument is still invalidArguments (3.6.2).
	r = callGadget(t, ts, inv("Gadget/get", fmt.Sprintf(
		`{"accountId":"Atest1","ids":[%q],"bogus":1}`, id), "0"))
	if methodArgs(t, r, 0, "error")["type"] != "invalidArguments" {
		t.Fatal("undeclared argument was not rejected")
	}
}

func TestExtResolveErrorIsServerFail(t *testing.T) {
	comp := &gadgetComputed{failResolve: true}
	ts := gadgetServer(t, &Extensions{Computed: comp})
	id := createGadget(t, ts, `{"subject":"s"}`)
	r := callGadget(t, ts, inv("Gadget/get", fmt.Sprintf(
		`{"accountId":"Atest1","ids":[%q],"properties":["shadow"]}`, id), "0"))
	if methodArgs(t, r, 0, "error")["type"] != "serverFail" {
		t.Fatal("resolver error was not serverFail")
	}
}

func TestExtChangesExtraResponse(t *testing.T) {
	var gotView *ChangesView
	var gotExtra map[string]json.RawMessage
	ts := gadgetServer(t, &Extensions{
		ExtraArgs: map[string]MethodArgs{
			"changes": {Names: []string{"scope"}},
		},
		ExtraResponse: &ResponseExtras{
			Changes: func(_ context.Context, _ jmap.Id, view *ChangesView, extra map[string]json.RawMessage) (map[string]json.RawMessage, error) {
				gotView = view
				gotExtra = extra
				return map[string]json.RawMessage{
					"updatedProperties": json.RawMessage(`["totalThings"]`),
				}, nil
			},
		},
	})
	id := createGadget(t, ts, `{"subject":"s"}`)

	r := callGadget(t, ts, inv("Gadget/changes",
		`{"accountId":"Atest1","sinceState":"0","scope":"counts"}`, "0"))
	args := methodArgs(t, r, 0, "Gadget/changes")
	// The extra field is merged next to the intact standard fields.
	got, _ := args["updatedProperties"].([]any)
	if len(got) != 1 || got[0] != "totalThings" {
		t.Fatalf("updatedProperties: %v", args)
	}
	if args["oldState"] != "0" || args["newState"] != "1" {
		t.Fatalf("standard fields: %v", args)
	}
	created := args["created"].([]any)
	if len(created) != 1 || created[0] != id {
		t.Fatalf("created: %v", args)
	}
	// The hook saw the response view and the decoded extra argument.
	if gotView == nil || gotView.NewState != "1" || len(gotView.Created) != 1 {
		t.Fatalf("view: %+v", gotView)
	}
	if string(gotExtra["scope"]) != `"counts"` {
		t.Fatalf("extra: %v", gotExtra)
	}
}

func TestExtChangesExtraFieldCollision(t *testing.T) {
	ts := gadgetServer(t, &Extensions{
		ExtraResponse: &ResponseExtras{
			Changes: func(_ context.Context, _ jmap.Id, _ *ChangesView, _ map[string]json.RawMessage) (map[string]json.RawMessage, error) {
				return map[string]json.RawMessage{"newState": json.RawMessage(`"9"`)}, nil
			},
		},
	})
	createGadget(t, ts, `{"subject":"s"}`)
	r := callGadget(t, ts, inv("Gadget/changes",
		`{"accountId":"Atest1","sinceState":"0"}`, "0"))
	if methodArgs(t, r, 0, "error")["type"] != "serverFail" {
		t.Fatal("colliding extra field was not serverFail")
	}
}

func TestExtRegistrationValidation(t *testing.T) {
	comp := &gadgetComputed{}
	hook := &ResponseExtras{
		Changes: func(_ context.Context, _ jmap.Id, _ *ChangesView, _ map[string]json.RawMessage) (map[string]json.RawMessage, error) {
			return nil, nil
		},
	}
	cases := []struct {
		name string
		ext  *Extensions
	}{
		{"unknown method suffix", &Extensions{Computed: comp,
			ExtraArgs: map[string]MethodArgs{"frobnicate": {Names: []string{"a"}}}}},
		{"unplumbed method", &Extensions{Computed: comp,
			ExtraArgs: map[string]MethodArgs{"copy": {Names: []string{"a"}}}}},
		{"get extras without Computed", &Extensions{
			ExtraArgs: map[string]MethodArgs{"get": {Names: []string{"a"}}}}},
		{"changes extras without hook", &Extensions{
			ExtraArgs: map[string]MethodArgs{"changes": {Names: []string{"a"}}}}},
		{"standard-arg collision", &Extensions{Computed: comp,
			ExtraArgs: map[string]MethodArgs{"get": {Names: []string{"ids"}}}}},
		{"no names", &Extensions{Computed: comp,
			ExtraArgs: map[string]MethodArgs{"get": {}}}},
		{"duplicate name", &Extensions{Computed: comp,
			ExtraArgs: map[string]MethodArgs{"get": {Names: []string{"a", "a"}}}}},
		{"empty name", &Extensions{Computed: comp,
			ExtraArgs: map[string]MethodArgs{"get": {Names: []string{""}}}}},
		{"unknown default property", &Extensions{
			DefaultGetProperties: []string{"nope"}}},
	}
	for _, tc := range cases {
		be := memory.New()
		db := objectdb.New(be, lease.NewInProcess(be))
		if tc.name == "changes extras without hook" {
			tc.ext.ExtraResponse = nil
		} else if tc.ext.ExtraArgs != nil {
			if _, has := tc.ext.ExtraArgs["changes"]; has {
				tc.ext.ExtraResponse = hook
			}
		}
		err := RegisterStandardTypeExt(NewProcessor(), db, gadgetType(), DefaultCoreCapabilities(), tc.ext)
		if err == nil {
			t.Errorf("%s: registration succeeded, want error", tc.name)
		}
	}

	// A well-formed Extensions registers fine.
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	err := RegisterStandardTypeExt(NewProcessor(), db, gadgetType(), DefaultCoreCapabilities(), &Extensions{
		Computed:             comp,
		DefaultGetProperties: []string{"id", "subject", "shadow", "tag:a"},
		ExtraArgs:            map[string]MethodArgs{"get": {Names: []string{"verbose"}}},
		ExtraResponse:        hook,
	})
	if err != nil {
		t.Fatalf("valid extensions rejected: %v", err)
	}
}
