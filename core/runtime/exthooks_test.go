package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// gadgetSetHooks rejects any subject containing "forbidden" on create
// and update, and rejects destroying a record whose subject is "keep"
// unless the extra /set argument force is true - the same shapes
// Mailbox/set needs (semantic validation, destroy preconditions,
// onDestroyRemoveEmails).
func gadgetSetHooks(sawOld *[]string) *SetHooks {
	return &SetHooks{
		Validate: func(u *objectdb.Update, old, new objectdb.Object, extra map[string]json.RawMessage) (*jmap.SetError, error) {
			if old != nil && sawOld != nil {
				var s string
				json.Unmarshal(old["subject"], &s)
				*sawOld = append(*sawOld, s)
			}
			var subject string
			json.Unmarshal(new["subject"], &subject)
			if strings.Contains(subject, "forbidden") {
				return &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: []string{"subject"}}, nil
			}
			return nil, nil
		},
		Destroy: func(u *objectdb.Update, id jmap.Id, extra map[string]json.RawMessage) (*jmap.SetError, error) {
			obj, err := u.Get("Gadget", id)
			if err != nil {
				return nil, err
			}
			if string(obj["subject"]) == `"keep"` && string(extra["force"]) != "true" {
				return &jmap.SetError{Type: "gadgetKept"}, nil
			}
			return nil, nil
		},
	}
}

func TestExtSetHooks(t *testing.T) {
	var sawOld []string
	ts := gadgetServer(t, &Extensions{
		ExtraArgs: map[string]MethodArgs{"set": {Names: []string{"force"}}},
		Set:       gadgetSetHooks(&sawOld),
	})

	// One call, one good and one bad create: per-record rejection, the
	// rest of the call proceeds (5.3).
	r := callGadget(t, ts, inv("Gadget/set",
		`{"accountId":"Atest1","create":{
			"a":{"subject":"fine"},
			"b":{"subject":"forbidden fruit"}}}`, "0"))
	args := methodArgs(t, r, 0, "Gadget/set")
	created := args["created"].(map[string]any)
	if _, ok := created["a"]; !ok {
		t.Fatalf("good create rejected: %v", args)
	}
	idA := created["a"].(map[string]any)["id"].(string)
	notCreated := args["notCreated"].(map[string]any)
	serr := notCreated["b"].(map[string]any)
	if serr["type"] != "invalidProperties" || serr["properties"].([]any)[0] != "subject" {
		t.Fatalf("notCreated: %v", args)
	}

	// Update hook: sees the pre-image, rejects the same way.
	r = callGadget(t, ts, inv("Gadget/set", fmt.Sprintf(
		`{"accountId":"Atest1","update":{%q:{"subject":"forbidden too"}}}`, idA), "0"))
	args = methodArgs(t, r, 0, "Gadget/set")
	if args["notUpdated"].(map[string]any)[idA].(map[string]any)["type"] != "invalidProperties" {
		t.Fatalf("notUpdated: %v", args)
	}
	if len(sawOld) != 1 || sawOld[0] != "fine" {
		t.Fatalf("update hook pre-image: %v", sawOld)
	}

	// Destroy hook: type-defined SetError without the extra argument,
	// allowed with it.
	idKeep := createGadget(t, ts, `{"subject":"keep"}`)
	r = callGadget(t, ts, inv("Gadget/set", fmt.Sprintf(
		`{"accountId":"Atest1","destroy":[%q]}`, idKeep), "0"))
	args = methodArgs(t, r, 0, "Gadget/set")
	if args["notDestroyed"].(map[string]any)[idKeep].(map[string]any)["type"] != "gadgetKept" {
		t.Fatalf("notDestroyed: %v", args)
	}
	r = callGadget(t, ts, inv("Gadget/set", fmt.Sprintf(
		`{"accountId":"Atest1","destroy":[%q],"force":true}`, idKeep), "0"))
	args = methodArgs(t, r, 0, "Gadget/set")
	if destroyed, _ := args["destroyed"].([]any); len(destroyed) != 1 || destroyed[0] != idKeep {
		t.Fatalf("forced destroy: %v", args)
	}

	// A missing record is plain notFound; the hook does not turn it
	// into anything else.
	r = callGadget(t, ts, inv("Gadget/set",
		`{"accountId":"Atest1","destroy":["Znope"]}`, "0"))
	args = methodArgs(t, r, 0, "Gadget/set")
	if args["notDestroyed"].(map[string]any)["Znope"].(map[string]any)["type"] != "notFound" {
		t.Fatalf("missing record: %v", args)
	}
}

// gadgetFilter gives Gadget a type-defined filter language: "subject"
// exact, "text" substring on subject - the Mailbox FilterCondition
// shape (custom semantics per condition property).
type gadgetFilter struct{}

func (gadgetFilter) ValidateCondition(name string, value json.RawMessage) error {
	switch name {
	case "subject", "text":
		var s string
		if err := json.Unmarshal(value, &s); err != nil {
			return fmt.Errorf("%s must be a string", name)
		}
		return nil
	}
	return UnsupportedFilterError{Description: fmt.Sprintf("cannot filter on %q", name)}
}

func (gadgetFilter) MatchCondition(_ context.Context, _ jmap.Id, obj objectdb.Object, name string, value json.RawMessage) (bool, error) {
	var want, got string
	json.Unmarshal(value, &want)
	json.Unmarshal(obj["subject"], &got)
	switch name {
	case "subject":
		return got == want, nil
	case "text":
		return strings.Contains(got, want), nil
	}
	return false, nil
}

func TestExtQueryHooks(t *testing.T) {
	var sawExtra map[string]json.RawMessage
	ts := gadgetServer(t, &Extensions{
		ExtraArgs: map[string]MethodArgs{"query": {Names: []string{"reverse"}}},
		Query: &QueryHooks{
			Filter: gadgetFilter{},
			Arrange: func(_ context.Context, _ jmap.Id, matched []QueryRecord, compare func(a, b objectdb.Object) int, extra map[string]json.RawMessage) ([]jmap.Id, error) {
				sawExtra = extra
				ids := make([]jmap.Id, len(matched))
				for i, m := range matched {
					ids[i] = m.Id
				}
				if string(extra["reverse"]) == "true" {
					for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
						ids[i], ids[j] = ids[j], ids[i]
					}
				}
				return ids, nil
			},
		},
	})
	idApple := createGadget(t, ts, `{"subject":"apple pie"}`)
	idBanana := createGadget(t, ts, `{"subject":"banana"}`)
	createGadget(t, ts, `{"subject":"cherry"}`)

	// The type's substring condition, standard sort, no reverse.
	r := callGadget(t, ts, inv("Gadget/query",
		`{"accountId":"Atest1","filter":{"text":"an"},"sort":[{"property":"subject"}]}`, "0"))
	args := methodArgs(t, r, 0, "Gadget/query")
	ids := args["ids"].([]any)
	if len(ids) != 1 || ids[0] != idBanana {
		t.Fatalf("substring filter: %v", args)
	}

	// AND of type conditions still composes structurally.
	r = callGadget(t, ts, inv("Gadget/query",
		`{"accountId":"Atest1","filter":{"operator":"AND","conditions":[{"text":"apple"},{"subject":"apple pie"}]}}`, "0"))
	args = methodArgs(t, r, 0, "Gadget/query")
	if ids := args["ids"].([]any); len(ids) != 1 || ids[0] != idApple {
		t.Fatalf("AND of custom conditions: %v", args)
	}

	// Unknown condition name is the type's unsupportedFilter, a bad
	// value invalidArguments.
	r = callGadget(t, ts, inv("Gadget/query",
		`{"accountId":"Atest1","filter":{"color":"red"}}`, "0"))
	if methodArgs(t, r, 0, "error")["type"] != "unsupportedFilter" {
		t.Fatal("unknown condition was not unsupportedFilter")
	}
	r = callGadget(t, ts, inv("Gadget/query",
		`{"accountId":"Atest1","filter":{"text":7}}`, "0"))
	if methodArgs(t, r, 0, "error")["type"] != "invalidArguments" {
		t.Fatal("bad condition value was not invalidArguments")
	}

	// Arrange sees the extra argument and reorders the full result
	// before windowing.
	r = callGadget(t, ts, inv("Gadget/query",
		`{"accountId":"Atest1","sort":[{"property":"subject"}],"reverse":true,"limit":1}`, "0"))
	args = methodArgs(t, r, 0, "Gadget/query")
	ids = args["ids"].([]any)
	if len(ids) != 1 {
		t.Fatalf("reversed window: %v", args)
	}
	// Reverse of subject order (apple, banana, cherry) starts at
	// cherry; the window of 1 must be cherry's id, not apple's.
	if ids[0] == idApple {
		t.Fatalf("Arrange did not run before windowing: %v", args)
	}
	if string(sawExtra["reverse"]) != "true" {
		t.Fatalf("Arrange extra: %v", sawExtra)
	}

	// queryChanges validates the type filter language too.
	r = callGadget(t, ts, inv("Gadget/queryChanges",
		`{"accountId":"Atest1","sinceQueryState":"x","filter":{"color":"red"}}`, "0"))
	if methodArgs(t, r, 0, "error")["type"] != "unsupportedFilter" {
		t.Fatal("queryChanges unknown condition was not unsupportedFilter")
	}
}

// TestExtSetQueryRegistration: extras for /set and /query demand their
// consuming hooks, mirroring the get/changes rules.
func TestExtSetQueryRegistration(t *testing.T) {
	cases := []struct {
		name string
		ext  *Extensions
	}{
		{"set extras without Set hook", &Extensions{
			ExtraArgs: map[string]MethodArgs{"set": {Names: []string{"force"}}}}},
		{"set extras with empty Set hook", &Extensions{
			ExtraArgs: map[string]MethodArgs{"set": {Names: []string{"force"}}},
			Set:       &SetHooks{}}},
		{"query extras without Query hook", &Extensions{
			ExtraArgs: map[string]MethodArgs{"query": {Names: []string{"deep"}}}}},
		{"query extras without Arrange", &Extensions{
			ExtraArgs: map[string]MethodArgs{"query": {Names: []string{"deep"}}},
			Query:     &QueryHooks{Filter: gadgetFilter{}}}},
	}
	for _, tc := range cases {
		be := memory.New()
		db := objectdb.New(be, lease.NewInProcess(be))
		if err := RegisterStandardTypeExt(NewProcessor(), db, gadgetType(), DefaultCoreCapabilities(), tc.ext); err == nil {
			t.Errorf("%s: registration succeeded, want error", tc.name)
		}
	}
}
