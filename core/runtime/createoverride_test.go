package runtime

// The create-override hooks (SetHooks.PrepareCreate/CommitCreate): a
// type whose creation object is input to a derivation, not the record.
// The toy semantics here: the client sends {"parts": [...]}, an
// undeclared property; prepare joins the parts outside the lease, commit
// stores the joined subject and echoes {id, size}.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// overrideHooks builds the toy override. sawValidate collects the
// subjects Validate saw, proving it runs for updates only.
func overrideHooks(sawValidate *[]string) *SetHooks {
	return &SetHooks{
		Validate: func(_ *objectdb.Update, old, new objectdb.Object, _ map[string]json.RawMessage) (*jmap.SetError, error) {
			var s string
			json.Unmarshal(new["subject"], &s)
			*sawValidate = append(*sawValidate, s)
			return nil, nil
		},
		PrepareCreate: func(_ context.Context, _ *Call, _, _ jmap.Id, raw json.RawMessage) (any, *jmap.SetError, error) {
			var in struct {
				Parts        []string `json:"parts"`
				Reject       bool     `json:"reject"`
				RejectCommit bool     `json:"rejectCommit"`
				Explode      bool     `json:"explode"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return nil, &jmap.SetError{Type: jmap.SetErrInvalidProperties}, nil
			}
			if in.Explode {
				return nil, nil, errors.New("prepare exploded")
			}
			if in.Reject {
				return nil, &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: []string{"parts"}}, nil
			}
			if in.RejectCommit {
				return "REJECT-AT-COMMIT", nil, nil
			}
			return strings.Join(in.Parts, " "), nil, nil
		},
		CommitCreate: func(u *objectdb.Update, prepared any) (jmap.Id, objectdb.Object, *jmap.SetError, error) {
			subject := prepared.(string)
			if subject == "REJECT-AT-COMMIT" {
				return "", nil, &jmap.SetError{Type: jmap.SetErrForbidden}, nil
			}
			id, err := u.Create("Gadget", objectdb.Object{"subject": mustRaw(subject)})
			if err != nil {
				return "", nil, nil, err
			}
			return id, objectdb.Object{
				"id":   mustRaw(string(id)),
				"size": json.RawMessage(fmt.Sprint(len(subject))),
			}, nil, nil
		},
	}
}

func mustRaw(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func TestCreateOverride(t *testing.T) {
	var sawValidate []string
	ts := gadgetServer(t, &Extensions{Set: overrideHooks(&sawValidate)})

	// The creation object's properties are the hook's input: "parts" is
	// not a descriptor property and would be invalidProperties under the
	// runtime's own pipeline, and per-record rejection from either phase
	// leaves the rest of the call running (5.3).
	r := callGadget(t, ts, inv("Gadget/set", `{"accountId":"Atest1","create":{
		"a":{"parts":["hello","world"]},
		"b":{"reject":true},
		"c":{"rejectCommit":true}}}`, "0"))
	args := methodArgs(t, r, 0, "Gadget/set")
	created := args["created"].(map[string]any)
	echo, ok := created["a"].(map[string]any)
	if !ok {
		t.Fatalf("created: %v", args)
	}
	// The echo is exactly what CommitCreate returned.
	if echo["size"] != float64(11) || echo["id"] == "" || len(echo) != 2 {
		t.Fatalf("echo = %v", echo)
	}
	nc := args["notCreated"].(map[string]any)
	if se := nc["b"].(map[string]any); se["type"] != "invalidProperties" {
		t.Fatalf("prepare rejection: %v", se)
	}
	if se := nc["c"].(map[string]any); se["type"] != "forbidden" {
		t.Fatalf("commit rejection: %v", se)
	}
	id := echo["id"].(string)

	// The record commit produced is the derived one.
	r = callGadget(t, ts, inv("Gadget/get",
		fmt.Sprintf(`{"accountId":"Atest1","ids":[%q]}`, id), "0"))
	got := methodArgs(t, r, 0, "Gadget/get")["list"].([]any)[0].(map[string]any)
	if got["subject"] != "hello world" {
		t.Fatalf("stored subject = %v", got["subject"])
	}

	// Validate never saw the creates; it still guards updates. The
	// creation id map is maintained: a later invocation destroys "#d".
	r = callGadget(t, ts,
		inv("Gadget/set", `{"accountId":"Atest1","create":{"d":{"parts":["bye"]}}}`, "0"),
		inv("Gadget/set", fmt.Sprintf(`{"accountId":"Atest1","update":{%q:{"subject":"edited"}}}`, id), "1"),
		inv("Gadget/set", `{"accountId":"Atest1","destroy":["#d"]}`, "2"))
	realD := methodArgs(t, r, 0, "Gadget/set")["created"].(map[string]any)["d"].(map[string]any)["id"].(string)
	if _, ok := methodArgs(t, r, 1, "Gadget/set")["updated"].(map[string]any); !ok {
		t.Fatalf("update failed: %v", r.MethodResponses[1].Args)
	}
	if d := methodArgs(t, r, 2, "Gadget/set")["destroyed"].([]any); len(d) != 1 || d[0] != realD {
		t.Fatalf("destroy by creation id: %v", r.MethodResponses[2].Args)
	}
	if len(sawValidate) != 1 || sawValidate[0] != "edited" {
		t.Fatalf("Validate saw %v, want only the update", sawValidate)
	}
}

// TestCreateOverrideErrors: a hard error from PrepareCreate fails the
// method as serverFail, and ifInState still guards a call whose creates
// were prepared before the commit opened.
func TestCreateOverrideErrors(t *testing.T) {
	var saw []string
	ts := gadgetServer(t, &Extensions{Set: overrideHooks(&saw)})

	r := callGadget(t, ts, inv("Gadget/set",
		`{"accountId":"Atest1","create":{"a":{"explode":true}}}`, "0"))
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("want error, got %v", r.MethodResponses[0])
	}

	r = callGadget(t, ts, inv("Gadget/set",
		`{"accountId":"Atest1","ifInState":"bogus","create":{"a":{"parts":["x"]}}}`, "0"))
	if args := methodArgs(t, r, 0, "error"); args["type"] != string(jmap.ErrStateMismatch) {
		t.Fatalf("want stateMismatch, got %v", args)
	}
}

// TestCreateOverrideRegistration: the hooks come as a pair.
func TestCreateOverrideRegistration(t *testing.T) {
	hooks := &SetHooks{
		PrepareCreate: func(context.Context, *Call, jmap.Id, jmap.Id, json.RawMessage) (any, *jmap.SetError, error) {
			return nil, nil, nil
		},
	}
	err := (&Extensions{Set: hooks}).validate(gadgetType())
	if err == nil || !strings.Contains(err.Error(), "together") {
		t.Fatalf("half-declared override accepted: %v", err)
	}
}
