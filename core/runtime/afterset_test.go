package runtime

// The AfterSet continuation hook (SetHooks.AfterSet): the seam for a
// spec's onSuccess /set arguments, where a committed /set triggers an
// implicit follow-up answering under the same method call id (RFC 8621
// section 7.5). The toy semantics here: an extra "followUp" argument makes
// the hook append one invocation summarizing the outcome.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// afterSetExt wires an AfterSet-only hook (no Validate/Destroy: the hook
// alone must satisfy the extra-args registration rule) that records the
// outcome it saw and, when the followUp argument is present, appends a
// summary invocation under the call's id.
func afterSetExt(saw **SetOutcome) *Extensions {
	return &Extensions{
		ExtraArgs: map[string]MethodArgs{
			"set": {Names: []string{"followUp"}},
		},
		Set: &SetHooks{
			AfterSet: func(_ context.Context, call *Call, outcome *SetOutcome, extra map[string]json.RawMessage) []jmap.Invocation {
				*saw = outcome
				if _, has := extra["followUp"]; !has {
					return nil
				}
				summary := map[string]any{
					"created":   len(outcome.Created),
					"updated":   len(outcome.Updated),
					"destroyed": len(outcome.Destroyed),
				}
				return reply("Gadget/followUp", call.CallID, summary)
			},
		},
	}
}

func TestAfterSet(t *testing.T) {
	var saw *SetOutcome
	ts := gadgetServer(t, afterSetExt(&saw))

	// Seed two records; no followUp argument, so the hook runs (outcome
	// still observed) but appends nothing.
	r := callGadget(t, ts, inv("Gadget/set",
		`{"accountId":"Atest1","create":{"a":{"subject":"one"},"b":{"subject":"two"}}}`, "0"))
	if len(r.MethodResponses) != 1 {
		t.Fatalf("hook without followUp appended: %v", r.MethodResponses)
	}
	created := methodArgs(t, r, 0, "Gadget/set")["created"].(map[string]any)
	idA := created["a"].(map[string]any)["id"].(string)
	idB := created["b"].(map[string]any)["id"].(string)
	if saw == nil || len(saw.Created) != 2 || string(saw.Created["a"]) != idA {
		t.Fatalf("outcome of seed call: %+v", saw)
	}

	// One call doing all three verbs plus a failing update: the appended
	// invocation answers under the same call id, after the /set response,
	// and the outcome holds only the successes - including the destroyed
	// record's last value.
	saw = nil
	r = callGadget(t, ts, inv("Gadget/set", fmt.Sprintf(
		`{"accountId":"Atest1","followUp":true,"create":{"c":{"subject":"three"}},"update":{%q:{"subject":"edited"},"missing":{"subject":"x"}},"destroy":[%q]}`,
		idA, idB), "7"))
	if len(r.MethodResponses) != 2 {
		t.Fatalf("want set response + follow-up, got %v", r.MethodResponses)
	}
	if r.MethodResponses[1].Name != "Gadget/followUp" || r.MethodResponses[1].CallID != "7" {
		t.Fatalf("follow-up = %v", r.MethodResponses[1])
	}
	sum := methodArgs(t, r, 1, "Gadget/followUp")
	if sum["created"] != float64(1) || sum["updated"] != float64(1) || sum["destroyed"] != float64(1) {
		t.Fatalf("summary = %v", sum)
	}
	if len(saw.Created) != 1 || saw.Created["c"] == "" {
		t.Fatalf("outcome created = %v", saw.Created)
	}
	if len(saw.Updated) != 1 || saw.Updated[0] != jmap.Id(idA) {
		t.Fatalf("outcome updated = %v", saw.Updated)
	}
	oldB, has := saw.Destroyed[jmap.Id(idB)]
	if !has || string(oldB["subject"]) != `"two"` {
		t.Fatalf("outcome destroyed = %v", saw.Destroyed)
	}
}

// TestAfterSetNotOnFailure: a /set that fails as a whole (stateMismatch)
// never reaches the hook.
func TestAfterSetNotOnFailure(t *testing.T) {
	var saw *SetOutcome
	ts := gadgetServer(t, afterSetExt(&saw))

	r := callGadget(t, ts, inv("Gadget/set",
		`{"accountId":"Atest1","ifInState":"bogus","followUp":true,"create":{"a":{"subject":"x"}}}`, "0"))
	if args := methodArgs(t, r, 0, "error"); args["type"] != string(jmap.ErrStateMismatch) {
		t.Fatalf("want stateMismatch, got %v", args)
	}
	if saw != nil {
		t.Fatalf("hook ran on a failed /set: %+v", saw)
	}
}

// TestUpdateSideEffectEcho: a Validate hook change beyond the client's
// patch appears in the updated echo (RFC 8620 section 5.3: "an object
// containing any property that changed as a side effect"), with internal
// properties excluded; an untouched update still echoes null.
func TestUpdateSideEffectEcho(t *testing.T) {
	echoType := &descriptor.Type{
		Name:       "Gadget",
		Capability: "urn:example:gadget",
		Properties: map[string]descriptor.Property{
			"subject": {Kind: descriptor.KindString},
			"body":    {Kind: descriptor.KindString, Default: json.RawMessage(`""`)},
			"hidden":  {Kind: descriptor.KindString, Internal: true},
		},
	}
	ext := &Extensions{
		Set: &SetHooks{
			Validate: func(_ *objectdb.Update, old, new objectdb.Object, _ map[string]json.RawMessage) (*jmap.SetError, error) {
				if old == nil {
					return nil, nil
				}
				// Canonicalize subject to upper case and stage an internal
				// side effect; a patch that already sends the canonical
				// value produces no visible diff.
				var s string
				json.Unmarshal(new["subject"], &s)
				if up := strings.ToUpper(s); up != s {
					new["subject"], _ = json.Marshal(up)
				}
				new["hidden"] = json.RawMessage(`"stamp"`)
				return nil, nil
			},
		},
	}
	ts := gadgetServerType(t, echoType, ext)

	r := callGadget(t, ts, inv("Gadget/set",
		`{"accountId":"Atest1","create":{"a":{"subject":"HI"}}}`, "0"))
	id := methodArgs(t, r, 0, "Gadget/set")["created"].(map[string]any)["a"].(map[string]any)["id"].(string)

	// Canonicalization: the server-side change is echoed, the internal
	// stamp is not.
	r = callGadget(t, ts, inv("Gadget/set", fmt.Sprintf(
		`{"accountId":"Atest1","update":{%q:{"subject":"quiet"}}}`, id), "0"))
	upd := methodArgs(t, r, 0, "Gadget/set")["updated"].(map[string]any)
	side, ok := upd[id].(map[string]any)
	if !ok || side["subject"] != "QUIET" || len(side) != 1 {
		t.Fatalf("side-effect echo = %v", upd[id])
	}

	// Already canonical: nothing beyond the patch changed, so the echo is
	// null.
	r = callGadget(t, ts, inv("Gadget/set", fmt.Sprintf(
		`{"accountId":"Atest1","update":{%q:{"subject":"LOUD"}}}`, id), "0"))
	upd = methodArgs(t, r, 0, "Gadget/set")["updated"].(map[string]any)
	if v, has := upd[id]; !has || v != nil {
		t.Fatalf("no-side-effect echo = %v (present %v)", v, has)
	}
}

// TestSortRejectsInternal: internal properties are not sortable, matching
// the filter language (they are invisible to the method layer).
func TestSortRejectsInternal(t *testing.T) {
	typ := &descriptor.Type{
		Name:       "Gadget",
		Capability: "urn:example:gadget",
		Properties: map[string]descriptor.Property{
			"subject": {Kind: descriptor.KindString},
			"hidden":  {Kind: descriptor.KindString, Internal: true},
		},
	}
	_, errType, _ := parseComparators(typ, []json.RawMessage{
		json.RawMessage(`{"property":"hidden"}`),
	})
	if errType != jmap.ErrUnsupportedSort {
		t.Fatalf("sort on internal property: %q, want unsupportedSort", errType)
	}
}
