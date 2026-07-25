package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// CheckRecordLocal verifies a type's record-local query declarations
// (QueryHooks.LocalConditions and LocalSorts) against its live
// semantics. A declaration claims that a condition's verdict and a sort
// comparator's ordering for a record depend on nothing but that
// record's own data - the property Foo/queryChanges (RFC 8620 section
// 5.6) builds its answer on. The core cannot see inside a Filter or
// Sort hook, so a false declaration is the one way an embedder can make
// queryChanges corrupt a client's cached list; this checker is how a
// datatype's own tests prove their declarations true, by construction
// of the experiment the declaration promises to survive:
//
//   - probes are records whose verdicts are watched; perturb mutates
//     the account around them - creating, updating, and destroying
//     OTHER records - and must leave every probe untouched (verified
//     byte-for-byte, so a leaky perturb fails loudly rather than
//     proving nothing);
//   - every declared condition name must come with at least one sample
//     value in conditions, and every declared sort property with at
//     least one raw Comparator in sorts - an unexercised declaration is
//     an unproven one, and fails;
//   - each probe's verdict for each condition sample, and the relative
//     order of each probe pair under each sort sample, is evaluated
//     before and after perturb; any difference means the declaration is
//     false, and the returned error names it.
//
// The checker proves sibling-independence, which is what tier-1 answers
// and thread-style group expansion rest on. It cannot prove a reads
// list exhaustive (that a condition reads only the properties it
// enumerates); a type whose declarations carry reads lists - the upToId
// and immutable-diet eligibility input - should additionally pin those
// with tests that mutate the probe's other properties.
//
// tb pins the checker to test code: its pairwise sort verification is
// O(probes^2) with no work budget, which is fine inside a conformance
// test and nothing else. Requiring a testing.TB makes wiring it to
// request-time input impossible by construction rather than by advice.
// Failures still come back as the error return (never tb.Fatal), so a
// test can assert that a deliberately false declaration is caught.
func CheckRecordLocal(
	tb testing.TB,
	ctx context.Context,
	db *objectdb.DB,
	q *QueryHooks,
	acct jmap.Id,
	typeName string,
	probes []jmap.Id,
	conditions map[string][]json.RawMessage,
	sorts map[string][]json.RawMessage,
	perturb func() error,
) error {
	tb.Helper()
	if q == nil {
		return fmt.Errorf("runtime: CheckRecordLocal: no QueryHooks")
	}
	if len(probes) == 0 {
		return fmt.Errorf("runtime: CheckRecordLocal: no probe records")
	}
	for name := range q.LocalConditions {
		if len(conditions[name]) == 0 {
			return fmt.Errorf("runtime: CheckRecordLocal: declared condition %q has no sample values", name)
		}
	}
	for name := range q.LocalSorts {
		if len(sorts[name]) == 0 {
			return fmt.Errorf("runtime: CheckRecordLocal: declared sort %q has no sample comparators", name)
		}
	}

	load := func() (map[jmap.Id]objectdb.Object, error) {
		objs := make(map[jmap.Id]objectdb.Object, len(probes))
		for _, id := range probes {
			obj, err := db.Get(ctx, acct, typeName, id)
			if err != nil {
				return nil, fmt.Errorf("runtime: CheckRecordLocal: probe %s: %w", id, err)
			}
			objs[id] = obj
		}
		return objs, nil
	}
	// verdicts evaluates every (condition sample, probe) match and every
	// (sort sample, probe pair) ordering.
	type key struct {
		name   string
		sample int
		a, b   jmap.Id // b empty for a condition verdict
	}
	verdicts := func(objs map[jmap.Id]objectdb.Object) (map[key]int, error) {
		out := make(map[key]int)
		scoper, _ := q.Filter.(RecordScoper)
		for name := range q.LocalConditions {
			for si, sample := range conditions[name] {
				for _, id := range probes {
					rctx := ctx
					if scoper != nil {
						rctx = scoper.EnterRecord(ctx)
					}
					ok, err := q.Filter.MatchCondition(rctx, acct, objs[id], name, sample)
					if err != nil {
						return nil, fmt.Errorf("runtime: CheckRecordLocal: condition %q sample %d on %s: %w", name, si, id, err)
					}
					v := 0
					if ok {
						v = 1
					}
					out[key{name, si, id, ""}] = v
				}
			}
		}
		for name := range q.LocalSorts {
			for si, sample := range sorts[name] {
				less, errType, desc := q.Sort.ParseSort(ctx, acct, []json.RawMessage{sample})
				if errType != "" {
					return nil, fmt.Errorf("runtime: CheckRecordLocal: sort %q sample %d: %s %s", name, si, errType, desc)
				}
				cmp := withIdTiebreak(less)
				for _, a := range probes {
					for _, b := range probes {
						if a >= b {
							continue
						}
						out[key{name, si, a, b}] = sign(cmp(objs[a], objs[b]))
					}
				}
			}
		}
		return out, nil
	}

	before, err := load()
	if err != nil {
		return err
	}
	baseline, err := verdicts(before)
	if err != nil {
		return err
	}
	if err := perturb(); err != nil {
		return fmt.Errorf("runtime: CheckRecordLocal: perturb: %w", err)
	}
	after, err := load()
	if err != nil {
		return err
	}
	for _, id := range probes {
		if !objectsEqual(before[id], after[id]) {
			return fmt.Errorf("runtime: CheckRecordLocal: perturb mutated probe %s, so the experiment proves nothing", id)
		}
	}
	got, err := verdicts(after)
	if err != nil {
		return err
	}
	for k, want := range baseline {
		if got[k] != want {
			if k.b == "" {
				return fmt.Errorf("runtime: CheckRecordLocal: condition %q verdict for %s moved when only other records changed: the record-local declaration is false", k.name, k.a)
			}
			return fmt.Errorf("runtime: CheckRecordLocal: sort %q order of (%s, %s) moved when only other records changed: the record-local declaration is false", k.name, k.a, k.b)
		}
	}
	return nil
}

func sign(v int) int {
	switch {
	case v < 0:
		return -1
	case v > 0:
		return 1
	}
	return 0
}

func objectsEqual(a, b objectdb.Object) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}
