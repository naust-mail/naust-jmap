package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// The Visible hook hides records per caller: /get answers notFound
// (RFC 8620 section 5.1 - "does not exist, or the user does not have
// permission"), /query omits them, /changes stays unfiltered (section
// 5.2 reports ids only), and no query over the type is change-calculable
// (section 5.6).

// hideSecret hides every record whose subject is "secret".
func hideSecret(_ context.Context, _ jmap.Id, obj objectdb.Object) bool {
	return string(obj["subject"]) != `"secret"`
}

func TestVisibleHidesFromGet(t *testing.T) {
	ts := gadgetServer(t, &Extensions{Visible: hideSecret})
	shown := createGadget(t, ts, `{"subject":"plain"}`)
	hidden := createGadget(t, ts, `{"subject":"secret"}`)

	r := callGadget(t, ts, inv("Gadget/get",
		`{"accountId":"Atest1","ids":["`+shown+`","`+hidden+`"],"properties":["subject"]}`, "0"))
	args := methodArgs(t, r, 0, "Gadget/get")
	list, _ := args["list"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != shown {
		t.Errorf("list = %v, want only %s", list, shown)
	}
	notFound, _ := args["notFound"].([]any)
	if len(notFound) != 1 || notFound[0] != hidden {
		t.Errorf("notFound = %v, want [%s]", notFound, hidden)
	}
	// The widened load must not leak properties past the requested list.
	if got := list[0].(map[string]any); len(got) != 2 || got["subject"] != "plain" {
		t.Errorf("visible record = %v, want id+subject only", got)
	}
}

func TestVisibleHidesFromQuery(t *testing.T) {
	ts := gadgetServer(t, &Extensions{Visible: hideSecret})
	shown := createGadget(t, ts, `{"subject":"plain"}`)
	createGadget(t, ts, `{"subject":"secret"}`)

	r := callGadget(t, ts, inv("Gadget/query", `{"accountId":"Atest1"}`, "0"))
	args := methodArgs(t, r, 0, "Gadget/query")
	ids, _ := args["ids"].([]any)
	if len(ids) != 1 || ids[0] != shown {
		t.Errorf("ids = %v, want only %s", ids, shown)
	}
	if args["canCalculateChanges"] != false {
		t.Errorf("canCalculateChanges = %v, want false", args["canCalculateChanges"])
	}
}

// An exact indexed-equality filter with no sort is the one evaluation
// that can answer from the index without loading records; a Visible
// hook must force the loads so hidden records still drop out.
func TestVisibleBypassesExactIndexAnswer(t *testing.T) {
	ts := gadgetServer(t, &Extensions{Visible: hideSecret})
	createGadget(t, ts, `{"subject":"secret"}`)

	r := callGadget(t, ts, inv("Gadget/query",
		`{"accountId":"Atest1","filter":{"subject":"secret"}}`, "0"))
	args := methodArgs(t, r, 0, "Gadget/query")
	ids, _ := args["ids"].([]any)
	if len(ids) != 0 {
		t.Errorf("ids = %v, want empty", ids)
	}
}

func TestVisibleChangesUnfiltered(t *testing.T) {
	ts := gadgetServer(t, &Extensions{Visible: hideSecret})
	r := callGadget(t, ts, inv("Gadget/get", `{"accountId":"Atest1","ids":[]}`, "0"))
	since := methodArgs(t, r, 0, "Gadget/get")["state"].(string)

	hidden := createGadget(t, ts, `{"subject":"secret"}`)

	r = callGadget(t, ts, inv("Gadget/changes",
		`{"accountId":"Atest1","sinceState":"`+since+`"}`, "0"))
	args := methodArgs(t, r, 0, "Gadget/changes")
	created, _ := args["created"].([]any)
	if len(created) != 1 || created[0] != hidden {
		t.Errorf("created = %v, want [%s]", created, hidden)
	}
}

func TestVisibleQueryChangesRefuses(t *testing.T) {
	ts := gadgetServer(t, &Extensions{Visible: hideSecret})
	r := callGadget(t, ts, inv("Gadget/query", `{"accountId":"Atest1"}`, "0"))
	state := methodArgs(t, r, 0, "Gadget/query")["queryState"].(string)

	r = callGadget(t, ts, inv("Gadget/queryChanges",
		`{"accountId":"Atest1","sinceQueryState":"`+state+`"}`, "0"))
	if r.MethodResponses[0].Name != "error" {
		t.Fatalf("got %+v, want error", r.MethodResponses[0])
	}
	var e jmap.MethodError
	if err := json.Unmarshal(r.MethodResponses[0].Args, &e); err != nil || e.Type != jmap.ErrCannotCalculateChanges {
		t.Errorf("error = %v %v, want %s", e, err, jmap.ErrCannotCalculateChanges)
	}
}
