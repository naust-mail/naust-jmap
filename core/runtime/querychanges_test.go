package runtime

// Foo/queryChanges (RFC 8620 section 5.6) behavior tests. The anchor
// assertion throughout is the section 5.6 client algorithm itself:
// applying removed (splice out) and added (splice in by ascending
// index) to the cached list must reproduce what re-running the query
// returns. removed may legally be a superset, so tests assert
// convergence and the load-bearing members, not exact minimality.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/internal/authtest"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// spliceApply is the section 5.6 client algorithm over a fully cached
// list: splice out every removed id present, then splice in each added
// item at its index, lowest first.
func spliceApply(cached []string, removed, added []any) []string {
	out := make([]string, 0, len(cached))
	drop := make(map[string]bool, len(removed))
	for _, r := range removed {
		drop[r.(string)] = true
	}
	for _, id := range cached {
		if !drop[id] {
			out = append(out, id)
		}
	}
	for _, a := range added {
		item := a.(map[string]any)
		idx := int(item["index"].(float64))
		id := item["id"].(string)
		if idx > len(out) {
			idx = len(out)
		}
		out = append(out[:idx], append([]string{id}, out[idx:]...)...)
	}
	return out
}

func idsOf(args map[string]any) []string {
	raw := args["ids"].([]any)
	out := make([]string, len(raw))
	for i, v := range raw {
		out[i] = v.(string)
	}
	return out
}

func anySlice(args map[string]any, key string) []any {
	if args[key] == nil {
		return nil
	}
	return args[key].([]any)
}

// ---- the RFC 8620 section 5.7 Todo example, as a fixture ----

// todoType is the section 5.7 example type: title, a String[Boolean]
// keywords set, and subTodoIds.
func todoType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "Todo",
		Capability: "urn:example:gadget",
		Properties: map[string]descriptor.Property{
			"title":      {Kind: descriptor.KindString},
			"keywords":   {Kind: descriptor.KindObject, Default: json.RawMessage(`{}`)},
			"subTodoIds": {Kind: descriptor.KindArray},
		},
	}
}

// todoFilter is the section 5.7 "hasKeyword" FilterCondition: a Todo
// matches when its keywords set contains the given keyword.
type todoFilter struct{}

func (todoFilter) ValidateCondition(name string, value json.RawMessage) error {
	if name != "hasKeyword" {
		return UnsupportedFilterError{Description: fmt.Sprintf("cannot filter on %q", name)}
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return fmt.Errorf("hasKeyword must be a string")
	}
	return nil
}

func (todoFilter) MatchCondition(_ context.Context, _ jmap.Id, obj objectdb.Object, name string, value json.RawMessage) (bool, error) {
	var kw string
	json.Unmarshal(value, &kw)
	var keywords map[string]bool
	if raw, has := obj["keywords"]; has {
		json.Unmarshal(raw, &keywords)
	}
	return keywords[kw], nil
}

// lyingFilter is todoFilter behind a false declaration: it reads
// "keywords" while declaring reads of "title" only. The projected load
// built from that declaration hides keywords from it, so its verdicts
// diverge from a full decode - the exact silent-corruption class
// Processor.VerifyQueryProjection exists to catch.
type lyingFilter struct{}

func (lyingFilter) ValidateCondition(name string, value json.RawMessage) error {
	return todoFilter{}.ValidateCondition(name, value)
}

func (lyingFilter) MatchCondition(ctx context.Context, acct jmap.Id, obj objectdb.Object, name string, value json.RawMessage) (bool, error) {
	return todoFilter{}.MatchCondition(ctx, acct, obj, name, value)
}

// TestVerifyQueryProjectionCatchesFalseReads pins the assertion mode's
// one job: a reads declaration omitting a property its hook reads must
// fail the query loudly instead of silently returning wrong results.
func TestVerifyQueryProjectionCatchesFalseReads(t *testing.T) {
	ts := gadgetServerType(t, todoType(), &Extensions{Query: &QueryHooks{
		Filter:          lyingFilter{},
		LocalConditions: map[string][]string{"hasKeyword": {"title"}},
	}})
	callGadget(t, ts, inv("Todo/set",
		`{"accountId":"Atest1","create":{"c":{"title":"Practise Piano","keywords":{"music":true}}}}`, "0"))
	r := callGadget(t, ts, inv("Todo/query",
		`{"accountId":"Atest1","filter":{"hasKeyword":"music"}}`, "0"))
	errArgs := methodArgs(t, r, 0, "error")
	if errArgs["type"] != "serverFail" {
		t.Fatalf("error type = %v, want serverFail", errArgs["type"])
	}
	desc, _ := errArgs["description"].(string)
	if !strings.Contains(desc, "diverged") {
		t.Fatalf("error description %q does not name the divergence", desc)
	}
}

// TestQueryChangesTodoExample walks the section 5.7 example: a query
// for Todos with a "music" or "video" keyword sorted by title, a
// keyword edit on one result, another result destroyed, then
// Todo/queryChanges from the original state. The destroyed Todo must be
// in removed and not in added, exactly as the example answers. The
// keyword edit makes this server also report that Todo in removed and
// re-add it at its position - the section 5.6 MUST for a filter on a
// mutable property ("MUST include all Foos in the current results for
// which this property may have changed"), which the example's terser
// answer elides; the client splice converges identically either way.
func TestQueryChangesTodoExample(t *testing.T) {
	ts := gadgetServerType(t, todoType(), &Extensions{Query: &QueryHooks{
		Filter:          todoFilter{},
		LocalConditions: map[string][]string{"hasKeyword": {"keywords"}},
	}})
	create := func(props string) string {
		t.Helper()
		r := callGadget(t, ts, inv("Todo/set",
			fmt.Sprintf(`{"accountId":"Atest1","create":{"c":%s}}`, props), "0"))
		created, ok := methodArgs(t, r, 0, "Todo/set")["created"].(map[string]any)
		if !ok {
			t.Fatalf("create failed: %v", r)
		}
		return created["c"].(map[string]any)["id"].(string)
	}
	piano := create(`{"title":"Practise Piano","keywords":{"music":true,"beethoven":true,"mozart":true,"liszt":true,"rachmaninov":true}}`)
	daft := create(`{"title":"Watch Daft Punk music video","keywords":{"music":true,"video":true,"trance":true}}`)
	create(`{"title":"Read a book"}`)

	const queryArgs = `"filter":{"operator":"OR","conditions":[{"hasKeyword":"music"},{"hasKeyword":"video"}]},"sort":[{"property":"title"}]`
	query := func() map[string]any {
		t.Helper()
		r := callGadget(t, ts, inv("Todo/query", `{"accountId":"Atest1",`+queryArgs+`}`, "0"))
		return methodArgs(t, r, 0, "Todo/query")
	}
	q0 := query()
	if q0["canCalculateChanges"] != true {
		t.Fatalf("canCalculateChanges = %v, want true (as in the 5.7 example)", q0["canCalculateChanges"])
	}
	cached := idsOf(q0)
	if len(cached) != 2 || cached[0] != piano || cached[1] != daft {
		t.Fatalf("query ids = %v, want [%s %s]", cached, piano, daft)
	}
	state0 := q0["queryState"].(string)

	// The keyword edit: chopin in, mozart out (the whole-object form).
	r := callGadget(t, ts, inv("Todo/set", fmt.Sprintf(
		`{"accountId":"Atest1","update":{%q:{"keywords":{"music":true,"beethoven":true,"chopin":true,"liszt":true,"rachmaninov":true}}}}`, piano), "0"))
	if _, ok := methodArgs(t, r, 0, "Todo/set")["updated"].(map[string]any)[piano]; !ok {
		t.Fatalf("keyword update failed: %v", r)
	}
	// Another user deletes the Daft Punk Todo.
	r = callGadget(t, ts, inv("Todo/set", fmt.Sprintf(
		`{"accountId":"Atest1","destroy":[%q]}`, daft), "0"))
	if destroyed, _ := methodArgs(t, r, 0, "Todo/set")["destroyed"].([]any); len(destroyed) != 1 {
		t.Fatalf("destroy failed: %v", r)
	}

	r = callGadget(t, ts, inv("Todo/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1",`+queryArgs+`,"sinceQueryState":%q,"maxChanges":50}`, state0), "1"))
	qc := methodArgs(t, r, 0, "Todo/queryChanges")
	if qc["oldQueryState"] != state0 || qc["newQueryState"] == state0 {
		t.Fatalf("states: old %v new %v (since %q)", qc["oldQueryState"], qc["newQueryState"], state0)
	}
	removed, added := anySlice(qc, "removed"), anySlice(qc, "added")
	sawDaft := false
	for _, id := range removed {
		if id == daft {
			sawDaft = true
		}
	}
	if !sawDaft {
		t.Fatalf("removed %v lacks the destroyed Todo %s", removed, daft)
	}
	for _, a := range added {
		if a.(map[string]any)["id"] == daft {
			t.Fatalf("destroyed Todo re-added: %v", added)
		}
	}
	if got := spliceApply(cached, removed, added); len(got) != 1 || got[0] != piano {
		t.Fatalf("splice result %v, want [%s]", got, piano)
	}
	if fresh := idsOf(query()); len(fresh) != 1 || fresh[0] != piano {
		t.Fatalf("refetch = %v, want [%s]", fresh, piano)
	}

	// The example continues by creating a sub-Todo with no keywords: a
	// creation outside the query answers an empty diff (tier shape: the
	// changed record does not match, so nothing was added and nothing
	// the client holds could have moved).
	state1 := query()["queryState"].(string)
	create(`{"title":"Warm up with scales"}`)
	r = callGadget(t, ts, inv("Todo/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1",`+queryArgs+`,"sinceQueryState":%q}`, state1), "2"))
	qc = methodArgs(t, r, 0, "Todo/queryChanges")
	if len(anySlice(qc, "removed")) != 0 || len(anySlice(qc, "added")) != 0 {
		t.Fatalf("out-of-query create: removed %v added %v, want empty", qc["removed"], qc["added"])
	}
}

// ---- lifecycle over the derived core language ----

func TestQueryChangesLifecycle(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	const filter = `"filter":{"subject":"target"}`
	query := func() map[string]any {
		t.Helper()
		r := callAPI(t, ts, inv("TestNote/query", `{"accountId":"Atest1",`+filter+`}`, "0"))
		return methodArgs(t, r, 0, "TestNote/query")
	}
	qc := func(state, extra string) map[string]any {
		t.Helper()
		r := callAPI(t, ts, inv("TestNote/queryChanges", fmt.Sprintf(
			`{"accountId":"Atest1",`+filter+`,"sinceQueryState":%q%s}`, state, extra), "0"))
		return methodArgs(t, r, 0, "TestNote/queryChanges")
	}
	n1 := createNote(t, ts, `{"subject":"target"}`)
	createNote(t, ts, `{"subject":"other"}`)

	q0 := query()
	cached := idsOf(q0)
	state0 := q0["queryState"].(string)
	if len(cached) != 1 || cached[0] != n1 {
		t.Fatalf("baseline: %v", cached)
	}

	// A matching creation arrives with its position.
	n3 := createNote(t, ts, `{"subject":"target"}`)
	c := qc(state0, "")
	cached = spliceApply(cached, anySlice(c, "removed"), anySlice(c, "added"))
	if fresh := idsOf(query()); strings.Join(cached, ",") != strings.Join(fresh, ",") {
		t.Fatalf("after create: splice %v vs refetch %v", cached, fresh)
	}
	sawNew := false
	for _, a := range anySlice(c, "added") {
		if a.(map[string]any)["id"] == n3 {
			sawNew = true
		}
	}
	if !sawNew {
		t.Fatalf("created match not in added: %v", c["added"])
	}
	state1 := query()["queryState"].(string)

	// The record leaves the query (tier-1 shape: removed superset, no
	// added, no evaluation observable).
	r := callAPI(t, ts, inv("TestNote/set", fmt.Sprintf(
		`{"accountId":"Atest1","update":{%q:{"subject":"gone"}}}`, n3), "0"))
	if _, ok := methodArgs(t, r, 0, "TestNote/set")["updated"].(map[string]any)[n3]; !ok {
		t.Fatalf("update failed: %v", r)
	}
	c = qc(state1, "")
	if len(anySlice(c, "added")) != 0 {
		t.Fatalf("departure added %v, want none", c["added"])
	}
	cached = spliceApply(cached, anySlice(c, "removed"), anySlice(c, "added"))
	if fresh := idsOf(query()); strings.Join(cached, ",") != strings.Join(fresh, ",") {
		t.Fatalf("after departure: splice %v vs refetch %v", cached, fresh)
	}
	state2 := query()["queryState"].(string)

	// Destroy the remaining match.
	r = callAPI(t, ts, inv("TestNote/set", fmt.Sprintf(
		`{"accountId":"Atest1","destroy":[%q]}`, n1), "0"))
	if destroyed, _ := methodArgs(t, r, 0, "TestNote/set")["destroyed"].([]any); len(destroyed) != 1 {
		t.Fatalf("destroy failed: %v", r)
	}
	c = qc(state2, "")
	cached = spliceApply(cached, anySlice(c, "removed"), anySlice(c, "added"))
	if len(cached) != 0 {
		t.Fatalf("after destroy: %v", cached)
	}

	// calculateTotal: present exactly when requested, and correct.
	c = qc(state0, `,"calculateTotal":true`)
	if c["total"] != float64(0) {
		t.Fatalf("total = %v, want 0", c["total"])
	}
	if _, has := qc(state0, "")["total"]; has {
		t.Fatal("total present without calculateTotal")
	}

	// tooManyChanges: judged against the client's number; zero is a
	// valid maxChanges on this method and any change is then too many,
	// while a caught-up client passes it.
	r = callAPI(t, ts, inv("TestNote/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1",`+filter+`,"sinceQueryState":%q,"maxChanges":0}`, state0), "0"))
	if methodArgs(t, r, 0, "error")["type"] != "tooManyChanges" {
		t.Fatal("expected tooManyChanges for maxChanges 0 with changes")
	}
	current := query()["queryState"].(string)
	r = callAPI(t, ts, inv("TestNote/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1",`+filter+`,"sinceQueryState":%q,"maxChanges":0}`, current), "0"))
	c = methodArgs(t, r, 0, "TestNote/queryChanges")
	if len(anySlice(c, "removed")) != 0 || len(anySlice(c, "added")) != 0 {
		t.Fatalf("caught up with maxChanges 0: %v", c)
	}

	// upToId on a mutable query is ignored (5.6 commands it): a change
	// past the anchor is still reported.
	stateBefore := query()["queryState"].(string)
	late := createNote(t, ts, `{"subject":"target"}`)
	r = callAPI(t, ts, inv("TestNote/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1",`+filter+`,"sinceQueryState":%q,"upToId":%q}`, stateBefore, n1), "0"))
	c = methodArgs(t, r, 0, "TestNote/queryChanges")
	found := false
	for _, a := range anySlice(c, "added") {
		if a.(map[string]any)["id"] == late {
			found = true
		}
	}
	if !found {
		t.Fatalf("mutable query trimmed by upToId: %v", c["added"])
	}

	// From the original state the diff is now two changes (one removed,
	// one added): a maxChanges of 1 is too small, 2 fits exactly.
	r = callAPI(t, ts, inv("TestNote/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1",`+filter+`,"sinceQueryState":%q,"maxChanges":1}`, state0), "0"))
	if methodArgs(t, r, 0, "error")["type"] != "tooManyChanges" {
		t.Fatal("expected tooManyChanges for maxChanges 1 with a two-change diff")
	}
	r = callAPI(t, ts, inv("TestNote/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1",`+filter+`,"sinceQueryState":%q,"maxChanges":2}`, state0), "0"))
	c = methodArgs(t, r, 0, "TestNote/queryChanges")
	if len(anySlice(c, "removed"))+len(anySlice(c, "added")) != 2 {
		t.Fatalf("two-change diff: %v", c)
	}
}

// ---- the immutable-query diet and upToId ----

// taskType has an Immutable, Indexed sort property: with a null filter
// (reads nothing) and a "due" sort, the whole query is proven
// immutable, which unlocks the diet and upToId.
func taskType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "Task",
		Capability: "urn:example:gadget",
		Properties: map[string]descriptor.Property{
			"due":   {Kind: descriptor.KindUnsignedInt, Immutable: true, Indexed: true},
			"title": {Kind: descriptor.KindString},
		},
	}
}

func TestQueryChangesImmutableUpToId(t *testing.T) {
	ts := gadgetServerType(t, taskType(), nil)
	create := func(due int, title string) string {
		t.Helper()
		r := callGadget(t, ts, inv("Task/set", fmt.Sprintf(
			`{"accountId":"Atest1","create":{"c":{"due":%d,"title":%q}}}`, due, title), "0"))
		created, ok := methodArgs(t, r, 0, "Task/set")["created"].(map[string]any)
		if !ok {
			t.Fatalf("create failed: %v", r)
		}
		return created["c"].(map[string]any)["id"].(string)
	}
	t1 := create(10, "one")
	t2 := create(20, "two")
	t3 := create(30, "three")
	t4 := create(40, "four")

	const sortArgs = `"sort":[{"property":"due"}]`
	query := func() []string {
		t.Helper()
		r := callGadget(t, ts, inv("Task/query", `{"accountId":"Atest1",`+sortArgs+`}`, "0"))
		return idsOf(methodArgs(t, r, 0, "Task/query"))
	}
	r := callGadget(t, ts, inv("Task/query", `{"accountId":"Atest1",`+sortArgs+`}`, "0"))
	q0 := methodArgs(t, r, 0, "Task/query")
	state0 := q0["queryState"].(string)
	if got := idsOf(q0); strings.Join(got, ",") != strings.Join([]string{t1, t2, t3, t4}, ",") {
		t.Fatalf("baseline order: %v", got)
	}
	cached := []string{t1, t2} // the client cached only the first page

	// A mutable-property edit is invisible to an immutable query: the
	// diet drops updated records entirely.
	r = callGadget(t, ts, inv("Task/set", fmt.Sprintf(
		`{"accountId":"Atest1","update":{%q:{"title":"renamed"}}}`, t3), "0"))
	if _, ok := methodArgs(t, r, 0, "Task/set")["updated"].(map[string]any)[t3]; !ok {
		t.Fatalf("title update failed: %v", r)
	}
	r = callGadget(t, ts, inv("Task/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1",`+sortArgs+`,"sinceQueryState":%q}`, state0), "0"))
	c := methodArgs(t, r, 0, "Task/queryChanges")
	if len(anySlice(c, "removed")) != 0 || len(anySlice(c, "added")) != 0 {
		t.Fatalf("diet: removed %v added %v, want empty", c["removed"], c["added"])
	}

	// Creations either side of the anchor, and a destroy past it.
	t0 := create(5, "zero")
	create(50, "five")
	r = callGadget(t, ts, inv("Task/set", fmt.Sprintf(
		`{"accountId":"Atest1","destroy":[%q]}`, t4), "0"))
	if destroyed, _ := methodArgs(t, r, 0, "Task/set")["destroyed"].([]any); len(destroyed) != 1 {
		t.Fatalf("destroy failed: %v", r)
	}
	r = callGadget(t, ts, inv("Task/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1",`+sortArgs+`,"sinceQueryState":%q,"upToId":%q}`, state0, t2), "0"))
	c = methodArgs(t, r, 0, "Task/queryChanges")
	added := anySlice(c, "added")
	if len(added) != 1 || added[0].(map[string]any)["id"] != t0 || added[0].(map[string]any)["index"] != float64(0) {
		t.Fatalf("added = %v, want [{%s 0}] (the creation past the anchor omitted)", added, t0)
	}
	removed := anySlice(c, "removed")
	if len(removed) != 1 || removed[0] != t4 {
		t.Fatalf("removed = %v, want [%s] (destroys only under the diet)", removed, t4)
	}
	cached = spliceApply(cached, removed, added)
	fresh := query()
	if strings.Join(cached, ",") != strings.Join(fresh[:len(cached)], ",") {
		t.Fatalf("anchored splice %v is not the refetch prefix %v", cached, fresh)
	}
}

// ---- collapse: the group companion expansion ----

func TestQueryChangesCollapseCompanion(t *testing.T) {
	ts := companionServer(t, &Extensions{Query: &QueryHooks{
		CollapseKey:    "subject",
		GroupCompanion: "Widget",
	}})
	// One Widget, two Gadgets in its group (subject holds the group id).
	r := callGadget(t, ts, inv("Widget/set",
		`{"accountId":"Atest1","create":{"w":{"name":"w"}}}`, "0"))
	w := methodArgs(t, r, 0, "Widget/set")["created"].(map[string]any)["w"].(map[string]any)["id"].(string)
	g1 := createGadget(t, ts, fmt.Sprintf(`{"subject":%q}`, w))
	g2 := createGadget(t, ts, fmt.Sprintf(`{"subject":%q}`, w))

	query := func() map[string]any {
		t.Helper()
		r := callGadget(t, ts, inv("Gadget/query", `{"accountId":"Atest1","collapseThreads":true}`, "0"))
		return methodArgs(t, r, 0, "Gadget/query")
	}
	q0 := query()
	cached := idsOf(q0)
	if len(cached) != 1 {
		t.Fatalf("collapsed baseline: %v", cached)
	}
	state0 := q0["queryState"].(string)

	// A companion-only commit: neither Gadget changed, but the group
	// did, so the expansion must surface the members - removed as a
	// superset, the representative re-added at its position.
	r = callGadget(t, ts, inv("Widget/set", fmt.Sprintf(
		`{"accountId":"Atest1","update":{%q:{"name":"renamed"}}}`, w), "0"))
	if _, ok := methodArgs(t, r, 0, "Widget/set")["updated"].(map[string]any)[w]; !ok {
		t.Fatalf("widget update failed: %v", r)
	}
	r = callGadget(t, ts, inv("Gadget/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1","collapseThreads":true,"sinceQueryState":%q}`, state0), "0"))
	c := methodArgs(t, r, 0, "Gadget/queryChanges")
	removed := anySlice(c, "removed")
	got := map[string]bool{}
	for _, id := range removed {
		got[id.(string)] = true
	}
	if !got[g1] || !got[g2] {
		t.Fatalf("removed %v lacks the expanded members %s %s", removed, g1, g2)
	}
	next := spliceApply(cached, removed, anySlice(c, "added"))
	if strings.Join(next, ",") != strings.Join(idsOf(query()), ",") {
		t.Fatalf("collapse splice %v vs refetch %v", next, idsOf(query()))
	}

	// The same state without collapse sees no Gadget-side changes at
	// all: an empty diff, not an expansion.
	r = callGadget(t, ts, inv("Gadget/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1","sinceQueryState":%q}`, state0), "0"))
	c = methodArgs(t, r, 0, "Gadget/queryChanges")
	if len(anySlice(c, "removed")) != 0 || len(anySlice(c, "added")) != 0 {
		t.Fatalf("uncollapsed diff not empty: %v", c)
	}
}

// ---- hostile input ----

func TestQueryChangesHostileStates(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	createNote(t, ts, `{"subject":"a"}`)

	for _, state := range []string{"abc", "-5", "1e9", "999999999999999999999999", "", "0x10", " 3", "3 ", "+3"} {
		r := callAPI(t, ts, inv("TestNote/queryChanges", fmt.Sprintf(
			`{"accountId":"Atest1","sinceQueryState":%q}`, state), "0"))
		if methodArgs(t, r, 0, "error")["type"] != "cannotCalculateChanges" {
			t.Errorf("state %q: %v, want cannotCalculateChanges", state, methodArgs(t, r, 0, "error"))
		}
	}
	// A state from the future: never issued, refused in O(1).
	r := callAPI(t, ts, inv("TestNote/queryChanges",
		`{"accountId":"Atest1","sinceQueryState":"999999"}`, "0"))
	if methodArgs(t, r, 0, "error")["type"] != "cannotCalculateChanges" {
		t.Fatal("future state accepted")
	}
}

// TestQueryChangesWorkBudget: a client far enough behind is refused
// before any walk; catching up (re-running the query) remains available.
func TestQueryChangesWorkBudget(t *testing.T) {
	saved := tuning.QueryChangesMaxWork
	tuning.QueryChangesMaxWork = 3
	defer func() { tuning.QueryChangesMaxWork = saved }()

	ts := noteServer(t, DefaultCoreCapabilities())
	for i := 0; i < 5; i++ {
		createNote(t, ts, fmt.Sprintf(`{"subject":"n%d"}`, i))
	}
	r := callAPI(t, ts, inv("TestNote/queryChanges",
		`{"accountId":"Atest1","sinceQueryState":"0"}`, "0"))
	if methodArgs(t, r, 0, "error")["type"] != "cannotCalculateChanges" {
		t.Fatal("over-budget distance accepted")
	}
	// Within the budget the answer works.
	r = callAPI(t, ts, inv("TestNote/queryChanges",
		`{"accountId":"Atest1","sinceQueryState":"3"}`, "0"))
	c := methodArgs(t, r, 0, "TestNote/queryChanges")
	if len(anySlice(c, "added")) != 2 {
		t.Fatalf("in-budget diff: %v", c)
	}
}

// TestQueryChangesUndeclared: a query /query would answer
// canCalculateChanges false for is refused with cannotCalculateChanges,
// after full argument validation.
func TestQueryChangesUndeclared(t *testing.T) {
	ts := companionServer(t, &Extensions{Query: &QueryHooks{
		Filter:          gadgetFilter{},
		LocalConditions: map[string][]string{"subject": {"subject"}},
	}})
	createGadget(t, ts, `{"subject":"a"}`)
	r := callGadget(t, ts, inv("Gadget/queryChanges",
		`{"accountId":"Atest1","sinceQueryState":"1","filter":{"text":"a"}}`, "0"))
	if methodArgs(t, r, 0, "error")["type"] != "cannotCalculateChanges" {
		t.Fatalf("undeclared condition: %v", methodArgs(t, r, 0, "error"))
	}
	// The declared condition answers.
	r = callGadget(t, ts, inv("Gadget/queryChanges",
		`{"accountId":"Atest1","sinceQueryState":"1","filter":{"subject":"a"}}`, "0"))
	methodArgs(t, r, 0, "Gadget/queryChanges")
}

// ---- the walk/evaluate race ----

// racingFilter is todoFilter with a trap: while shots is positive,
// each MatchCondition call first decrements it and runs commit,
// injecting a write between the change-log walk and the evaluation -
// the interleaving in which an evaluation snapshot can run ahead of
// the state the response claims to describe (section 5.6: an added
// index is only meaningful against newQueryState).
type racingFilter struct {
	shots  *int
	commit func()
}

func (racingFilter) ValidateCondition(name string, value json.RawMessage) error {
	return todoFilter{}.ValidateCondition(name, value)
}

func (f racingFilter) MatchCondition(ctx context.Context, acct jmap.Id, obj objectdb.Object, name string, value json.RawMessage) (bool, error) {
	if *f.shots > 0 {
		*f.shots--
		f.commit()
	}
	return todoFilter{}.MatchCondition(ctx, acct, obj, name, value)
}

// raceServer is gadgetServerType for the race tests: same wiring, but
// the db comes back too, so the test's injected commits and the served
// queries share one store.
func raceServer(t *testing.T, ext *Extensions) (*httptest.Server, *objectdb.DB) {
	t.Helper()
	a := authtest.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	if err := RegisterStandardTypeExt(p, db, todoType(), DefaultCoreCapabilities(), ext); err != nil {
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
	return ts, db
}

// TestQueryChangesRaceRetry: a commit that lands between the change
// walk and the evaluation would make the reported added indexes
// describe a later list than newQueryState - the client splice would
// place items off by the racer's position shift. The bracket read must
// catch it and the redo must fold the racing write into the diff, so
// the splice still converges with a refetch.
func TestQueryChangesRaceRetry(t *testing.T) {
	shots := 0
	var db *objectdb.DB
	inject := func() {
		if _, err := db.Update(context.Background(), "Atest1", func(u *objectdb.Update) error {
			_, err := u.Create("Todo", objectdb.Object{
				"title":    json.RawMessage(`"Aaa first by title"`),
				"keywords": json.RawMessage(`{"music":true}`),
			})
			return err
		}); err != nil {
			t.Error(err)
		}
	}
	ts, sdb := raceServer(t, &Extensions{Query: &QueryHooks{
		Filter:          racingFilter{shots: &shots, commit: func() { inject() }},
		LocalConditions: map[string][]string{"hasKeyword": {"keywords"}},
	}})
	db = sdb

	create := func(props string) string {
		t.Helper()
		r := callGadget(t, ts, inv("Todo/set",
			fmt.Sprintf(`{"accountId":"Atest1","create":{"c":%s}}`, props), "0"))
		created, ok := methodArgs(t, r, 0, "Todo/set")["created"].(map[string]any)
		if !ok {
			t.Fatalf("create failed: %v", r)
		}
		return created["c"].(map[string]any)["id"].(string)
	}
	alpha := create(`{"title":"Mmm middle","keywords":{"music":true}}`)
	beta := create(`{"title":"Zzz last","keywords":{"music":true}}`)

	const args = `"filter":{"hasKeyword":"music"},"sort":[{"property":"title"}]`
	query := func() []string {
		t.Helper()
		r := callGadget(t, ts, inv("Todo/query", `{"accountId":"Atest1",`+args+`}`, "0"))
		return idsOf(methodArgs(t, r, 0, "Todo/query"))
	}
	cached := query()
	if len(cached) != 2 || cached[0] != alpha || cached[1] != beta {
		t.Fatalf("baseline %v, want [%s %s]", cached, alpha, beta)
	}
	r := callGadget(t, ts, inv("Todo/query", `{"accountId":"Atest1",`+args+`}`, "0"))
	state0 := methodArgs(t, r, 0, "Todo/query")["queryState"].(string)

	// A real change so queryChanges has work to do (tier 0 must not
	// answer), then one armed shot: the first predicate check commits a
	// Todo sorting before everything else.
	r = callGadget(t, ts, inv("Todo/set", fmt.Sprintf(
		`{"accountId":"Atest1","update":{%q:{"keywords":{"music":true,"jazz":true}}}}`, beta), "0"))
	if _, ok := methodArgs(t, r, 0, "Todo/set")["updated"].(map[string]any)[beta]; !ok {
		t.Fatalf("update failed: %v", r)
	}
	shots = 1
	r = callGadget(t, ts, inv("Todo/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1",`+args+`,"sinceQueryState":%q}`, state0), "1"))
	c := methodArgs(t, r, 0, "Todo/queryChanges")
	if shots != 0 {
		t.Fatal("the racing commit never fired; the test exercised nothing")
	}
	next := spliceApply(cached, anySlice(c, "removed"), anySlice(c, "added"))
	fresh := query()
	if strings.Join(next, ",") != strings.Join(fresh, ",") {
		t.Fatalf("raced splice %v vs refetch %v", next, fresh)
	}
	if len(fresh) != 3 {
		t.Fatalf("refetch %v should include the racing Todo", fresh)
	}

	// A racer on every attempt exhausts the one redo: the answer must
	// be the always-legal refusal, never indexes that can be wrong.
	r = callGadget(t, ts, inv("Todo/query", `{"accountId":"Atest1",`+args+`}`, "0"))
	state1 := methodArgs(t, r, 0, "Todo/query")["queryState"].(string)
	r = callGadget(t, ts, inv("Todo/set", fmt.Sprintf(
		`{"accountId":"Atest1","update":{%q:{"keywords":{"music":true,"funk":true}}}}`, alpha), "0"))
	if _, ok := methodArgs(t, r, 0, "Todo/set")["updated"].(map[string]any)[alpha]; !ok {
		t.Fatalf("update failed: %v", r)
	}
	shots = 1 << 20
	r = callGadget(t, ts, inv("Todo/queryChanges", fmt.Sprintf(
		`{"accountId":"Atest1",`+args+`,"sinceQueryState":%q}`, state1), "1"))
	shots = 0
	if methodArgs(t, r, 0, "error")["type"] != "cannotCalculateChanges" {
		t.Fatalf("persistent racer: %v, want cannotCalculateChanges", methodArgs(t, r, 0, "error"))
	}
}

// ---- the expansion budget on one oversized group ----

// TestQueryChangesMonsterGroup: a single changed group larger than the
// remaining budget must refuse - and must do so without materializing
// the whole group (the scan itself is bounded at remaining+1). A group
// that exactly fits is answered.
func TestQueryChangesMonsterGroup(t *testing.T) {
	saved := tuning.QueryChangesMaxWork
	tuning.QueryChangesMaxWork = 3
	defer func() { tuning.QueryChangesMaxWork = saved }()

	run := func(members int) *jmap.Response {
		t.Helper()
		ts := companionServer(t, &Extensions{Query: &QueryHooks{
			CollapseKey:    "subject",
			GroupCompanion: "Widget",
		}})
		r := callGadget(t, ts, inv("Widget/set",
			`{"accountId":"Atest1","create":{"w":{"name":"w"}}}`, "0"))
		w := methodArgs(t, r, 0, "Widget/set")["created"].(map[string]any)["w"].(map[string]any)["id"].(string)
		for i := 0; i < members; i++ {
			createGadget(t, ts, fmt.Sprintf(`{"subject":%q}`, w))
		}
		r = callGadget(t, ts, inv("Gadget/query", `{"accountId":"Atest1","collapseThreads":true}`, "0"))
		state0 := methodArgs(t, r, 0, "Gadget/query")["queryState"].(string)
		// One companion-only commit behind: the walk is trivially inside
		// the budget, so the verdict is the expansion's alone.
		r = callGadget(t, ts, inv("Widget/set", fmt.Sprintf(
			`{"accountId":"Atest1","update":{%q:{"name":"renamed"}}}`, w), "0"))
		if _, ok := methodArgs(t, r, 0, "Widget/set")["updated"].(map[string]any)[w]; !ok {
			t.Fatalf("widget update failed: %v", r)
		}
		return callGadget(t, ts, inv("Gadget/queryChanges", fmt.Sprintf(
			`{"accountId":"Atest1","collapseThreads":true,"sinceQueryState":%q}`, state0), "0"))
	}

	if got := methodArgs(t, run(4), 0, "error")["type"]; got != "cannotCalculateChanges" {
		t.Fatalf("oversized group: %v, want cannotCalculateChanges", got)
	}
	c := methodArgs(t, run(3), 0, "Gadget/queryChanges")
	if len(anySlice(c, "removed")) != 3 {
		t.Fatalf("exact-fit removed %v, want all 3 members", anySlice(c, "removed"))
	}
}
