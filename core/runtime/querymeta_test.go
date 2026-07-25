package runtime

// The metamorphic property behind Foo/queryChanges (RFC 8620 section
// 5.6): for ANY record-local query, applying the reported removed and
// added arrays to a cached result list via the spec's client splice
// algorithm must yield exactly what re-running the query returns. The
// test drives random mutation rounds against a set of tracked queries
// and asserts that equivalence every round - the whole tier structure,
// coalescing, and position arithmetic stand or fall together here.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

func TestQueryChangesMetamorphic(t *testing.T) {
	ts := noteServer(t, DefaultCoreCapabilities())
	rng := rand.New(rand.NewSource(823549))

	// A deliberately small subject pool: heavy ties make the id
	// tiebreak load-bearing, exactly the 5.5 stable-order requirement
	// the client splice model rests on.
	subjects := []string{"alpha", "beta", "gamma", "alpha", "beta"}
	type tracked struct {
		name   string
		args   string // filter+sort JSON fragment
		cached []string
		state  string
	}
	queries := []*tracked{
		{name: "all by id", args: ``},
		{name: "subject eq", args: `"filter":{"subject":"alpha"},`},
		{name: "flagged", args: `"filter":{"flagged":true},`},
		{name: "or", args: `"filter":{"operator":"OR","conditions":[{"subject":"beta"},{"flagged":true}]},`},
		{name: "not", args: `"filter":{"operator":"NOT","conditions":[{"subject":"alpha"}]},`},
		{name: "sorted", args: `"sort":[{"property":"subject"}],`},
		{name: "sorted desc flag tie", args: `"sort":[{"property":"flagged"},{"property":"subject","isAscending":false}],`},
		{name: "filter and sort", args: `"filter":{"operator":"OR","conditions":[{"subject":"alpha"},{"subject":"gamma"}]},"sort":[{"property":"subject"}],`},
	}
	query := func(q *tracked) map[string]any {
		t.Helper()
		r := callAPI(t, ts, inv("TestNote/query",
			`{"accountId":"Atest1",`+q.args+`"limit":1000}`, "0"))
		return methodArgs(t, r, 0, "TestNote/query")
	}
	refresh := func(q *tracked) {
		t.Helper()
		res := query(q)
		q.cached = idsOf(res)
		q.state = res["queryState"].(string)
	}
	for _, q := range queries {
		refresh(q)
	}

	var live []string
	mutate := func() {
		t.Helper()
		switch op := rng.Intn(3); {
		case op == 0 || len(live) == 0: // create
			id := createNote(t, ts, fmt.Sprintf(`{"subject":%q,"flagged":%v}`,
				subjects[rng.Intn(len(subjects))], rng.Intn(2) == 0))
			live = append(live, id)
		case op == 1: // update
			id := live[rng.Intn(len(live))]
			var patch string
			switch rng.Intn(3) {
			case 0:
				patch = fmt.Sprintf(`{"subject":%q}`, subjects[rng.Intn(len(subjects))])
			case 1:
				patch = fmt.Sprintf(`{"flagged":%v}`, rng.Intn(2) == 0)
			default:
				patch = fmt.Sprintf(`{"body":"r%d"}`, rng.Int())
			}
			r := callAPI(t, ts, inv("TestNote/set", fmt.Sprintf(
				`{"accountId":"Atest1","update":{%q:%s}}`, id, patch), "0"))
			if _, ok := methodArgs(t, r, 0, "TestNote/set")["updated"].(map[string]any)[id]; !ok {
				t.Fatalf("update %s failed", id)
			}
		default: // destroy
			i := rng.Intn(len(live))
			id := live[i]
			live = append(live[:i], live[i+1:]...)
			r := callAPI(t, ts, inv("TestNote/set", fmt.Sprintf(
				`{"accountId":"Atest1","destroy":[%q]}`, id), "0"))
			if destroyed, _ := methodArgs(t, r, 0, "TestNote/set")["destroyed"].([]any); len(destroyed) != 1 {
				t.Fatalf("destroy %s failed", id)
			}
		}
	}

	const rounds = 120
	for round := 0; round < rounds; round++ {
		for n := 1 + rng.Intn(3); n > 0; n-- {
			mutate()
		}
		for _, q := range queries {
			r := callAPI(t, ts, inv("TestNote/queryChanges", fmt.Sprintf(
				`{"accountId":"Atest1",`+q.args+`"sinceQueryState":%q}`, q.state), "0"))
			resp := r.MethodResponses[0]
			if resp.Name == "error" {
				// A refusal is always legal; the mandated client fallback
				// is a refetch. (With the default budget it should not
				// happen in this test's scale - flag it if it does.)
				t.Fatalf("round %d %q: unexpected refusal %s", round, q.name, resp.Args)
			}
			c := methodArgs(t, r, 0, "TestNote/queryChanges")
			spliced := spliceApply(q.cached, anySlice(c, "removed"), anySlice(c, "added"))
			fresh := query(q)
			if want := idsOf(fresh); strings.Join(spliced, ",") != strings.Join(want, ",") {
				t.Fatalf("round %d %q since %q: splice %v != refetch %v (removed %v added %v)",
					round, q.name, q.state, spliced, want, c["removed"], c["added"])
			}
			q.cached = spliced
			q.state = c["newQueryState"].(string)
		}
	}
}

// ---- the conformance checker proves and refutes declarations ----

func conformanceDB(t *testing.T) (*objectdb.DB, []jmap.Id) {
	t.Helper()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	if err := db.RegisterType(gadgetType()); err != nil {
		t.Fatal(err)
	}
	var probes []jmap.Id
	for _, subject := range []string{"apple", "banana", "cherry"} {
		var id jmap.Id
		if _, err := db.Update(context.Background(), "Atest1", func(u *objectdb.Update) error {
			var err error
			id, err = u.Create("Gadget", objectdb.Object{"subject": json.RawMessage(fmt.Sprintf("%q", subject))})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		probes = append(probes, id)
	}
	return db, probes
}

// churn creates, updates, and destroys records other than the probes.
func churn(db *objectdb.DB) func() error {
	return func() error {
		ctx := context.Background()
		var extra jmap.Id
		if _, err := db.Update(ctx, "Atest1", func(u *objectdb.Update) error {
			var err error
			extra, err = u.Create("Gadget", objectdb.Object{"subject": json.RawMessage(`"noise"`)})
			return err
		}); err != nil {
			return err
		}
		if _, err := db.Update(ctx, "Atest1", func(u *objectdb.Update) error {
			_, err := u.Create("Gadget", objectdb.Object{"subject": json.RawMessage(`"more noise"`)})
			return err
		}); err != nil {
			return err
		}
		_, err := db.Update(ctx, "Atest1", func(u *objectdb.Update) error {
			return u.Destroy("Gadget", extra)
		})
		return err
	}
}

func TestCheckRecordLocalHonest(t *testing.T) {
	db, probes := conformanceDB(t)
	q := &QueryHooks{
		Filter:          gadgetFilter{},
		LocalConditions: map[string][]string{"subject": {"subject"}, "text": nil},
	}
	err := CheckRecordLocal(t, context.Background(), db, q, "Atest1", "Gadget", probes,
		map[string][]json.RawMessage{
			"subject": {json.RawMessage(`"apple"`), json.RawMessage(`"zzz"`)},
			"text":    {json.RawMessage(`"an"`)},
		}, nil, churn(db))
	if err != nil {
		t.Fatalf("honest declarations failed the checker: %v", err)
	}
}

// countingFilter is a deliberately false declaration: its "atLeast"
// condition matches when the account holds at least N records, a
// verdict that moves when OTHER records are created - exactly the lie
// the checker exists to catch.
type countingFilter struct {
	db *objectdb.DB
}

func (countingFilter) ValidateCondition(name string, value json.RawMessage) error { return nil }

func (f countingFilter) MatchCondition(ctx context.Context, acct jmap.Id, _ objectdb.Object, name string, value json.RawMessage) (bool, error) {
	var n int
	json.Unmarshal(value, &n)
	ids, err := f.db.AllIds(ctx, acct, "Gadget", 0)
	if err != nil {
		return false, err
	}
	return len(ids) >= n, nil
}

func TestCheckRecordLocalCatchesLiar(t *testing.T) {
	db, probes := conformanceDB(t)
	q := &QueryHooks{
		Filter:          countingFilter{db: db},
		LocalConditions: map[string][]string{"atLeast": nil},
	}
	err := CheckRecordLocal(t, context.Background(), db, q, "Atest1", "Gadget", probes,
		map[string][]json.RawMessage{"atLeast": {json.RawMessage(`4`)}},
		nil, churn(db))
	if err == nil || !strings.Contains(err.Error(), "record-local declaration is false") {
		t.Fatalf("false declaration passed the checker: %v", err)
	}
}

func TestCheckRecordLocalMisuse(t *testing.T) {
	db, probes := conformanceDB(t)
	q := &QueryHooks{
		Filter:          gadgetFilter{},
		LocalConditions: map[string][]string{"subject": {"subject"}},
	}
	samples := map[string][]json.RawMessage{"subject": {json.RawMessage(`"apple"`)}}

	// A perturb that touches a probe proves nothing and must say so.
	err := CheckRecordLocal(t, context.Background(), db, q, "Atest1", "Gadget", probes,
		samples, nil, func() error {
			_, err := db.Update(context.Background(), "Atest1", func(u *objectdb.Update) error {
				obj, err := u.Get("Gadget", probes[0])
				if err != nil {
					return err
				}
				next := make(objectdb.Object, len(obj))
				for k, v := range obj {
					next[k] = v
				}
				next["subject"] = json.RawMessage(`"mutated"`)
				return u.Put("Gadget", probes[0], next)
			})
			return err
		})
	if err == nil || !strings.Contains(err.Error(), "proves nothing") {
		t.Fatalf("probe-mutating perturb not rejected: %v", err)
	}

	// A declared name with no samples is an unproven declaration.
	err = CheckRecordLocal(t, context.Background(), db, q, "Atest1", "Gadget", probes,
		map[string][]json.RawMessage{}, nil, churn(db))
	if err == nil || !strings.Contains(err.Error(), "no sample values") {
		t.Fatalf("sampleless declaration not rejected: %v", err)
	}
}
