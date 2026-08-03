package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// Foo/query (RFC 8620 section 5.5), initial edition: the filter language
// derived from a descriptor is property equality (a FilterCondition's
// keys are declared property names, matched under the type's comparison
// rules) composed with AND/OR/NOT. The planner is rule-based and dumb:
// a condition on an indexed property becomes an index range scan for
// the candidate set; everything else evaluates in memory over the
// loaded records. canCalculateChanges is per-query truth (5.5 defines it
// per filter/sort combination): true exactly when the query is record-
// local (see changeCalculable). Foo/queryChanges (5.6) currently still
// answers cannotCalculateChanges after validating its arguments - always
// a legal response - and a client re-runs the query as the spec directs.

type queryArgs struct {
	AccountId      jmap.Id           `json:"accountId"`
	Filter         json.RawMessage   `json:"filter"`
	Sort           []json.RawMessage `json:"sort"`
	Position       int64             `json:"position"`
	Anchor         *jmap.Id          `json:"anchor"`
	AnchorOffset   int64             `json:"anchorOffset"`
	Limit          *int64            `json:"limit"`
	CalculateTotal bool              `json:"calculateTotal"`
	// CollapseThreads is accepted only for a type that declares a
	// Query.CollapseKey (RFC 8621 Email); a pointer so its presence can be
	// rejected on any other type.
	CollapseThreads *bool `json:"collapseThreads"`
}

type queryResponse struct {
	AccountId           jmap.Id   `json:"accountId"`
	QueryState          string    `json:"queryState"`
	CanCalculateChanges bool      `json:"canCalculateChanges"`
	Position            int64     `json:"position"`
	Ids                 []jmap.Id `json:"ids"`
	// Total appears only when calculateTotal was requested (5.5).
	Total *int64 `json:"total,omitzero"`
	// Limit appears only when the server set or changed the limit (5.5).
	Limit *int64 `json:"limit,omitzero"`
}

func (st *stdType) query(ctx context.Context, call *Call) []jmap.Invocation {
	var a queryArgs
	extra, err := st.decodeWithExtras("query", call.Args, &a)
	if err != nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := checkAccount(call, a.AccountId, false); errType != "" {
		return fail(call.CallID, errType, desc)
	}
	// collapseThreads is accepted only for a type that declares a grouping
	// key (RFC 8621 section 4.4.3); any other type rejects it as unknown.
	collapseKey := st.collapseKey()
	if a.CollapseThreads != nil && collapseKey == "" {
		return fail(call.CallID, jmap.ErrInvalidArguments, "unknown argument collapseThreads")
	}
	collapse := a.CollapseThreads != nil && *a.CollapseThreads

	root, errType, desc := parseFilter(st.t, st.filterSemantics(), a.Filter)
	if errType != "" {
		return fail(call.CallID, errType, desc)
	}
	compare, errType, desc := st.buildCompare(ctx, a.AccountId, a.Sort)
	if errType != "" {
		return fail(call.CallID, errType, desc)
	}
	// A negative limit MUST be rejected (5.5); the server always enforces
	// a cap so an unbounded query cannot be asked for by omission.
	// MaxObjectsInGet is reused as the cap: a /query window larger than
	// what /get will hand over is never useful.
	serverCap := st.core.MaxObjectsInGet
	if a.Limit != nil && *a.Limit < 0 {
		return fail(call.CallID, jmap.ErrInvalidArguments, "limit must not be negative")
	}
	limit := serverCap
	limitChanged := a.Limit == nil || *a.Limit > serverCap
	if !limitChanged {
		limit = *a.Limit
	}

	results, err := st.evaluate(ctx, a.AccountId, root, compare, len(a.Sort) == 0, collapse, extra, nil, st.queryProjection(root, a.Sort, collapse))
	if err != nil {
		return fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	total := int64(len(results))

	// Anchor wins over position; a missing anchor rejects the call (5.5).
	position := a.Position
	if a.Anchor != nil {
		idx := int64(-1)
		for i, id := range results {
			if id == *a.Anchor {
				idx = int64(i)
				break
			}
		}
		if idx < 0 {
			return fail(call.CallID, jmap.ErrAnchorNotFound, "")
		}
		position = idx + a.AnchorOffset
	} else if position < 0 {
		// Negative position counts from the end, clamped to 0 (5.5).
		position += total
	}
	if position < 0 {
		position = 0
	}

	windowEnd := position + limit
	if position > total {
		windowEnd = position
	} else if windowEnd > total {
		windowEnd = total
	}
	ids := []jmap.Id{}
	if position < total {
		ids = results[position:windowEnd]
	}

	// The query state is a commit sequence number, not a fingerprint of
	// the result list: section 5.5 requires the string to change whenever
	// the results change - any such change is a commit that advances the
	// number - and explicitly permits it to change when something changed
	// on the server without affecting the results. Unlike a result hash,
	// the number tells Foo/queryChanges exactly how far behind a client
	// is before doing any work.
	queryState, err := st.queryStateOf(ctx, a.AccountId)
	if err != nil {
		return fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	resp := queryResponse{
		AccountId:           a.AccountId,
		QueryState:          queryState,
		CanCalculateChanges: st.changeCalculable(root, a.Sort, collapse, len(extra) > 0),
		Position:            position,
		Ids:                 ids,
	}
	if a.CalculateTotal {
		resp.Total = &total
	}
	if limitChanged {
		resp.Limit = &limit
	}
	return reply(st.t.Name+"/query", call.CallID, resp)
}

// Foo/queryChanges (RFC 8620 section 5.6): update a cached query result
// to the current state by reporting removed ids and added (id, index)
// pairs the client splices into its cached list.
//
// The answer leans on two section 5.6 latitudes. First, removed may be
// a superset: "the server MAY return the ids of extra Foos in addition
// that may have been in the old results". Second, refusing with
// cannotCalculateChanges is always legal and always safe - the client's
// mandated fallback is re-running the query, which costs about what a
// full recalculation here would have. So the implementation is built to
// be never-wrong rather than always-complete: every uncertain path
// refuses or over-reports removed, and only provable additions are
// reported with positions.
//
// The correctness core: for a record-local query (the only kind that
// reaches this code; see changeCalculable), an id's membership or
// position in the results can only change if that record itself changed
// - so the coalesced changed ids since the client's state cover every
// possible difference between the old and new results. removed = the
// changed ids (minus creations, which the old results never held) is
// then a legal superset, and added = the changed ids present in the
// current results, with their positions - which by 5.6's own wording
// must include still-present ids from removed ("due to a filter or sort
// based upon a mutable property"), and does so here by construction.
// For a collapsed query the same argument holds after expanding every
// changed group to its members: an untouched record's standing can only
// move when a group sibling changed, and group membership changes -
// including destroys, whose own records are gone from the log's view -
// appear as companion-record updates in the same account log.
type queryChangesArgs struct {
	AccountId       jmap.Id           `json:"accountId"`
	Filter          json.RawMessage   `json:"filter"`
	Sort            []json.RawMessage `json:"sort"`
	SinceQueryState *string           `json:"sinceQueryState"`
	MaxChanges      *int64            `json:"maxChanges"`
	UpToId          *jmap.Id          `json:"upToId"`
	CalculateTotal  bool              `json:"calculateTotal"`
	// CollapseThreads mirrors the /query argument (RFC 8621 section 4.5);
	// accepted only for a type that declares a Query.CollapseKey.
	CollapseThreads *bool `json:"collapseThreads"`
}

type queryChangesResponse struct {
	AccountId     jmap.Id   `json:"accountId"`
	OldQueryState string    `json:"oldQueryState"`
	NewQueryState string    `json:"newQueryState"`
	Total         *int64    `json:"total,omitzero"`
	Removed       []jmap.Id `json:"removed"`
	// Added is sorted by index, lowest first (5.6).
	Added []addedItem `json:"added"`
}

// addedItem is the section 5.6 AddedItem: an id and its absolute index
// in the new results.
type addedItem struct {
	Id    jmap.Id `json:"id"`
	Index int64   `json:"index"`
}

func (st *stdType) queryChanges(ctx context.Context, call *Call) []jmap.Invocation {
	var a queryChangesArgs
	if err := decodeArgs(call.Args, &a); err != nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := checkAccount(call, a.AccountId, false); errType != "" {
		return fail(call.CallID, errType, desc)
	}
	if a.CollapseThreads != nil && st.collapseKey() == "" {
		return fail(call.CallID, jmap.ErrInvalidArguments, "unknown argument collapseThreads")
	}
	collapse := a.CollapseThreads != nil && *a.CollapseThreads
	if a.SinceQueryState == nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, "sinceQueryState is required")
	}
	// maxChanges is an UnsignedInt (5.6); unlike /changes, zero is not
	// excluded by the spec text.
	if a.MaxChanges != nil && (*a.MaxChanges < 0 || !jmap.ValidUnsignedInt(*a.MaxChanges)) {
		return fail(call.CallID, jmap.ErrInvalidArguments, "maxChanges must be an UnsignedInt")
	}
	root, errType, desc := parseFilter(st.t, st.filterSemantics(), a.Filter)
	if errType != "" {
		return fail(call.CallID, errType, desc)
	}
	// Sort validation goes through the same path as /query, so a type's
	// SortSemantics answers identically on both methods.
	compare, errType, desc := st.buildCompare(ctx, a.AccountId, a.Sort)
	if errType != "" {
		return fail(call.CallID, errType, desc)
	}
	// A query /query answers canCalculateChanges: false for gets the
	// matching refusal here; the two verdicts are one function.
	if !st.changeCalculable(root, a.Sort, collapse, false) {
		return fail(call.CallID, jmap.ErrCannotCalculateChanges, "")
	}

	// The gates: three O(1) decisions before any real work. A state that
	// is not one of this server's commit numbers, or is ahead of the
	// account, was never issued (5.6: "cannot calculate the changes from
	// the queryState string"); how far behind a valid one is, is plain
	// subtraction, checked inside ChangedSince against the work budget.
	since, err := strconv.ParseInt(*a.SinceQueryState, 10, 64)
	if err != nil || since < 0 {
		return fail(call.CallID, jmap.ErrCannotCalculateChanges, "")
	}
	global, err := st.db.Sequence(ctx, a.AccountId)
	if err != nil {
		return fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	if since > global {
		return fail(call.CallID, jmap.ErrCannotCalculateChanges, "")
	}

	// Everything from the state read to the answer runs as one attempt:
	// the diff window and the evaluation must describe the same
	// sequence, and without backend snapshots the only way to know they
	// did is to bracket the work with sequence reads and redo it when a
	// commit slipped in between (the check before the reply below).
	for attempt := 0; ; attempt++ {

		// newQueryState is read BEFORE the log walk. The walk clamps at the
		// sequence it reads on entry, which is at or past this one, so every
		// change up to newQueryState is fully covered by the diff; commits
		// racing in behind it are reported again on the client's next call
		// (the removed superset makes re-reporting harmless), never lost.
		newState, err := st.queryStateOf(ctx, a.AccountId)
		if err != nil {
			return fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		newNum, err := strconv.ParseInt(newState, 10, 64)
		if err != nil {
			return fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		resp := queryChangesResponse{
			AccountId:     a.AccountId,
			OldQueryState: *a.SinceQueryState,
			NewQueryState: newState,
			Removed:       []jmap.Id{},
			Added:         []addedItem{},
		}

		// Tier 0: no commit this query can see landed after the client's
		// number - the diff is empty, in O(1). The trim floor is irrelevant
		// here: any trimmed entries described other types' changes, or this
		// state number would be higher. calculateTotal forfeits the shortcut
		// (the total requires an evaluation, 5.6 makes it opt-in for exactly
		// that reason); a state between newQueryState and the account
		// sequence can only come from a foreign state string, and answers
		// empty here rather than pretending to compute from it.
		if newNum <= since && !a.CalculateTotal {
			return reply(st.t.Name+"/queryChanges", call.CallID, resp)
		}

		// One work budget covers the whole answer: commits walked, changed
		// ids held, and group members expanded. Exceeding it refuses before
		// the expensive work happens - the client's re-run costs about the
		// same as the answer the budget disallowed.
		budget := tuning.QueryChangesMaxWork
		q := st.queryHooks()
		types := []string{st.t.Name}
		if collapse {
			types = append(types, q.GroupCompanion)
		}
		changed, upTo, err := st.db.ChangedSince(ctx, a.AccountId, types, since, budget, budget)
		if errors.Is(err, objectdb.ErrCannotCalculateChanges) {
			return fail(call.CallID, jmap.ErrCannotCalculateChanges, "")
		}
		if err != nil {
			return fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		primary := changed[st.t.Name]
		created := sliceToSet(primary.Created)
		updated := sliceToSet(primary.Updated)
		destroyed := sliceToSet(primary.Destroyed)

		// Collapsed queries: expand every changed group to its current
		// members through the collapse-key index. A destroyed member is
		// absent from the index, but its own destroy is in the primary sets;
		// an untouched sibling whose standing may have moved (a displaced
		// representative) is exactly what this pulls in.
		expanded := map[jmap.Id]bool{}
		if collapse {
			comp := changed[q.GroupCompanion]
			spent := len(created) + len(updated) + len(destroyed)
			for _, groups := range [][]jmap.Id{comp.Created, comp.Updated, comp.Destroyed} {
				for _, gid := range groups {
					key, err := json.Marshal(string(gid))
					if err != nil {
						return fail(call.CallID, jmap.ErrServerFail, err.Error())
					}
					// budget-spent+1 bounds the scan itself: one oversized
					// group can neither be materialized past the budget nor
					// slip through it, mirroring the mid-walk refusal in
					// ChangedSince.
					members, err := st.db.IdsWhereEqual(ctx, a.AccountId, st.t.Name, q.CollapseKey, key, budget-spent+1)
					if err != nil {
						return fail(call.CallID, jmap.ErrServerFail, err.Error())
					}
					spent += len(members)
					if spent > budget {
						return fail(call.CallID, jmap.ErrCannotCalculateChanges, "")
					}
					for _, id := range members {
						expanded[id] = true
					}
				}
			}
		}

		// The immutable-query diet: when every property the filter and sort
		// read is declared Immutable, a live record can never enter, leave,
		// or move - updated ids are droppable entirely, so removed shrinks
		// to the destroys and only creations can have been added. (A
		// collapsed query stays on the general path: its representative
		// depends on which siblings exist, not only on each record's own
		// immutable properties.)
		reads, readsKnown := st.queryReads(root, a.Sort)
		immutable := readsKnown && !collapse
		if immutable {
			for _, p := range reads {
				if !st.t.Properties[p].Immutable {
					immutable = false
					break
				}
			}
		}

		removedSet := destroyed
		addCand := created
		if !immutable {
			removedSet = make(map[jmap.Id]bool, len(updated)+len(destroyed)+len(expanded))
			addCand = make(map[jmap.Id]bool, len(created)+len(updated)+len(expanded))
			for id := range destroyed {
				removedSet[id] = true
			}
			for id := range updated {
				removedSet[id] = true
				addCand[id] = true
			}
			for id := range created {
				addCand[id] = true
			}
			for id := range expanded {
				if !created[id] {
					removedSet[id] = true
				}
				addCand[id] = true
			}
		}
		resp.Removed = setToSortedIds(removedSet)
		// tooManyChanges compares every removed or added item against the
		// CLIENT's number (5.6) - and removed alone deciding it means the
		// evaluation below never runs for a client that cannot accept the
		// answer anyway. maxChanges of zero is valid on this method (unlike
		// /changes, whose section 5.2 text excludes it): any change at all
		// is then too many.
		tooMany := func(count int) bool {
			return a.MaxChanges != nil && int64(count) > *a.MaxChanges
		}
		if tooMany(len(resp.Removed)) {
			return fail(call.CallID, jmap.ErrTooManyChanges, "")
		}

		// Tier 1: predicate-check ONLY the changed records (their count is
		// already inside the budget). If none of them matches the filter
		// now, nothing was added to the results - removed already covers
		// every possible departure as a superset - and no evaluation is
		// needed at all. calculateTotal forfeits the shortcut.
		wanted := st.queryProjection(root, a.Sort, collapse)
		if !a.CalculateTotal {
			matched, err := st.loadAndMatch(ctx, a.AccountId, root, setToSortedIds(addCand), wanted)
			if err != nil {
				return fail(call.CallID, jmap.ErrServerFail, err.Error())
			}
			if len(matched) == 0 {
				return reply(st.t.Name+"/queryChanges", call.CallID, resp)
			}
		}

		// Tier 2: the full evaluation - the same pipeline /query answers
		// from, so the positions reported here are exactly the positions a
		// re-run of the query would show. This is the tier whose cost equals
		// the refetch the client would otherwise do; it is not waste.
		//
		// upToId (5.6): honored only when the whole filter and sort are
		// proven immutable - for a mutable query the spec commands ignoring
		// it. When additionally the sort is a single ascending comparator on
		// an indexed property under the default collation (so index order is
		// comparator order) and the anchor record still exists and provably
		// matches, the evaluation itself is narrowed to the ids at or before
		// the anchor's sort key: every result at or before the anchor is
		// inside that range, so positions inside it are exact, and nothing
		// past the anchor was going to be reported anyway.
		var restrict map[jmap.Id]bool
		anchored := false
		if a.UpToId != nil && immutable {
			anchored = true
			if !a.CalculateTotal {
				if prop, ok := st.narrowableSort(a.Sort); ok {
					if anchorObj, err := st.db.Get(ctx, a.AccountId, st.t.Name, *a.UpToId); err == nil {
						if matches, err := root.matches(ctx, a.AccountId, st.t, st.filterSemantics(), anchorObj); err == nil && matches {
							if val, has := anchorObj[prop]; has {
								ids, err := st.db.IdsWhereAtMost(ctx, a.AccountId, st.t.Name, prop, val, 0)
								if err != nil {
									return fail(call.CallID, jmap.ErrServerFail, err.Error())
								}
								restrict = sliceToSet(ids)
							}
						}
					}
				}
			}
		}
		results, err := st.evaluate(ctx, a.AccountId, root, compare, len(a.Sort) == 0, collapse, nil, restrict, wanted)
		if err != nil {
			return fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		if a.CalculateTotal {
			total := int64(len(results))
			resp.Total = &total
		}

		// The anchor's index bounds the report: added ids past it are
		// omitted (5.6 SHOULD, applicable because the anchor was found in
		// the results). Removed is left whole: a destroyed record's old
		// position is unprovable without its record, and 5.6's SHOULD
		// tolerates reporting it; under the immutable diet removed is
		// destroys only, so there is nothing else to trim.
		trimIdx := int64(-1)
		if anchored {
			for i, id := range results {
				if id == *a.UpToId {
					trimIdx = int64(i)
					break
				}
			}
		}
		count := len(resp.Removed)
		for i, id := range results {
			if trimIdx >= 0 && int64(i) > trimIdx {
				break
			}
			if !addCand[id] {
				continue
			}
			resp.Added = append(resp.Added, addedItem{Id: id, Index: int64(i)})
			count++
			if tooMany(count) {
				return fail(call.CallID, jmap.ErrTooManyChanges, "")
			}
		}

		// The walk was clamped at upTo, but the evaluation reads live state
		// with no snapshot: a commit landing between them can shift absolute
		// positions, and an added index (or a total) is only correct against
		// NewQueryState - 5.6's client splice has no tolerance for a wrong
		// index, unlike the removed superset. Answers carrying no positions
		// are immune and skip the read. On a detected race, one redo against
		// the moved sequence; a second collision refuses, and the client's
		// refetch then costs about what this evaluation did.
		if len(resp.Added) > 0 || a.CalculateTotal {
			after, err := st.db.Sequence(ctx, a.AccountId)
			if err != nil {
				return fail(call.CallID, jmap.ErrServerFail, err.Error())
			}
			if after > upTo {
				if attempt == 0 {
					continue
				}
				return fail(call.CallID, jmap.ErrCannotCalculateChanges, "")
			}
		}
		return reply(st.t.Name+"/queryChanges", call.CallID, resp)

	}
}

// queryReads returns the stored properties this query's filter and sort
// read, and whether that set is fully known. Core conditions and
// comparators read exactly the property they name; a type's own
// semantics read what their declaration enumerates, and a declared name
// with a nil list makes the set unknowable (still calculable, never
// immutability-proven). Only called for queries changeCalculable
// accepted, so every name has its declaration.
func (st *stdType) queryReads(root *filterNode, sortRaw []json.RawMessage) ([]string, bool) {
	var props []string
	known := true
	q := st.queryHooks()
	var walk func(n *filterNode)
	walk = func(n *filterNode) {
		if n == nil {
			return
		}
		for _, c := range n.children {
			walk(c)
		}
		for name := range n.cond {
			if q != nil && q.Filter != nil {
				reads := q.LocalConditions[name]
				if reads == nil {
					known = false
				}
				props = append(props, reads...)
				continue
			}
			props = append(props, name)
		}
	}
	walk(root)
	for _, raw := range sortRaw {
		var c struct {
			Property string `json:"property"`
		}
		if json.Unmarshal(raw, &c) != nil || c.Property == "" {
			return nil, false
		}
		if q != nil && q.Sort != nil {
			reads := q.LocalSorts[c.Property]
			if reads == nil {
				known = false
			}
			props = append(props, reads...)
			continue
		}
		props = append(props, c.Property)
	}
	return props, known
}

// narrowableSort reports whether the sort is a single ascending core
// comparator on an indexed property under the default collation - the
// shape where the property index's order IS the result order, so an
// index range read can bound the evaluation. An empty sort (pure id
// order) is not narrowed: ids have no property index to range over.
func (st *stdType) narrowableSort(sortRaw []json.RawMessage) (string, bool) {
	if len(sortRaw) != 1 {
		return "", false
	}
	if q := st.queryHooks(); q != nil && q.Sort != nil {
		return "", false // a Sort override owns its order; no index promise
	}
	var c struct {
		Property    string `json:"property"`
		IsAscending *bool  `json:"isAscending"`
		Collation   string `json:"collation"`
	}
	if json.Unmarshal(sortRaw[0], &c) != nil || c.Property == "" || c.Collation != "" {
		return "", false
	}
	if c.IsAscending != nil && !*c.IsAscending {
		return "", false
	}
	if p, declared := st.t.Properties[c.Property]; !declared || !p.Indexed {
		return "", false
	}
	return c.Property, true
}

func setToSortedIds(set map[jmap.Id]bool) []jmap.Id {
	out := make([]jmap.Id, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---- filter ----

// filterNode is the parsed filter AST: op is AND/OR/NOT for a
// FilterOperator, empty for a FilterCondition (5.5).
type filterNode struct {
	op       string
	children []*filterNode
	cond     map[string]json.RawMessage
}

// evaluate runs the full query pipeline (RFC 8620 section 5.5) and
// returns the complete ordered result ids: candidate narrowing from the
// index producers, authoritative load-and-match, stable sort, any
// collapse (RFC 8621 section 4.4.3), and any Arrange hook. It is the
// one evaluation both Foo/query and Foo/queryChanges answer from, so
// the two methods can never disagree about what the results are.
// emptySort reports the sort argument was empty (the fast path may then
// answer in id order). restrict, when non-nil, drops every candidate
// outside it before records are loaded - the queryChanges range
// narrowing under an immutable sort, where positions inside the range
// are provably unaffected by anything outside it; the fast path is
// skipped then, since restrict changes what the candidate set means.
// wanted, when non-nil, narrows what each loaded record materializes
// to the properties the query provably reads (see queryProjection);
// nil loads full records.
func (st *stdType) evaluate(ctx context.Context, acct jmap.Id, root *filterNode, compare func(a, b objectdb.Object) int, emptySort, collapse bool, extra map[string]json.RawMessage, restrict map[jmap.Id]bool, wanted map[string]bool) ([]jmap.Id, error) {
	results, err := st.evaluateWith(ctx, acct, root, compare, emptySort, collapse, extra, restrict, wanted)
	if err != nil || wanted == nil || !st.p.verifyQueryProjection {
		return results, err
	}
	// Assertion mode (Processor.VerifyQueryProjection): the projected
	// answer must equal the full-decode answer, or a reads declaration
	// omitted a property its hook reads.
	full, err := st.evaluateWith(ctx, acct, root, compare, emptySort, collapse, extra, restrict, nil)
	if err != nil {
		return nil, err
	}
	if len(results) != len(full) {
		return nil, fmt.Errorf("runtime: %s: projected query evaluation diverged from full evaluation (%d vs %d results): a LocalConditions/LocalSorts reads list omits a property its hook reads", st.t.Name, len(results), len(full))
	}
	for i := range results {
		if results[i] != full[i] {
			return nil, fmt.Errorf("runtime: %s: projected query evaluation diverged from full evaluation at index %d (%s vs %s): a LocalConditions/LocalSorts reads list omits a property its hook reads", st.t.Name, i, results[i], full[i])
		}
	}
	return results, nil
}

func (st *stdType) evaluateWith(ctx context.Context, acct jmap.Id, root *filterNode, compare func(a, b objectdb.Object) int, emptySort, collapse bool, extra map[string]json.RawMessage, restrict map[jmap.Id]bool, wanted map[string]bool) ([]jmap.Id, error) {
	// Candidate set: the filter tree composed from index producers, a
	// SUPERSET of the true matches (5.5). exact reports the set is precisely
	// the match set, so no residual predicate could drop any; narrowed
	// reports the producers narrowed at all (else scan everything).
	set, exact, narrowed, err := st.candidateSet(ctx, acct, root)
	if err != nil {
		return nil, err
	}
	if narrowed && exact && emptySort && !collapse && restrict == nil {
		// Fast path: the candidate set is exactly the match set and needs no
		// ordering beyond id order, so the ids are the answer with no record
		// loads. RFC 8621 4.4's "total is fast for a single inMailbox
		// filter" is this case. Trusting exact here without a per-record
		// predicate recheck is the ONE place the narrow-then-verify
		// invariant is waived; the metamorphic test guards it.
		return dedupSortIds(set), nil
	}
	candidates := set
	if !narrowed {
		candidates, err = st.db.AllIds(ctx, acct, st.t.Name, 0)
		if err != nil {
			return nil, err
		}
	}
	if restrict != nil {
		kept := make([]jmap.Id, 0, len(candidates))
		for _, id := range candidates {
			if restrict[id] {
				kept = append(kept, id)
			}
		}
		candidates = kept
	}
	matched, err := st.loadAndMatch(ctx, acct, root, candidates, wanted)
	if err != nil {
		return nil, err
	}
	// Ties (including the empty sort) fall back to id order, keeping the
	// full order stable between calls as 5.5 requires. compare is that
	// total order; collapse and Arrange receive records in it.
	sort.SliceStable(matched, func(i, j int) bool {
		return compare(matched[i].Obj, matched[j].Obj) < 0
	})
	// collapseThreads keeps only the first record of each grouping-key
	// value in the sorted list (RFC 8621 4.4.3); core behaviour, the
	// type supplies only the key name.
	if collapse {
		matched = collapseByKey(matched, st.collapseKey())
	}
	if q := st.queryHooks(); q != nil && q.Arrange != nil {
		return q.Arrange(ctx, acct, matched, compare, extra)
	}
	results := make([]jmap.Id, len(matched))
	for i, m := range matched {
		results[i] = m.Id
	}
	return results, nil
}

// filterSemantics returns the type's custom FilterCondition semantics,
// or nil for the core equality language.
func (st *stdType) filterSemantics() FilterSemantics {
	if st.ext == nil || st.ext.Query == nil {
		return nil
	}
	return st.ext.Query.Filter
}

// queryHooks returns the type's QueryHooks, or nil.
func (st *stdType) queryHooks() *QueryHooks {
	if st.ext == nil {
		return nil
	}
	return st.ext.Query
}

// changeCalculable reports whether Foo/queryChanges can answer for this
// exact filter/sort/collapse combination. RFC 8620 section 5.5 defines
// canCalculateChanges per query ("with these "filter"/"sort" parameters"),
// so the verdict is computed per call, not per type. The calculation is
// sound only for a record-local query (see the QueryHooks declaration
// fields): every filter condition and sort property must be record-local
// - by construction for the core language, by declaration for a type's
// own Filter/Sort semantics - a collapsed query additionally needs the
// group companion that makes sibling-driven changes visible, and a query
// carrying extra arguments is never calculable because the core cannot
// know what an extras-driven arrangement reads.
func (st *stdType) changeCalculable(root *filterNode, sortRaw []json.RawMessage, collapse, hasExtras bool) bool {
	if hasExtras {
		return false
	}
	q := st.queryHooks()
	if q == nil {
		return true // pure core language: record-local by construction
	}
	if q.Arrange != nil && !q.LocalArrange {
		return false
	}
	if collapse && q.GroupCompanion == "" {
		return false
	}
	if q.Filter != nil && !condsDeclared(root, q.LocalConditions) {
		return false
	}
	if q.Sort != nil {
		for _, raw := range sortRaw {
			var c struct {
				Property string `json:"property"`
			}
			// buildCompare already validated the comparators; a name the
			// declaration map lacks simply answers false.
			if json.Unmarshal(raw, &c) != nil {
				return false
			}
			if _, ok := q.LocalSorts[c.Property]; !ok {
				return false
			}
		}
	}
	return true
}

// condsDeclared walks the filter tree and reports whether every
// FilterCondition name appears in the declared record-local set.
func condsDeclared(n *filterNode, declared map[string][]string) bool {
	if n == nil {
		return true
	}
	if n.op != "" {
		for _, c := range n.children {
			if !condsDeclared(c, declared) {
				return false
			}
		}
		return true
	}
	for name := range n.cond {
		if _, ok := declared[name]; !ok {
			return false
		}
	}
	return true
}

// queryStateOf returns the query state: the queried type's current state
// (its last-touching commit sequence, the number space Foo/get reports;
// RFC 8620 section 5.1), joined with the group companion's when one is
// declared. A collapsed result can change through a commit that touches
// only the companion (a group member destroyed elsewhere moves the
// representative), so the state that MUST change when results change
// (section 5.5) is the later of the two; section 5.5 explicitly permits
// the extra movement this costs uncollapsed queries.
func (st *stdType) queryStateOf(ctx context.Context, acct jmap.Id) (string, error) {
	state, err := st.db.TypeState(ctx, acct, st.t.Name)
	if err != nil {
		return "", err
	}
	q := st.queryHooks()
	if q == nil || q.GroupCompanion == "" {
		return state, nil
	}
	comp, err := st.db.TypeState(ctx, acct, q.GroupCompanion)
	if err != nil {
		return "", err
	}
	a, errA := strconv.ParseInt(state, 10, 64)
	b, errB := strconv.ParseInt(comp, 10, 64)
	if errA != nil || errB != nil {
		return "", fmt.Errorf("runtime: non-numeric type state (%q, %q)", state, comp)
	}
	if b > a {
		return comp, nil
	}
	return state, nil
}

// parseFilter validates the filter argument. Structural violations
// (bad operator, missing conditions) are invalidArguments; a
// syntactically valid condition naming an undeclared property is
// unsupportedFilter (5.5). With FilterSemantics, condition leaves are
// validated by the type instead of the core equality rules.
func parseFilter(t *descriptor.Type, sem FilterSemantics, raw json.RawMessage) (*filterNode, string, string) {
	budget := tuning.MaxFilterNodes
	return parseFilterNode(t, sem, raw, &budget)
}

// parseFilterNode is parseFilter's recursion. budget is shared across the
// whole tree so its total breadth, not just its depth, is bounded; every
// node (including a null leaf) spends one unit.
func parseFilterNode(t *descriptor.Type, sem FilterSemantics, raw json.RawMessage, budget *int) (*filterNode, string, string) {
	if *budget--; *budget < 0 {
		return nil, jmap.ErrUnsupportedFilter, fmt.Sprintf("filter has more than %d nodes", tuning.MaxFilterNodes)
	}
	if raw == nil || isNull(raw) {
		return nil, "", ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, jmap.ErrInvalidArguments, "filter must be an object"
	}
	if opRaw, isOperator := m["operator"]; isOperator {
		var op string
		if err := json.Unmarshal(opRaw, &op); err != nil || (op != "AND" && op != "OR" && op != "NOT") {
			return nil, jmap.ErrInvalidArguments, `operator must be "AND", "OR", or "NOT"`
		}
		condsRaw, has := m["conditions"]
		if !has || len(m) != 2 {
			return nil, jmap.ErrInvalidArguments, "a FilterOperator has exactly operator and conditions"
		}
		var conds []json.RawMessage
		if err := json.Unmarshal(condsRaw, &conds); err != nil {
			return nil, jmap.ErrInvalidArguments, "conditions must be an array"
		}
		node := &filterNode{op: op, children: make([]*filterNode, 0, min(len(conds), tuning.MaxFilterNodes))}
		for _, c := range conds {
			child, errType, desc := parseFilterNode(t, sem, c, budget)
			if errType != "" {
				return nil, errType, desc
			}
			node.children = append(node.children, child)
		}
		return node, "", ""
	}
	// FilterCondition: type semantics when declared, else declared
	// properties matched for equality.
	for name, v := range m {
		if sem != nil {
			if err := sem.ValidateCondition(name, v); err != nil {
				var unsup UnsupportedFilterError
				if errors.As(err, &unsup) {
					return nil, jmap.ErrUnsupportedFilter, unsup.Description
				}
				return nil, jmap.ErrInvalidArguments, fmt.Sprintf("filter condition %q: %v", name, err)
			}
			continue
		}
		p, declared := t.Properties[name]
		if !declared || p.Internal {
			return nil, jmap.ErrUnsupportedFilter, fmt.Sprintf("cannot filter on %q", name)
		}
		if err := p.CheckValue(v); err != nil {
			return nil, jmap.ErrInvalidArguments, fmt.Sprintf("filter condition %q: %v", name, err)
		}
	}
	return &filterNode{cond: m}, "", ""
}

// matches is the authoritative predicate over a loaded record (RFC 8620
// section 5.5). It takes ctx and acct because a type's MatchCondition may
// need I/O (RFC 8621 Email's thread-keyword and text conditions read other
// records and the message blob); the core equality path never uses them.
func (n *filterNode) matches(ctx context.Context, acct jmap.Id, t *descriptor.Type, sem FilterSemantics, obj objectdb.Object) (bool, error) {
	if n == nil {
		return true, nil
	}
	switch n.op {
	case "AND":
		for _, c := range n.children {
			ok, err := c.matches(ctx, acct, t, sem, obj)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	case "OR":
		for _, c := range n.children {
			ok, err := c.matches(ctx, acct, t, sem, obj)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
		return false, nil
	case "NOT":
		for _, c := range n.children {
			ok, err := c.matches(ctx, acct, t, sem, obj)
			if err != nil {
				return false, err
			}
			if ok {
				return false, nil
			}
		}
		return true, nil
	}
	for name, want := range n.cond {
		if sem != nil {
			ok, err := sem.MatchCondition(ctx, acct, obj, name, want)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
			continue
		}
		got, has := obj[name]
		if !has {
			return false, nil
		}
		p := t.Properties[name]
		wk, err1 := objectdb.SortKey(p, want)
		gk, err2 := objectdb.SortKey(p, got)
		if err1 != nil || err2 != nil || !bytes.Equal(wk, gk) {
			return false, nil
		}
	}
	return true, nil
}

// ---- candidate set (the index planner) ----
//
// The RFC 8620 section 5.5 filter is a FilterOperator (AND/OR/NOT) tree
// over FilterConditions. Each condition is evaluated two ways: as a set
// producer (an index-backed SUPERSET of matching ids, or "universe" when it
// cannot narrow) and as a predicate (matches, above, the authoritative
// section 5.5 comparison). candidateSet composes the producers with the
// tree operators into a superset of the true matches; loadAndMatch then
// verifies every candidate with the predicate tree. A producer only ever
// narrows - it is a hint, never authoritative - which is what keeps the
// optimization safe under an imperfect index (the invariant the metamorphic
// test pins down). The one exception is the fast path in query(), which
// trusts an exact set without re-verifying, and only when the whole tree is
// exact (see RFC 8621 section 4.4's fast-"total" expectation).

// candidateSet composes the filter tree into a candidate id set. narrowed
// is false when the producers could not narrow at all (scan everything);
// exact is true when the set is precisely the match set (no residual
// predicate could drop any), which - with no sort and no collapse - lets
// the caller answer without loading records.
func (st *stdType) candidateSet(ctx context.Context, acct jmap.Id, root *filterNode) (set []jmap.Id, exact, narrowed bool, err error) {
	ids, ex, ok, err := st.produce(ctx, acct, root)
	if err != nil || !ok {
		return nil, false, false, err
	}
	out := make([]jmap.Id, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	return out, ex, true, nil
}

// produce evaluates one filter node to an id set, mapping the RFC 8620
// section 5.5 FilterOperator semantics onto set algebra. ok=false means the
// node is the universe (could match anything the index cannot rule out);
// the planner then falls back to a full scan for it. AND intersects, OR
// unions (any universe branch makes the union the universe), NOT is always
// the universe (a real complement costs a full scan). exact tracks whether
// the returned set is precisely the match set for the node.
func (st *stdType) produce(ctx context.Context, acct jmap.Id, n *filterNode) (map[jmap.Id]bool, bool, bool, error) {
	if n == nil {
		return nil, false, false, nil // no filter: matches all -> scan
	}
	switch n.op {
	case "AND":
		var acc map[jmap.Id]bool
		exact, any := true, false
		for _, c := range n.children {
			ids, ex, ok, err := st.produce(ctx, acct, c)
			if err != nil {
				return nil, false, false, err
			}
			if !ok {
				exact = false // a universe branch: the intersection is a superset
				continue
			}
			if !any {
				acc, any = cloneSet(ids), true
			} else {
				acc = intersectSets(acc, ids)
			}
			if !ex {
				exact = false
			}
		}
		if !any {
			return nil, false, false, nil // every branch was the universe
		}
		return acc, exact, true, nil
	case "OR":
		acc := map[jmap.Id]bool{}
		exact := true
		for _, c := range n.children {
			ids, ex, ok, err := st.produce(ctx, acct, c)
			if err != nil {
				return nil, false, false, err
			}
			if !ok {
				return nil, false, false, nil // a universe branch: OR is the universe
			}
			for id := range ids {
				acc[id] = true
			}
			if !ex {
				exact = false
			}
		}
		return acc, exact, true, nil
	case "NOT":
		return nil, false, false, nil // a real complement costs a full scan
	}
	return st.produceLeaf(ctx, acct, n.cond)
}

// produceLeaf composes a FilterCondition from per-condition producers. A
// condition with several properties is an implicit AND of them (RFC 8620
// section 5.5; spelled out for Email in RFC 8621 section 4.4.1), so the
// per-pair sets are intersected.
func (st *stdType) produceLeaf(ctx context.Context, acct jmap.Id, cond map[string]json.RawMessage) (map[jmap.Id]bool, bool, bool, error) {
	names := make([]string, 0, len(cond))
	for name := range cond {
		names = append(names, name)
	}
	sort.Strings(names)
	var acc map[jmap.Id]bool
	produced, exact := 0, true
	for _, name := range names {
		ids, ex, ok, err := st.produceCondition(ctx, acct, name, cond[name])
		if err != nil {
			return nil, false, false, err
		}
		if !ok {
			exact = false // this pair is not narrowed; the predicate covers it
			continue
		}
		if produced == 0 {
			acc = ids
		} else {
			acc = intersectSets(acc, ids)
		}
		produced++
		if !ex {
			exact = false
		}
	}
	if produced == 0 {
		return nil, false, false, nil // universe
	}
	return acc, exact, true, nil
}

// produceCondition resolves one condition pair to a candidate set. With
// custom FilterSemantics the type supplies it (if it implements
// ConditionSetProducer); otherwise the generic path uses an Indexed
// property's equality index (the RFC 8620 section 5.5 note that a server
// SHOULD use an index where one exists), which is always exact.
func (st *stdType) produceCondition(ctx context.Context, acct jmap.Id, name string, value json.RawMessage) (map[jmap.Id]bool, bool, bool, error) {
	if sem := st.filterSemantics(); sem != nil {
		csp, ok := sem.(ConditionSetProducer)
		if !ok {
			return nil, false, false, nil
		}
		ids, exact, ok, err := csp.ConditionSet(ctx, acct, name, value)
		if err != nil || !ok {
			return nil, false, false, err
		}
		return sliceToSet(ids), exact, true, nil
	}
	p, declared := st.t.Properties[name]
	if !declared || !p.Indexed {
		return nil, false, false, nil
	}
	// Unbounded: this feeds the /query candidate set, whose size is the
	// account's own matching records - the cost a /query answer is.
	ids, err := st.db.IdsWhereEqual(ctx, acct, st.t.Name, name, value, 0)
	if err != nil {
		return nil, false, false, err
	}
	return sliceToSet(ids), true, true, nil
}

// RecordScoper is an optional FilterSemantics extension. loadAndMatch
// calls EnterRecord once per candidate record, before evaluating that
// record's conditions, and uses the returned context for them; so a
// semantics can attach per-record state - a parsed-blob cache shared by
// the several text conditions on one record (RFC 8621 section 4.4.1) -
// that never outlives the record or crosses concurrent queries. A
// semantics needing no per-record state does not implement it.
type RecordScoper interface {
	EnterRecord(ctx context.Context) context.Context
}

// loadAndMatch loads each candidate and keeps the ones the predicate tree
// accepts (the authoritative verification of the narrowed superset).
//
// GetMany, not one Get per id: on a backend fronted by a network database
// this is one round trip for the whole candidate set instead of one per
// candidate - see objectdb.DB.GetMany and backend.MultiGetter.
func (st *stdType) loadAndMatch(ctx context.Context, acct jmap.Id, root *filterNode, ids []jmap.Id, wanted map[string]bool) ([]QueryRecord, error) {
	sem := st.filterSemantics()
	scoper, _ := sem.(RecordScoper)
	objs, err := st.db.GetManyProjected(ctx, acct, st.t.Name, ids, wanted)
	if err != nil {
		return nil, err
	}
	matched := make([]QueryRecord, 0, len(ids))
	for i, obj := range objs {
		if obj == nil {
			continue // not found: GetMany's nil-slot convention, same as Get's ErrNotFound
		}
		rctx := ctx
		if scoper != nil {
			rctx = scoper.EnterRecord(ctx)
		}
		ok, err := root.matches(rctx, acct, st.t, sem, obj)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, QueryRecord{Id: ids[i], Obj: obj})
		}
	}
	return matched, nil
}

// collapseByKey keeps only the first record of each distinct grouping-key
// value, walking the already-sorted list (RFC 8621 section 4.4.3).
func collapseByKey(matched []QueryRecord, key string) []QueryRecord {
	seen := make(map[string]bool, len(matched))
	out := make([]QueryRecord, 0, len(matched))
	for _, m := range matched {
		k := string(m.Obj[key])
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, m)
	}
	return out
}

func (st *stdType) collapseKey() string {
	if st.ext != nil && st.ext.Query != nil {
		return st.ext.Query.CollapseKey
	}
	return ""
}

func sliceToSet(ids []jmap.Id) map[jmap.Id]bool {
	set := make(map[jmap.Id]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

func cloneSet(s map[jmap.Id]bool) map[jmap.Id]bool {
	out := make(map[jmap.Id]bool, len(s))
	for id := range s {
		out[id] = true
	}
	return out
}

// intersectSets returns the ids in both sets, iterating the smaller one.
func intersectSets(a, b map[jmap.Id]bool) map[jmap.Id]bool {
	if len(b) < len(a) {
		a, b = b, a
	}
	out := make(map[jmap.Id]bool, len(a))
	for id := range a {
		if b[id] {
			out[id] = true
		}
	}
	return out
}

func dedupSortIds(ids []jmap.Id) []jmap.Id {
	out := make([]jmap.Id, len(ids))
	copy(out, ids)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	// The candidate set already carries unique ids (map-backed), so no
	// dedup pass is needed; sorting gives the stable id order the empty-sort
	// result requires (5.5).
	return out
}

// queryProjection returns the stored-property set sufficient for
// evaluate to answer this query - the union of what its conditions,
// comparators, and collapse read, always including id - or nil when
// only a full decode is provably sufficient: an Arrange hook (its
// reads are undeclared), a condition or sort a custom semantics has
// not declared, or a declaration whose reads list is unenumerated
// (nil). Projection makes a reads list load-bearing for correctness,
// not just for the section 5.6 immutability proofs: a list omitting a
// property its hook reads makes the hook see the property as absent.
// Processor.VerifyQueryProjection is the harness that catches such a
// declaration.
func (st *stdType) queryProjection(root *filterNode, sortRaw []json.RawMessage, collapse bool) map[string]bool {
	q := st.queryHooks()
	if q != nil && q.Arrange != nil {
		return nil
	}
	wanted := map[string]bool{"id": true}
	if !st.projectConds(root, wanted) {
		return nil
	}
	for _, raw := range sortRaw {
		var c struct {
			Property string `json:"property"`
		}
		// buildCompare already validated the comparators; this parse
		// cannot newly fail, but a failure simply forfeits projection.
		if json.Unmarshal(raw, &c) != nil {
			return nil
		}
		if q != nil && q.Sort != nil {
			reads, ok := q.LocalSorts[c.Property]
			if !ok || reads == nil {
				return nil
			}
			for _, r := range reads {
				wanted[r] = true
			}
			continue
		}
		// Core comparators read exactly their property (and the id
		// tiebreak, always included).
		wanted[c.Property] = true
	}
	if collapse {
		wanted[st.collapseKey()] = true
	}
	return wanted
}

// projectConds accumulates the stored properties every condition in the
// tree reads, reporting false when any condition's reads cannot be
// enumerated. A custom FilterSemantics evaluates every condition, so
// its LocalConditions declarations decide; the core equality language
// reads exactly the condition's property.
func (st *stdType) projectConds(n *filterNode, wanted map[string]bool) bool {
	if n == nil {
		return true
	}
	for _, c := range n.children {
		if !st.projectConds(c, wanted) {
			return false
		}
	}
	sem := st.filterSemantics()
	q := st.queryHooks()
	for name := range n.cond {
		if sem != nil {
			var reads []string
			ok := false
			if q != nil {
				reads, ok = q.LocalConditions[name]
			}
			if !ok || reads == nil {
				return false
			}
			for _, r := range reads {
				wanted[r] = true
			}
			continue
		}
		wanted[name] = true
	}
	return true
}

// ---- sort ----

// buildCompare returns the total-order comparison for /query results. A
// type with a Sort override owns comparator parsing and comparison
// (PROVISIONAL; see SortSemantics); otherwise the core declared-property
// comparators apply. Either way an id tiebreak is appended so equal
// records keep the stable order 5.5 requires.
func (st *stdType) buildCompare(ctx context.Context, acct jmap.Id, sortRaw []json.RawMessage) (func(a, b objectdb.Object) int, string, string) {
	if st.ext != nil && st.ext.Query != nil && st.ext.Query.Sort != nil {
		less, errType, desc := st.ext.Query.Sort.ParseSort(ctx, acct, sortRaw)
		if errType != "" {
			return nil, errType, desc
		}
		return withIdTiebreak(less), "", ""
	}
	cmps, errType, desc := parseComparators(st.t, sortRaw)
	if errType != "" {
		return nil, errType, desc
	}
	return func(a, b objectdb.Object) int {
		for _, c := range cmps {
			if r := c.compare(a, b); r != 0 {
				return r
			}
		}
		return strings.Compare(string(a["id"]), string(b["id"]))
	}, "", ""
}

// withIdTiebreak appends the id tiebreak to a type-supplied comparator; a
// nil comparator (empty sort) leaves pure id order.
func withIdTiebreak(less func(a, b objectdb.Object) int) func(a, b objectdb.Object) int {
	return func(a, b objectdb.Object) int {
		if less != nil {
			if r := less(a, b); r != 0 {
				return r
			}
		}
		return strings.Compare(string(a["id"]), string(b["id"]))
	}
}

type comparator struct {
	prop       descriptor.Property
	name       string
	descending bool
	// numeric selects the i;ascii-numeric collation on a string property
	// (RFC 4790: compare by leading decimal digits; values without any
	// sort after all numeric values, equal to each other).
	numeric bool
}

// parseComparators validates the sort argument. A Comparator may carry
// additional type-specific properties (5.5), so parsing is not strict;
// an undeclared property or unknown collation is unsupportedSort.
func parseComparators(t *descriptor.Type, raws []json.RawMessage) ([]comparator, string, string) {
	out := make([]comparator, 0, len(raws))
	for _, raw := range raws {
		var c struct {
			Property    string `json:"property"`
			IsAscending *bool  `json:"isAscending"`
			Collation   string `json:"collation"`
		}
		if err := json.Unmarshal(raw, &c); err != nil || c.Property == "" {
			return nil, jmap.ErrInvalidArguments, "each Comparator needs a property"
		}
		// Internal properties are invisible to the method layer: not
		// sortable, exactly as they are not filterable.
		p, declared := t.Properties[c.Property]
		if !declared || p.Internal {
			return nil, jmap.ErrUnsupportedSort, fmt.Sprintf("cannot sort on %q", c.Property)
		}
		cmp := comparator{prop: p, name: c.Property, descending: c.IsAscending != nil && !*c.IsAscending}
		// Collation applies only to strings; ignored otherwise (5.5).
		if p.Kind == descriptor.KindString {
			switch c.Collation {
			case "", "i;ascii-casemap":
			case "i;ascii-numeric":
				cmp.numeric = true
			default:
				return nil, jmap.ErrUnsupportedSort, fmt.Sprintf("unknown collation %q", c.Collation)
			}
		}
		out = append(out, cmp)
	}
	return out, "", ""
}

// compare returns <0, 0, >0 for a against b under this comparator.
// Records missing the property sort first (a deterministic, stable
// choice; 5.5 leaves it to the type).
func (c comparator) compare(a, b objectdb.Object) int {
	ra, hasA := a[c.name]
	rb, hasB := b[c.name]
	var r int
	switch {
	case !hasA && !hasB:
		return 0
	case !hasA:
		r = -1
	case !hasB:
		r = 1
	case c.numeric:
		r = compareASCIINumeric(ra, rb)
	default:
		ka, err1 := objectdb.SortKey(c.prop, ra)
		kb, err2 := objectdb.SortKey(c.prop, rb)
		if err1 != nil || err2 != nil {
			return 0
		}
		r = bytes.Compare(ka, kb)
	}
	if c.descending {
		return -r
	}
	return r
}

// compareASCIINumeric implements the i;ascii-numeric collation
// (RFC 4790 section 9.1) for JSON string values: order by the leading
// run of ASCII digits interpreted as a decimal integer; values with no
// leading digit compare equal to each other and greater than all
// numeric values.
func compareASCIINumeric(a, b json.RawMessage) int {
	da, okA := leadingDigits(a)
	db, okB := leadingDigits(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return 1
	case !okB:
		return -1
	}
	// Compare as unbounded integers: strip leading zeros, then longer is
	// bigger, then lexicographic.
	if len(da) != len(db) {
		if len(da) < len(db) {
			return -1
		}
		return 1
	}
	return strings.Compare(da, db)
}

func leadingDigits(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return "", false
	}
	return strings.TrimLeft(s[:i], "0"), true
}
