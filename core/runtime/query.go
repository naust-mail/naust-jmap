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
	if err := decodeArgs(call.Args, &a); err != nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := checkAccount(call, a.AccountId, false); errType != "" {
		return fail(call.CallID, errType, desc)
	}
	root, errType, desc := parseFilter(st.t, a.Filter)
	if errType != "" {
		return fail(call.CallID, errType, desc)
	}
	cmps, errType, desc := parseComparators(st.t, a.Sort)
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

	// Candidate set: an equality condition on an indexed property is
	// answered by the index; anything else starts from all records.
	var candidates []jmap.Id
	var err error
	if prop, value, ok := indexedEquality(st.t, root); ok {
		candidates, err = st.db.IdsWhereEqual(ctx, a.AccountId, st.t.Name, prop, value)
	} else {
		candidates, err = st.db.AllIds(ctx, a.AccountId, st.t.Name, 0)
	}
	if err != nil {
		return fail(call.CallID, jmap.ErrServerFail, err.Error())
	}

	type rec struct {
		id  jmap.Id
		obj objectdb.Object
	}
	matched := make([]rec, 0, len(candidates))
	for _, id := range candidates {
		obj, err := st.db.Get(ctx, a.AccountId, st.t.Name, id)
		if errors.Is(err, objectdb.ErrNotFound) {
			continue
		}
		if err != nil {
			return fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		if root.matches(st.t, obj) {
			matched = append(matched, rec{id: id, obj: obj})
		}
	}
	// Ties (including the empty sort) fall back to id order, keeping the
	// full order stable between calls as 5.5 requires.
	sort.Slice(matched, func(i, j int) bool {
		for _, c := range cmps {
			if r := c.compare(matched[i].obj, matched[j].obj); r != 0 {
				return r < 0
			}
		}
		return matched[i].id < matched[j].id
	})
	results := make([]jmap.Id, len(matched))
	for i, m := range matched {
		results[i] = m.id
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
}

func (st *stdType) queryChanges(ctx context.Context, call *Call) []jmap.Invocation {
	var a queryChangesArgs
	if err := decodeArgs(call.Args, &a); err != nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := checkAccount(call, a.AccountId, false); errType != "" {
		return fail(call.CallID, errType, desc)
	}
	if a.SinceQueryState == nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, "sinceQueryState is required")
	}
	// maxChanges is an UnsignedInt (5.6); unlike /changes, zero is not
	// excluded by the spec text.
	if a.MaxChanges != nil && (*a.MaxChanges < 0 || !jmap.ValidUnsignedInt(*a.MaxChanges)) {
		return fail(call.CallID, jmap.ErrInvalidArguments, "maxChanges must be an UnsignedInt")
	}
	if _, errType, desc := parseFilter(st.t, a.Filter); errType != "" {
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

// parseFilter validates the filter argument. Structural violations
// (bad operator, missing conditions) are invalidArguments; a
// syntactically valid condition naming an undeclared property is
// unsupportedFilter (5.5).
func parseFilter(t *descriptor.Type, raw json.RawMessage) (*filterNode, string, string) {
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
		node := &filterNode{op: op, children: make([]*filterNode, 0, len(conds))}
		for _, c := range conds {
			child, errType, desc := parseFilter(t, c)
			if errType != "" {
				return nil, errType, desc
			}
			node.children = append(node.children, child)
		}
		return node, "", ""
	}
	// FilterCondition: declared properties matched for equality.
	for name, v := range m {
		p, declared := t.Properties[name]
		if !declared {
			return nil, jmap.ErrUnsupportedFilter, fmt.Sprintf("cannot filter on %q", name)
		}
		if err := p.CheckValue(v); err != nil {
			return nil, jmap.ErrInvalidArguments, fmt.Sprintf("filter condition %q: %v", name, err)
		}
	}
	return &filterNode{cond: m}, "", ""
}

func (n *filterNode) matches(t *descriptor.Type, obj objectdb.Object) bool {
	if n == nil {
		return true
	}
	switch n.op {
	case "AND":
		for _, c := range n.children {
			if !c.matches(t, obj) {
				return false
			}
		}
		return true
	case "OR":
		for _, c := range n.children {
			if c.matches(t, obj) {
				return true
			}
		}
		return false
	case "NOT":
		for _, c := range n.children {
			if c.matches(t, obj) {
				return false
			}
		}
		return true
	}
	for name, want := range n.cond {
		got, has := obj[name]
		if !has {
			return false
		}
		p := t.Properties[name]
		wk, err1 := objectdb.SortKey(p, want)
		gk, err2 := objectdb.SortKey(p, got)
		if err1 != nil || err2 != nil || !bytes.Equal(wk, gk) {
			return false
		}
	}
	return true
}

// indexedEquality reports whether the filter is a single condition with
// an indexed property the planner can range-scan; the full condition is
// still evaluated on the loaded records, so extra properties in the
// same condition just narrow the residual.
func indexedEquality(t *descriptor.Type, n *filterNode) (string, json.RawMessage, bool) {
	if n == nil || n.op != "" {
		return "", nil, false
	}
	names := make([]string, 0, len(n.cond))
	for name := range n.cond {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if t.Properties[name].Indexed {
			return name, n.cond[name], true
		}
	}
	return "", nil, false
}

// ---- sort ----

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
