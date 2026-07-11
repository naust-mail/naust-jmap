package runtime

import (
	"fmt"
	"net/http/httptest"
	"testing"
)

// queryNotes runs TestNote/query with the given argument JSON and
// returns the response arguments (which may be an error invocation's).
func queryNotes(t *testing.T, ts *httptest.Server, args string) map[string]any {
	t.Helper()
	r := callAPI(t, ts, inv("TestNote/query", args, "0"))
	got := r.MethodResponses[0]
	name := "TestNote/query"
	if got.Name == "error" {
		name = "error"
	}
	return methodArgs(t, r, 0, name)
}

func queryIds(t *testing.T, args map[string]any) []string {
	t.Helper()
	raw, ok := args["ids"].([]any)
	if !ok {
		t.Fatalf("no ids in response: %v", args)
	}
	ids := make([]string, len(raw))
	for i, v := range raw {
		ids[i] = v.(string)
	}
	return ids
}

func wantIds(t *testing.T, args map[string]any, want ...string) {
	t.Helper()
	got := queryIds(t, args)
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

// bySubject creates one note per subject and returns subject -> id.
func bySubject(t *testing.T, ts *httptest.Server, subjects ...string) map[string]string {
	t.Helper()
	ids := make(map[string]string, len(subjects))
	for _, s := range subjects {
		ids[s] = createNote(t, ts, fmt.Sprintf(`{"subject":%q}`, s))
	}
	return ids
}

func TestQueryNullFilterCasemapSort(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	ids := bySubject(t, ts, "Banana", "apple", "Cherry")

	// Null filter includes everything (5.5); i;ascii-casemap is the
	// default string collation, so case must not affect the order.
	args := queryNotes(t, ts,
		`{"accountId":"Atest1","sort":[{"property":"subject"}]}`)
	wantIds(t, args, ids["apple"], ids["Banana"], ids["Cherry"])

	if args["canCalculateChanges"] != false {
		t.Fatalf("canCalculateChanges = %v, want false (M0)", args["canCalculateChanges"])
	}
	if args["position"] != float64(0) {
		t.Fatalf("position = %v, want 0", args["position"])
	}
	// total MUST be omitted when calculateTotal was not requested (5.5).
	if _, has := args["total"]; has {
		t.Fatalf("total present without calculateTotal: %v", args)
	}
	// The client gave no limit, so the server-enforced one is returned.
	if args["limit"] != float64(DefaultCoreCapabilities().MaxObjectsInGet) {
		t.Fatalf("limit = %v, want server cap", args["limit"])
	}

	// isAscending false reverses the comparator (5.5).
	args = queryNotes(t, ts,
		`{"accountId":"Atest1","sort":[{"property":"subject","isAscending":false}]}`)
	wantIds(t, args, ids["Cherry"], ids["Banana"], ids["apple"])
}

func TestQueryEmptySortIsStable(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	bySubject(t, ts, "b", "a", "c")

	// With no comparators the order is server-dependent but MUST be
	// stable between calls (5.5), as must queryState.
	first := queryNotes(t, ts, `{"accountId":"Atest1"}`)
	second := queryNotes(t, ts, `{"accountId":"Atest1"}`)
	wantIds(t, second, queryIds(t, first)...)
	if first["queryState"] != second["queryState"] {
		t.Fatalf("queryState changed with no data change: %v vs %v",
			first["queryState"], second["queryState"])
	}
}

func TestQueryFilterConditions(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	hello := createNote(t, ts, `{"subject":"hello","flagged":true}`)
	world := createNote(t, ts, `{"subject":"world","flagged":true}`)
	other := createNote(t, ts, `{"subject":"hello","flagged":false}`)

	// Equality on an indexed property (the planner's range-scan path);
	// matching follows the casemap comparison rules, so case is ignored.
	args := queryNotes(t, ts,
		`{"accountId":"Atest1","filter":{"subject":"HELLO"},"sort":[{"property":"flagged"}]}`)
	wantIds(t, args, other, hello)

	// A multi-property condition ANDs its keys.
	args = queryNotes(t, ts,
		`{"accountId":"Atest1","filter":{"subject":"hello","flagged":true}}`)
	wantIds(t, args, hello)

	// FilterOperator composition (5.5): NOT of an OR.
	args = queryNotes(t, ts, `{"accountId":"Atest1","filter":
		{"operator":"NOT","conditions":[
			{"operator":"OR","conditions":[{"subject":"hello"},{"subject":"world"}]}]}}`)
	wantIds(t, args)

	// AND across operator and condition levels.
	args = queryNotes(t, ts, `{"accountId":"Atest1","filter":
		{"operator":"AND","conditions":[{"flagged":true},{"subject":"world"}]}}`)
	wantIds(t, args, world)
}

func TestQueryFilterErrors(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	createNote(t, ts, `{"subject":"x"}`)

	for _, tc := range []struct {
		filter, wantType string
	}{
		// Undeclared condition property: syntactically valid but the
		// server cannot process it (5.5).
		{`{"nope":"x"}`, "unsupportedFilter"},
		// operator MUST be AND/OR/NOT.
		{`{"operator":"XOR","conditions":[]}`, "invalidArguments"},
		// A FilterOperator has exactly operator+conditions.
		{`{"operator":"AND"}`, "invalidArguments"},
		{`{"operator":"AND","conditions":[],"subject":"x"}`, "invalidArguments"},
		// Condition value of the wrong kind for the property.
		{`{"subject":42}`, "invalidArguments"},
		{`"subject"`, "invalidArguments"},
	} {
		args := queryNotes(t, ts,
			fmt.Sprintf(`{"accountId":"Atest1","filter":%s}`, tc.filter))
		if args["type"] != tc.wantType {
			t.Fatalf("filter %s: got %v, want %s", tc.filter, args["type"], tc.wantType)
		}
	}
}

func TestQuerySortErrors(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	createNote(t, ts, `{"subject":"x"}`)

	for _, tc := range []struct {
		sort, wantType string
	}{
		// Sorting on an undeclared property or an unknown collation is
		// unsupportedSort (5.5).
		{`[{"property":"nope"}]`, "unsupportedSort"},
		{`[{"property":"subject","collation":"i;octet"}]`, "unsupportedSort"},
		// A Comparator without a property is not syntactically valid.
		{`[{"isAscending":true}]`, "invalidArguments"},
	} {
		args := queryNotes(t, ts,
			fmt.Sprintf(`{"accountId":"Atest1","sort":%s}`, tc.sort))
		if args["type"] != tc.wantType {
			t.Fatalf("sort %s: got %v, want %s", tc.sort, args["type"], tc.wantType)
		}
	}

	// A Comparator may carry additional type-specific properties (5.5),
	// so extras must NOT be rejected.
	args := queryNotes(t, ts,
		`{"accountId":"Atest1","sort":[{"property":"subject","custom":true}]}`)
	if _, isErr := args["type"]; isErr {
		t.Fatalf("extra Comparator property rejected: %v", args)
	}

	// Collation is ignored when the property is not a string (5.5).
	args = queryNotes(t, ts,
		`{"accountId":"Atest1","sort":[{"property":"flagged","collation":"i;octet"}]}`)
	if _, isErr := args["type"]; isErr {
		t.Fatalf("collation on non-string rejected: %v", args)
	}
}

func TestQueryNumericCollation(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	ids := bySubject(t, ts, "9", "10", "007", "banana")

	// i;ascii-numeric (RFC 4790): leading digit runs compared as
	// unbounded integers ("007" = 7 < 9 < 10); values with no leading
	// digit sort after all numeric values.
	args := queryNotes(t, ts,
		`{"accountId":"Atest1","sort":[{"property":"subject","collation":"i;ascii-numeric"}]}`)
	wantIds(t, args, ids["007"], ids["9"], ids["10"], ids["banana"])
}

func TestQueryPositionAndLimit(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	ids := bySubject(t, ts, "a", "b", "c", "d", "e")
	sorted := `"sort":[{"property":"subject"}]`

	// Window from a positive position with a client limit.
	args := queryNotes(t, ts,
		fmt.Sprintf(`{"accountId":"Atest1",%s,"position":1,"limit":2}`, sorted))
	wantIds(t, args, ids["b"], ids["c"])
	if args["position"] != float64(1) {
		t.Fatalf("position = %v, want 1", args["position"])
	}
	// The client limit was within the cap, so no limit in the response.
	if _, has := args["limit"]; has {
		t.Fatalf("limit echoed though the server did not change it: %v", args)
	}

	// Negative position is added to the total (5.5): -2 of 5 -> index 3.
	args = queryNotes(t, ts,
		fmt.Sprintf(`{"accountId":"Atest1",%s,"position":-2}`, sorted))
	wantIds(t, args, ids["d"], ids["e"])
	if args["position"] != float64(3) {
		t.Fatalf("position = %v, want 3", args["position"])
	}

	// Still negative after adding the total -> clamped to 0.
	args = queryNotes(t, ts,
		fmt.Sprintf(`{"accountId":"Atest1",%s,"position":-99}`, sorted))
	wantIds(t, args, ids["a"], ids["b"], ids["c"], ids["d"], ids["e"])

	// position >= total: empty ids, not an error (5.5).
	args = queryNotes(t, ts,
		fmt.Sprintf(`{"accountId":"Atest1",%s,"position":5}`, sorted))
	wantIds(t, args)

	// Negative limit MUST be rejected with invalidArguments (5.5).
	args = queryNotes(t, ts, `{"accountId":"Atest1","limit":-1}`)
	if args["type"] != "invalidArguments" {
		t.Fatalf("negative limit: got %v", args["type"])
	}
}

func TestQueryServerLimitClamp(t *testing.T) {
	core := DefaultCoreCapabilities()
	core.MaxObjectsInGet = 2
	ts := noteServer(t, core)
	ids := bySubject(t, ts, "a", "b", "c")

	// A greater limit than the server maximum is clamped, and the new
	// limit is returned with the response (5.5).
	args := queryNotes(t, ts,
		`{"accountId":"Atest1","sort":[{"property":"subject"}],"limit":10}`)
	wantIds(t, args, ids["a"], ids["b"])
	if args["limit"] != float64(2) {
		t.Fatalf("limit = %v, want clamped 2", args["limit"])
	}
}

func TestQueryAnchor(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	ids := bySubject(t, ts, "a", "b", "c", "d")
	sorted := `"sort":[{"property":"subject"}]`

	// The anchor's index becomes the position; any client position MUST
	// be ignored when an anchor is given (5.5).
	args := queryNotes(t, ts, fmt.Sprintf(
		`{"accountId":"Atest1",%s,"anchor":%q,"position":99,"limit":2}`, sorted, ids["b"]))
	wantIds(t, args, ids["b"], ids["c"])
	if args["position"] != float64(1) {
		t.Fatalf("position = %v, want 1", args["position"])
	}

	// anchorOffset -1: the record immediately preceding the anchor.
	args = queryNotes(t, ts, fmt.Sprintf(
		`{"accountId":"Atest1",%s,"anchor":%q,"anchorOffset":-1,"limit":2}`, sorted, ids["c"]))
	wantIds(t, args, ids["b"], ids["c"])

	// Offset pushing the index negative clamps to 0.
	args = queryNotes(t, ts, fmt.Sprintf(
		`{"accountId":"Atest1",%s,"anchor":%q,"anchorOffset":-9,"limit":2}`, sorted, ids["a"]))
	wantIds(t, args, ids["a"], ids["b"])

	// The anchor is looked for AFTER filtering: a real record outside
	// the filtered results is anchorNotFound, as is an unknown id.
	args = queryNotes(t, ts, fmt.Sprintf(
		`{"accountId":"Atest1","filter":{"subject":"a"},"anchor":%q}`, ids["c"]))
	if args["type"] != "anchorNotFound" {
		t.Fatalf("filtered-out anchor: got %v", args["type"])
	}
	args = queryNotes(t, ts,
		`{"accountId":"Atest1","anchor":"Anope"}`)
	if args["type"] != "anchorNotFound" {
		t.Fatalf("unknown anchor: got %v", args["type"])
	}
}

func TestQueryTotalAndState(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	ids := bySubject(t, ts, "a", "b", "c")

	// calculateTotal: total is the full filtered count, independent of
	// the returned window (5.5).
	args := queryNotes(t, ts,
		`{"accountId":"Atest1","calculateTotal":true,"limit":1,"sort":[{"property":"subject"}]}`)
	if args["total"] != float64(3) {
		t.Fatalf("total = %v, want 3", args["total"])
	}
	wantIds(t, args, ids["a"])
	state := args["queryState"].(string)

	// queryState MUST change when the matching ids change (5.5): destroy
	// a record in the results.
	r := callAPI(t, ts, inv("TestNote/set",
		fmt.Sprintf(`{"accountId":"Atest1","destroy":[%q]}`, ids["b"]), "0"))
	methodArgs(t, r, 0, "TestNote/set")
	args = queryNotes(t, ts,
		`{"accountId":"Atest1","calculateTotal":true,"limit":1,"sort":[{"property":"subject"}]}`)
	if args["queryState"] == state {
		t.Fatal("queryState unchanged after a result was destroyed")
	}
	if args["total"] != float64(2) {
		t.Fatalf("total after destroy = %v, want 2", args["total"])
	}
}

func TestQueryAccountChecks(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	args := queryNotes(t, ts, `{"accountId":"Anope"}`)
	if args["type"] != "accountNotFound" {
		t.Fatalf("unknown account: got %v", args["type"])
	}
	args = queryNotes(t, ts, `{"accountId":"Atest1","bogus":1}`)
	if args["type"] != "invalidArguments" {
		t.Fatalf("unknown argument: got %v", args["type"])
	}
}

// Foo/queryChanges (5.6): the runtime keeps no per-query change history
// (/query always answers canCalculateChanges: false), so a fully valid
// call gets cannotCalculateChanges - not unknownMethod - and invalid
// arguments are rejected before that answer.
func TestQueryChanges(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	createNote(t, ts, `{"subject":"a"}`)

	cases := []struct {
		name    string
		args    string
		errType string
	}{
		{"valid minimal call",
			`{"accountId":"Atest1","sinceQueryState":"abc"}`,
			"cannotCalculateChanges"},
		{"valid full call",
			`{"accountId":"Atest1","sinceQueryState":"abc","filter":{"subject":"a"},"sort":[{"property":"subject"}],"maxChanges":0,"upToId":"x","calculateTotal":true}`,
			"cannotCalculateChanges"},
		{"missing sinceQueryState",
			`{"accountId":"Atest1"}`,
			"invalidArguments"},
		{"negative maxChanges",
			`{"accountId":"Atest1","sinceQueryState":"abc","maxChanges":-1}`,
			"invalidArguments"},
		{"unknown account",
			`{"accountId":"Anope","sinceQueryState":"abc"}`,
			"accountNotFound"},
		{"undeclared filter property",
			`{"accountId":"Atest1","sinceQueryState":"abc","filter":{"nope":"x"}}`,
			"unsupportedFilter"},
		{"undeclared sort property",
			`{"accountId":"Atest1","sinceQueryState":"abc","sort":[{"property":"nope"}]}`,
			"unsupportedSort"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := callAPI(t, ts, inv("TestNote/queryChanges", tc.args, "0"))
			args := methodArgs(t, r, 0, "error")
			if args["type"] != tc.errType {
				t.Fatalf("error type = %v, want %s", args["type"], tc.errType)
			}
		})
	}
}
