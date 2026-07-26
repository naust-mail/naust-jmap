package runtime

// Foo/query's canCalculateChanges is per-query truth (RFC 8620 section
// 5.5 defines it "with these "filter"/"sort" parameters"): true exactly
// when every part of the query is record-local - by construction for
// core conditions and comparators, by QueryHooks declaration for a
// type's own semantics. These tests pin the declaration surface: its
// registration-time validation, the per-query verdicts, and the group
// companion's effect on the query state.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/internal/authtest"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// widgetType is a second registrable type for group-companion tests.
func widgetType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "Widget",
		Capability: "urn:example:gadget",
		Properties: map[string]descriptor.Property{
			"name": {Kind: descriptor.KindString},
		},
	}
}

func passIds(_ context.Context, _ jmap.Id, matched []QueryRecord, _ func(a, b objectdb.Object) int, _ map[string]json.RawMessage) ([]jmap.Id, error) {
	ids := make([]jmap.Id, len(matched))
	for i, m := range matched {
		ids[i] = m.Id
	}
	return ids, nil
}

// TestChangeCalcRegistration: every malformed declaration is a loud
// registration error - the declarations qualify hooks, so a declaration
// without its hook, or one naming unknown data, is a bug in the
// embedder, not something to degrade around at request time.
func TestChangeCalcRegistration(t *testing.T) {
	cases := []struct {
		name string
		ext  *Extensions
	}{
		{"LocalConditions without Filter hook", &Extensions{
			Query: &QueryHooks{LocalConditions: map[string][]string{"subject": nil}}}},
		{"LocalSorts without Sort hook", &Extensions{
			Query: &QueryHooks{LocalSorts: map[string][]string{"subject": nil}}}},
		{"LocalArrange without Arrange hook", &Extensions{
			Query: &QueryHooks{LocalArrange: true}}},
		{"reads naming unknown property", &Extensions{
			Query: &QueryHooks{Filter: gadgetFilter{},
				LocalConditions: map[string][]string{"subject": {"nope"}}}}},
		{"empty declared name", &Extensions{
			Query: &QueryHooks{Filter: gadgetFilter{},
				LocalConditions: map[string][]string{"": nil}}}},
		{"GroupCompanion without CollapseKey", &Extensions{
			Query: &QueryHooks{GroupCompanion: "Widget"}}},
		{"GroupCompanion never registered", &Extensions{
			Query: &QueryHooks{CollapseKey: "subject", GroupCompanion: "Ghost"}}},
		{"GroupCompanion names the type itself", &Extensions{
			Query: &QueryHooks{CollapseKey: "subject", GroupCompanion: "Gadget"}}},
	}
	for _, tc := range cases {
		be := memory.New()
		db := objectdb.New(be, lease.NewInProcess(be))
		if err := db.RegisterType(widgetType()); err != nil {
			t.Fatal(err)
		}
		if err := RegisterStandardTypeExt(NewProcessor(), db, gadgetType(), DefaultCoreCapabilities(), tc.ext); err == nil {
			t.Errorf("%s: registration succeeded, want error", tc.name)
		}
	}

	// The valid shape registers: declared conditions beside their Filter
	// hook, a companion registered first, LocalArrange beside Arrange.
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	if err := RegisterStandardType(p, db, widgetType(), DefaultCoreCapabilities()); err != nil {
		t.Fatal(err)
	}
	ok := &Extensions{Query: &QueryHooks{
		Filter:          gadgetFilter{},
		LocalConditions: map[string][]string{"subject": {"subject"}, "text": nil},
		Arrange:         passIds,
		LocalArrange:    true,
		CollapseKey:     "subject",
		GroupCompanion:  "Widget",
	}}
	if err := RegisterStandardTypeExt(p, db, gadgetType(), DefaultCoreCapabilities(), ok); err != nil {
		t.Fatalf("valid declarations rejected: %v", err)
	}
}

// companionServer registers Widget then Gadget on one server, Gadget
// declaring Widget as its group companion, and returns the server.
func companionServer(t *testing.T, ext *Extensions) *httptest.Server {
	t.Helper()
	a := authtest.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	if err := RegisterStandardType(p, db, widgetType(), DefaultCoreCapabilities()); err != nil {
		t.Fatal(err)
	}
	if err := RegisterStandardTypeExt(p, db, gadgetType(), DefaultCoreCapabilities(), ext); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(a, p, "https://jmap.example.com", DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Capability("urn:example:gadget").Advertise(struct{}{}, struct{}{}).Err(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// TestQueryCanCalculateChanges: the verdict follows the query, not the
// type - declared parts answer true, anything undeclared or extras-
// driven answers false.
func TestQueryCanCalculateChanges(t *testing.T) {
	ts := companionServer(t, &Extensions{
		ExtraArgs: map[string]MethodArgs{"query": {Names: []string{"reverse"}}},
		Query: &QueryHooks{
			Filter: gadgetFilter{},
			// "subject" is declared record-local; "text" deliberately not.
			LocalConditions: map[string][]string{"subject": {"subject"}},
			Arrange:         passIds,
			LocalArrange:    true,
			CollapseKey:     "subject",
			GroupCompanion:  "Widget",
		},
	})
	createGadget(t, ts, `{"subject":"apple"}`)

	cases := []struct {
		name string
		args string
		want bool
	}{
		{"declared condition", `{"accountId":"Atest1","filter":{"subject":"apple"}}`, true},
		{"undeclared condition", `{"accountId":"Atest1","filter":{"text":"app"}}`, false},
		{"operator over declared conditions",
			`{"accountId":"Atest1","filter":{"operator":"AND","conditions":[{"subject":"a"},{"subject":"b"}]}}`, true},
		{"undeclared leaf under operator",
			`{"accountId":"Atest1","filter":{"operator":"AND","conditions":[{"subject":"a"},{"text":"b"}]}}`, false},
		// No Sort hook: core comparators are record-local by construction.
		{"core sort", `{"accountId":"Atest1","sort":[{"property":"subject"}]}`, true},
		// Extra arguments drive Arrange beyond its declared identity.
		{"extras present", `{"accountId":"Atest1","reverse":true}`, false},
		// Collapse is calculable because the companion is declared.
		{"collapse with companion", `{"accountId":"Atest1","collapseThreads":true}`, true},
	}
	for _, tc := range cases {
		r := callGadget(t, ts, inv("Gadget/query", tc.args, "0"))
		args := methodArgs(t, r, 0, "Gadget/query")
		if args["canCalculateChanges"] != tc.want {
			t.Errorf("%s: canCalculateChanges = %v, want %v", tc.name, args["canCalculateChanges"], tc.want)
		}
	}

	// Arrange without LocalArrange: never calculable, even for an
	// otherwise fully declared query.
	ts2 := gadgetServer(t, &Extensions{Query: &QueryHooks{
		Filter:          gadgetFilter{},
		LocalConditions: map[string][]string{"subject": {"subject"}},
		Arrange:         passIds,
	}})
	r := callGadget(t, ts2, inv("Gadget/query", `{"accountId":"Atest1","filter":{"subject":"a"}}`, "0"))
	if args := methodArgs(t, r, 0, "Gadget/query"); args["canCalculateChanges"] != false {
		t.Errorf("undeclared Arrange: canCalculateChanges = %v, want false", args["canCalculateChanges"])
	}

	// Collapse without a companion: the sibling-driven changes a
	// collapsed list can undergo are invisible in the type's own log.
	ts3 := gadgetServer(t, &Extensions{Query: &QueryHooks{CollapseKey: "subject"}})
	r = callGadget(t, ts3, inv("Gadget/query", `{"accountId":"Atest1","collapseThreads":true}`, "0"))
	if args := methodArgs(t, r, 0, "Gadget/query"); args["canCalculateChanges"] != false {
		t.Errorf("collapse without companion: canCalculateChanges = %v, want false", args["canCalculateChanges"])
	}
}

// TestQueryStateGroupCompanion: with a companion declared, the query
// state is the later of the two type states, so a commit touching only
// the companion moves it - the 5.5 MUST for collapsed results, whose
// representative can be displaced by a companion-side change alone.
func TestQueryStateGroupCompanion(t *testing.T) {
	ts := companionServer(t, &Extensions{Query: &QueryHooks{
		CollapseKey:    "subject",
		GroupCompanion: "Widget",
	}})
	createGadget(t, ts, `{"subject":"apple"}`)

	query := func() string {
		r := callGadget(t, ts, inv("Gadget/query", `{"accountId":"Atest1"}`, "0"))
		return methodArgs(t, r, 0, "Gadget/query")["queryState"].(string)
	}
	s0 := query()
	if s1 := query(); s1 != s0 {
		t.Fatalf("query state moved with no commit: %q -> %q", s0, s1)
	}

	// A Widget-only commit: the Gadget type state is untouched, the
	// joined query state must still advance.
	r := callGadget(t, ts, inv("Widget/set",
		`{"accountId":"Atest1","create":{"w":{"name":"x"}}}`, "0"))
	if created, ok := methodArgs(t, r, 0, "Widget/set")["created"].(map[string]any); !ok || created["w"] == nil {
		t.Fatalf("widget create failed: %v", r)
	}
	if s2 := query(); s2 == s0 {
		t.Fatalf("query state did not move on a companion-only commit: %q", s2)
	}
}
