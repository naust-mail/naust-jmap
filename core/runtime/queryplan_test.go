package runtime

// Tests for the set-algebra query planner (query.go). The headline test is
// metamorphic: for random AND/OR/NOT filter trees, the planner's result
// (ids AND total) MUST equal a brute-force oracle over all records. That
// pins down the non-negotiable invariant - producers only narrow, the
// predicate always verifies - and catches a subtly non-exact producer that
// would drop or add results only on the compound-filter path (including the
// fast path, which trusts an exact set without a per-record recheck). The
// white-box tests assert the exact/narrowed flags the fast path keys on,
// and the collapse tests cover the core collapseThreads behaviour on
// synthetic data with no mail dependency.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// note is one synthetic record: subject and flagged are Indexed (the
// generic set producer narrows them exactly), body is not (a universe
// producer, verified only by the residual predicate).
type note struct {
	id      string
	subject string
	flagged bool
	body    string
}

// eval mirrors the core equality FilterCondition semantics for the oracle.
func (n note) matchLeaf(cond map[string]any) bool {
	for k, v := range cond {
		switch k {
		case "subject":
			if n.subject != v.(string) {
				return false
			}
		case "flagged":
			if n.flagged != v.(bool) {
				return false
			}
		case "body":
			if n.body != v.(string) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// tnode is a generated filter: op "" is a leaf (cond), else AND/OR/NOT.
type tnode struct {
	op   string
	kids []tnode
	cond map[string]any
}

func (t tnode) eval(n note) bool {
	switch t.op {
	case "AND":
		for _, k := range t.kids {
			if !k.eval(n) {
				return false
			}
		}
		return true
	case "OR":
		for _, k := range t.kids {
			if k.eval(n) {
				return true
			}
		}
		return false
	case "NOT":
		// NOT matches iff no child matches (core matches() semantics).
		for _, k := range t.kids {
			if k.eval(n) {
				return false
			}
		}
		return true
	}
	return n.matchLeaf(t.cond)
}

func (t tnode) json() string {
	if t.op == "" {
		b, _ := json.Marshal(t.cond)
		return string(b)
	}
	parts := make([]string, len(t.kids))
	for i, k := range t.kids {
		parts[i] = k.json()
	}
	conds := "[" + join(parts, ",") + "]"
	return fmt.Sprintf(`{"operator":%q,"conditions":%s}`, t.op, conds)
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

var (
	genSubjects = []string{"s0", "s1", "s2"}
	genBodies   = []string{"b0", "b1"}
)

// genLeaf builds a random leaf condition, sometimes a two-key one (an
// implicit AND, exercising intra-leaf intersection).
func genLeaf(r *rand.Rand) tnode {
	switch r.Intn(5) {
	case 0:
		return tnode{cond: map[string]any{"subject": genSubjects[r.Intn(len(genSubjects))]}}
	case 1:
		return tnode{cond: map[string]any{"flagged": r.Intn(2) == 0}}
	case 2:
		return tnode{cond: map[string]any{"body": genBodies[r.Intn(len(genBodies))]}}
	case 3:
		return tnode{cond: map[string]any{
			"subject": genSubjects[r.Intn(len(genSubjects))],
			"flagged": r.Intn(2) == 0,
		}}
	default:
		return tnode{cond: map[string]any{
			"subject": genSubjects[r.Intn(len(genSubjects))],
			"body":    genBodies[r.Intn(len(genBodies))],
		}}
	}
}

func genTree(r *rand.Rand, depth int) tnode {
	if depth == 0 || r.Intn(3) == 0 {
		return genLeaf(r)
	}
	op := []string{"AND", "OR", "NOT"}[r.Intn(3)]
	n := 1 + r.Intn(3)
	if op == "NOT" {
		n = 1 + r.Intn(2)
	}
	kids := make([]tnode, n)
	for i := range kids {
		kids[i] = genTree(r, depth-1)
	}
	return tnode{op: op, kids: kids}
}

// planNotes creates a fixed corpus of notes over a HTTP TestNote server and
// returns them plus the server.
func planNotes(t *testing.T) (*httptest.Server, []note) {
	t.Helper()
	ts := noteServer(t, DefaultCoreCapabilities())
	r := rand.New(rand.NewSource(7))
	creates := map[string]any{}
	corpus := make([]note, 0, 40)
	for i := 0; i < 40; i++ {
		cid := fmt.Sprintf("c%d", i)
		n := note{
			subject: genSubjects[r.Intn(len(genSubjects))],
			flagged: r.Intn(2) == 0,
			body:    genBodies[r.Intn(len(genBodies))],
		}
		creates[cid] = map[string]any{"subject": n.subject, "flagged": n.flagged, "body": n.body}
		corpus = append(corpus, n)
	}
	body, _ := json.Marshal(map[string]any{"accountId": "Atest1", "create": creates})
	resp := callAPI(t, ts, inv("TestNote/set", string(body), "0"))
	created := methodArgs(t, resp, 0, "TestNote/set")["created"].(map[string]any)
	for i := range corpus {
		cid := fmt.Sprintf("c%d", i)
		corpus[i].id = created[cid].(map[string]any)["id"].(string)
	}
	return ts, corpus
}

// TestQuerySetAlgebraMetamorphic is the core insurance: planner result ==
// brute-force oracle, for many random filter trees.
func TestQuerySetAlgebraMetamorphic(t *testing.T) {
	ts, corpus := planNotes(t)
	r := rand.New(rand.NewSource(99))
	for iter := 0; iter < 400; iter++ {
		tree := genTree(r, 3)
		filter := tree.json()
		args := fmt.Sprintf(`{"accountId":"Atest1","filter":%s,"calculateTotal":true}`, filter)
		resp := callAPI(t, ts, inv("TestNote/query", args, "0"))
		got := methodArgs(t, resp, 0, "TestNote/query")
		if _, isErr := got["type"]; isErr {
			t.Fatalf("iter %d filter %s: error %v", iter, filter, got)
		}

		// Oracle: matching ids in id order (the empty-sort result order).
		var want []string
		for _, n := range corpus {
			if tree.eval(n) {
				want = append(want, n.id)
			}
		}
		sort.Strings(want)

		gotIds := toStrs(got["ids"])
		if !equalStrs(gotIds, want) {
			t.Fatalf("iter %d filter %s:\n got  %v\n want %v", iter, filter, gotIds, want)
		}
		if total := int(got["total"].(float64)); total != len(want) {
			t.Fatalf("iter %d filter %s: total %d, want %d", iter, filter, total, len(want))
		}
	}
}

// planDB builds an in-memory TestNote db with a fixed corpus for the
// white-box planner tests, returning the stdType and the corpus.
func planDB(t testing.TB) (*stdType, []note) {
	ctx := context.Background()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	if err := db.RegisterType(testNoteType()); err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(3))
	var corpus []note
	if _, err := db.Update(ctx, "Atest1", func(u *objectdb.Update) error {
		for i := 0; i < 12; i++ {
			n := note{
				subject: genSubjects[r.Intn(len(genSubjects))],
				flagged: r.Intn(2) == 0,
				body:    genBodies[r.Intn(len(genBodies))],
			}
			id, err := u.Create("TestNote", objectdb.Object{
				"subject": json.RawMessage(fmt.Sprintf("%q", n.subject)),
				"flagged": json.RawMessage(fmt.Sprintf("%t", n.flagged)),
				"body":    json.RawMessage(fmt.Sprintf("%q", n.body)),
			})
			if err != nil {
				return err
			}
			n.id = string(id)
			corpus = append(corpus, n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return &stdType{db: db, t: testNoteType(), core: DefaultCoreCapabilities()}, corpus
}

// FuzzQueryPlan drives the planner with trees derived from fuzz input and
// asserts the narrow-then-verify invariant directly: the verified result
// equals the brute-force oracle, and whenever the tree is reported exact,
// the raw candidate set (what the fast path trusts without re-verifying)
// already equals the oracle.
func FuzzQueryPlan(f *testing.F) {
	f.Add(int64(1))
	f.Add(int64(42))
	f.Add(int64(1 << 20))
	st, corpus := planDB(f)
	ctx := context.Background()
	f.Fuzz(func(t *testing.T, seed int64) {
		tree := genTree(rand.New(rand.NewSource(seed)), 3)
		root, errType, _ := parseFilter(st.t, nil, json.RawMessage(tree.json()))
		if errType != "" {
			t.Fatalf("parse: %s", errType)
		}
		var want []string
		for _, n := range corpus {
			if tree.eval(n) {
				want = append(want, n.id)
			}
		}
		sort.Strings(want)

		set, exact, narrowed, err := st.candidateSet(ctx, "Atest1", root)
		if err != nil {
			t.Fatal(err)
		}
		candidates := set
		if !narrowed {
			candidates, err = st.db.AllIds(ctx, "Atest1", st.t.Name, 0)
			if err != nil {
				t.Fatal(err)
			}
		}
		matched, err := st.loadAndMatch(ctx, "Atest1", root, candidates)
		if err != nil {
			t.Fatal(err)
		}
		gotIds := make([]string, len(matched))
		for i, m := range matched {
			gotIds[i] = string(m.Id)
		}
		sort.Strings(gotIds)
		if !equalStrs(gotIds, want) {
			t.Fatalf("verified %v, want %v (filter %s)", gotIds, want, tree.json())
		}
		if narrowed && exact {
			fast := make([]string, len(set))
			for i, id := range set {
				fast[i] = string(id)
			}
			sort.Strings(fast)
			if !equalStrs(fast, want) {
				t.Fatalf("exact fast set %v != oracle %v (filter %s)", fast, want, tree.json())
			}
		}
	})
}

// TestQueryProduceFlags white-boxes the exact/narrowed flags the fast path
// depends on: an Indexed-equality leaf is exact; AND/OR of exact leaves stay
// exact; NOT, a non-indexed condition, and a mixed leaf are not.
func TestQueryProduceFlags(t *testing.T) {
	ctx := context.Background()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	if err := db.RegisterType(testNoteType()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Update(ctx, "Atest1", func(u *objectdb.Update) error {
		for _, s := range []string{"s0", "s1", "s2"} {
			if _, err := u.Create("TestNote", objectdb.Object{
				"subject": json.RawMessage(fmt.Sprintf("%q", s)),
				"flagged": json.RawMessage("true"),
				"body":    json.RawMessage(`"b0"`),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st := &stdType{db: db, t: testNoteType(), core: DefaultCoreCapabilities()}

	cases := []struct {
		name            string
		filter          string
		exact, narrowed bool
	}{
		{"indexed leaf", `{"subject":"s1"}`, true, true},
		{"indexed two-key leaf", `{"subject":"s1","flagged":true}`, true, true},
		{"AND of indexed", `{"operator":"AND","conditions":[{"subject":"s1"},{"flagged":true}]}`, true, true},
		{"OR of indexed", `{"operator":"OR","conditions":[{"subject":"s1"},{"subject":"s2"}]}`, true, true},
		{"NOT is universe", `{"operator":"NOT","conditions":[{"subject":"s1"}]}`, false, false},
		{"non-indexed is universe", `{"body":"b0"}`, false, false},
		{"mixed leaf narrows, inexact", `{"subject":"s1","body":"b0"}`, false, true},
		{"AND with universe branch narrows", `{"operator":"AND","conditions":[{"subject":"s1"},{"body":"b0"}]}`, false, true},
		{"OR with universe branch is universe", `{"operator":"OR","conditions":[{"subject":"s1"},{"body":"b0"}]}`, false, false},
	}
	for _, c := range cases {
		root, errType, _ := parseFilter(st.t, nil, json.RawMessage(c.filter))
		if errType != "" {
			t.Fatalf("%s: parse: %s", c.name, errType)
		}
		_, exact, narrowed, err := st.candidateSet(ctx, "Atest1", root)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if exact != c.exact || narrowed != c.narrowed {
			t.Errorf("%s: exact=%v narrowed=%v, want exact=%v narrowed=%v", c.name, exact, narrowed, c.exact, c.narrowed)
		}
	}
}

// ---- core collapse (collapseThreads) on a synthetic grouping type ----

func groupType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "TestGroup",
		Capability: "urn:example:testnote",
		Properties: map[string]descriptor.Property{
			"group": {Kind: descriptor.KindString, Indexed: true},
			"rank":  {Kind: descriptor.KindUnsignedInt},
		},
	}
}

func groupServer(t *testing.T) *httptest.Server {
	t.Helper()
	a := auth.NewStatic()
	a.AddUser("john@example.com", "secret", "Atest1")
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	p := NewProcessor()
	ext := &Extensions{Query: &QueryHooks{CollapseKey: "group"}}
	if err := RegisterStandardTypeExt(p, db, groupType(), DefaultCoreCapabilities(), ext); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(a, p, "https://jmap.example.com", DefaultCoreCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability("urn:example:testnote", struct{}{}, struct{}{}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// TestQueryCollapseByKey checks collapseThreads keeps the first record of
// each grouping-key value in sorted order, and changes the total.
func TestQueryCollapseByKey(t *testing.T) {
	ts := groupServer(t)
	mk := func(cid, group string, rank int) {
		callAPICap(t, ts, "urn:example:testnote", inv("TestGroup/set",
			fmt.Sprintf(`{"accountId":"Atest1","create":{%q:{"group":%q,"rank":%d}}}`, cid, group, rank), "0"))
	}
	// Two groups; within each, two members.
	mk("a", "g1", 0)
	mk("b", "g1", 1)
	mk("c", "g2", 0)
	mk("d", "g2", 1)

	sortByRank := `,"sort":[{"property":"rank"}]`
	full := queryGroup(t, ts, `{"accountId":"Atest1"`+sortByRank+`,"calculateTotal":true}`)
	if total := int(full["total"].(float64)); total != 4 {
		t.Fatalf("uncollapsed total %d, want 4", total)
	}
	col := queryGroup(t, ts, `{"accountId":"Atest1"`+sortByRank+`,"collapseThreads":true,"calculateTotal":true}`)
	ids := toStrs(col["ids"])
	if len(ids) != 2 {
		t.Fatalf("collapsed ids %v, want 2 (one per group)", ids)
	}
	if total := int(col["total"].(float64)); total != 2 {
		t.Fatalf("collapsed total %d, want 2", total)
	}
}

// TestQueryCollapseRejectedWithoutKey: a type that declares no grouping key
// rejects collapseThreads as an unknown argument.
func TestQueryCollapseRejectedWithoutKey(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	got := queryNotes(t, ts, `{"accountId":"Atest1","collapseThreads":true}`)
	if got["type"] != jmap.ErrInvalidArguments {
		t.Fatalf("want invalidArguments, got %v", got)
	}
}

func queryGroup(t *testing.T, ts *httptest.Server, args string) map[string]any {
	t.Helper()
	resp := callAPICap(t, ts, "urn:example:testnote", inv("TestGroup/query", args, "0"))
	return methodArgs(t, resp, 0, "TestGroup/query")
}

// ---- helpers ----

func toStrs(v any) []string {
	if v == nil {
		return nil
	}
	list := v.([]any)
	out := make([]string, len(list))
	for i, x := range list {
		out[i] = x.(string)
	}
	return out
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func callAPICap(t *testing.T, ts *httptest.Server, cap string, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	req := map[string]any{
		"using":       []string{jmap.CoreCapability, cap},
		"methodCalls": calls,
	}
	body, _ := json.Marshal(req)
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
