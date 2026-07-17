package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// Foo/query (RFC 8620 section 5.5), M0 edition: the filter language
// derived from a descriptor is property equality (a FilterCondition's
// keys are declared property names, matched under the type's comparison
// rules) composed with AND/OR/NOT. The planner is rule-based and dumb:
// a condition on an indexed property becomes an index range scan for
// the candidate set; everything else evaluates in memory over the
// loaded records. canCalculateChanges is always false: the runtime
// keeps no per-query change history, so Foo/queryChanges (5.6) answers
// cannotCalculateChanges after validating its arguments, and a client
// re-runs the query as the spec directs.

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

	// Candidate set: the filter tree composed from index producers, a
	// SUPERSET of the true matches (5.5). exact reports the set is precisely
	// the match set, so no residual predicate could drop any; narrowed
	// reports the producers narrowed at all (else scan everything).
	set, exact, narrowed, err := st.candidateSet(ctx, a.AccountId, root)
	if err != nil {
		return fail(call.CallID, jmap.ErrServerFail, err.Error())
	}

	var results []jmap.Id
	if narrowed && exact && len(a.Sort) == 0 && !collapse {
		// Fast path: the candidate set is exactly the match set and needs no
		// ordering beyond id order, so the ids are the answer with no record
		// loads. RFC 8621 4.4's "total is fast for a single inMailbox
		// filter" is this case. Trusting exact here without a per-record
		// predicate recheck is the ONE place the narrow-then-verify
		// invariant is waived; the metamorphic test guards it.
		results = dedupSortIds(set)
	} else {
		candidates := set
		if !narrowed {
			candidates, err = st.db.AllIds(ctx, a.AccountId, st.t.Name, 0)
			if err != nil {
				return fail(call.CallID, jmap.ErrServerFail, err.Error())
			}
		}
		matched, err := st.loadAndMatch(ctx, a.AccountId, root, candidates)
		if err != nil {
			return fail(call.CallID, jmap.ErrServerFail, err.Error())
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
			matched = collapseByKey(matched, collapseKey)
		}
		if st.ext != nil && st.ext.Query != nil && st.ext.Query.Arrange != nil {
			results, err = st.ext.Query.Arrange(ctx, a.AccountId, matched, compare, extra)
			if err != nil {
				return fail(call.CallID, jmap.ErrServerFail, err.Error())
			}
		} else {
			results = make([]jmap.Id, len(matched))
			for i, m := range matched {
				results[i] = m.Id
			}
		}
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

	resp := queryResponse{
		AccountId:           a.AccountId,
		QueryState:          queryStateOf(results),
		CanCalculateChanges: false,
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

// Foo/queryChanges (RFC 8620 section 5.6). /query always advertises
// canCalculateChanges: false, so a spec-following client never calls
// this; one that does anyway gets the section 5.6 answer for "the
// server cannot calculate the changes from the queryState string" -
// cannotCalculateChanges, telling it to refetch the query - rather
// than unknownMethod. Arguments are still validated in full first
// (section 3.9), with the same filter/sort rules as /query.
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
	if a.SinceQueryState == nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, "sinceQueryState is required")
	}
	// maxChanges is an UnsignedInt (5.6); unlike /changes, zero is not
	// excluded by the spec text.
	if a.MaxChanges != nil && (*a.MaxChanges < 0 || !jmap.ValidUnsignedInt(*a.MaxChanges)) {
		return fail(call.CallID, jmap.ErrInvalidArguments, "maxChanges must be an UnsignedInt")
	}
	if _, errType, desc := parseFilter(st.t, st.filterSemantics(), a.Filter); errType != "" {
		return fail(call.CallID, errType, desc)
	}
	if _, errType, desc := parseComparators(st.t, a.Sort); errType != "" {
		return fail(call.CallID, errType, desc)
	}
	return fail(call.CallID, jmap.ErrCannotCalculateChanges, "")
}

// queryStateOf derives the query state from the ordered result ids: it
// changes exactly when the matching ids or their order change (5.5).
func queryStateOf(ids []jmap.Id) string {
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// ---- filter ----

// filterNode is the parsed filter AST: op is AND/OR/NOT for a
// FilterOperator, empty for a FilterCondition (5.5).
type filterNode struct {
	op       string
	children []*filterNode
	cond     map[string]json.RawMessage
}

// filterSemantics returns the type's custom FilterCondition semantics,
// or nil for the core equality language.
func (st *stdType) filterSemantics() FilterSemantics {
	if st.ext == nil || st.ext.Query == nil {
		return nil
	}
	return st.ext.Query.Filter
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
	ids, err := st.db.IdsWhereEqual(ctx, acct, st.t.Name, name, value)
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
func (st *stdType) loadAndMatch(ctx context.Context, acct jmap.Id, root *filterNode, ids []jmap.Id) ([]QueryRecord, error) {
	sem := st.filterSemantics()
	scoper, _ := sem.(RecordScoper)
	matched := make([]QueryRecord, 0, len(ids))
	for _, id := range ids {
		obj, err := st.db.Get(ctx, acct, st.t.Name, id)
		if errors.Is(err, objectdb.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
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
			matched = append(matched, QueryRecord{Id: id, Obj: obj})
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
		p, declared := t.Properties[c.Property]
		if !declared {
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
