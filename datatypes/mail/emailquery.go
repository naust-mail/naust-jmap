package mail

// Email/query (RFC 8621 section 4.4): the mail FilterCondition semantics and
// the sort comparators. Text conditions delegate to the Searcher (search.go).
//
// emailFilter implements runtime.FilterSemantics (validate + predicate) and
// runtime.ConditionSetProducer (index-backed candidate sets). The planner
// composes the producers with AND/OR/NOT and then verifies every candidate
// with MatchCondition, so a producer only narrows - inMailbox and hasKeyword
// resolve from the membership index, everything else falls to the predicate
// over the loaded record. emailSort implements the PROVISIONAL
// runtime.SortSemantics for the section 4.4.2 sort keys. collapseThreads is
// core behaviour keyed on threadId (declared in RegisterEmail).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// emailSortProps are the sort Comparator "property" values Email/query
// supports (RFC 8621 section 4.4.2); also the emailQuerySortOptions
// capability value (section 1.3.1).
var emailSortProps = []string{
	"receivedAt", "size", "from", "to", "subject", "sentAt",
	"hasKeyword", "allInThreadHaveKeyword", "someInThreadHaveKeyword",
}

// threadKeywordProps are the three thread-scoped keyword conditions
// (section 4.4.1); they and the keyword sorts need the record's Thread.
var threadKeywordSorts = map[string]bool{
	"allInThreadHaveKeyword":  true,
	"someInThreadHaveKeyword": true,
}

// ---- filter ----

type emailFilter struct {
	db       *objectdb.DB
	searcher Searcher
}

// ValidateCondition checks one FilterCondition pair (section 4.4.1). An
// unknown name is unsupportedFilter; a wrong-typed value is invalidArguments.
func (emailFilter) ValidateCondition(name string, value json.RawMessage) error {
	switch name {
	case "inMailbox":
		if !validId(value) {
			return fmt.Errorf("inMailbox must be an Id")
		}
	case "inMailboxOtherThan":
		var ids []jmap.Id
		if json.Unmarshal(value, &ids) != nil {
			return fmt.Errorf("inMailboxOtherThan must be an Id[]")
		}
		for _, id := range ids {
			if !id.Valid() {
				return fmt.Errorf("inMailboxOtherThan holds an invalid Id")
			}
		}
	case "before", "after":
		if !validUTCDate(value) {
			return fmt.Errorf("%s must be a UTCDate", name)
		}
	case "minSize", "maxSize":
		if !validUnsignedInt(value) {
			return fmt.Errorf("%s must be an UnsignedInt", name)
		}
	case "allInThreadHaveKeyword", "someInThreadHaveKeyword", "noneInThreadHaveKeyword",
		"hasKeyword", "notKeyword":
		if s, ok := decodeString(value); !ok || !validKeyword(s) {
			return fmt.Errorf("%s must be a keyword String", name)
		}
	case "hasAttachment":
		var b bool
		if json.Unmarshal(value, &b) != nil {
			return fmt.Errorf("hasAttachment must be a Boolean")
		}
	case "text", "from", "to", "cc", "bcc", "subject", "body":
		if _, ok := decodeString(value); !ok {
			return fmt.Errorf("%s must be a String", name)
		}
	case "header":
		var h []string
		if json.Unmarshal(value, &h) != nil || len(h) < 1 || len(h) > 2 {
			return fmt.Errorf("header must be a String[] of one or two elements")
		}
	default:
		return runtime.UnsupportedFilterError{Description: fmt.Sprintf("cannot filter on %q", name)}
	}
	return nil
}

// EnterRecord installs a fresh per-record parse cache (runtime.RecordScoper),
// so the text conditions Email/query evaluates against one record share a
// single parse of its message blob instead of reparsing per condition.
func (emailFilter) EnterRecord(ctx context.Context) context.Context {
	return context.WithValue(ctx, parseCacheKey{}, &parseCache{})
}

// ConditionSet answers the two membership conditions from the set index;
// every other condition returns ok=false and is left to the predicate.
func (f emailFilter) ConditionSet(ctx context.Context, acct jmap.Id, name string, value json.RawMessage) ([]jmap.Id, bool, bool, error) {
	var prop, member string
	switch name {
	case "inMailbox":
		prop = "mailboxIds"
		member, _ = decodeString(value)
	case "hasKeyword":
		prop = "keywords"
		member = keywordArg(value)
	default:
		return nil, false, false, nil
	}
	ids, err := f.db.IdsWhereMember(ctx, acct, TypeEmail, prop, member)
	if err != nil {
		return nil, false, false, err
	}
	// Exact: membership in a single Mailbox / keyword is precisely the match
	// set for that condition (no residual predicate can drop any).
	return ids, true, true, nil
}

// MatchCondition is the authoritative predicate over a loaded Email
// (section 4.4.1).
func (f emailFilter) MatchCondition(ctx context.Context, acct jmap.Id, obj objectdb.Object, name string, value json.RawMessage) (bool, error) {
	switch name {
	case "inMailbox":
		id, _ := decodeString(value)
		return objectKeys(obj["mailboxIds"])[id], nil
	case "inMailboxOtherThan":
		var exclude []string
		json.Unmarshal(value, &exclude)
		ex := make(map[string]bool, len(exclude))
		for _, e := range exclude {
			ex[e] = true
		}
		for mb := range objectKeys(obj["mailboxIds"]) {
			if !ex[mb] {
				return true, nil
			}
		}
		return false, nil
	case "before":
		return emailDate(obj, "receivedAt").Before(utcDate(value)), nil
	case "after":
		return !emailDate(obj, "receivedAt").Before(utcDate(value)), nil
	case "minSize":
		return emailUint(obj, "size") >= uintVal(value), nil
	case "maxSize":
		return emailUint(obj, "size") < uintVal(value), nil
	case "hasKeyword":
		return objectKeys(obj["keywords"])[keywordArg(value)], nil
	case "notKeyword":
		return !objectKeys(obj["keywords"])[keywordArg(value)], nil
	case "hasAttachment":
		var want bool
		json.Unmarshal(value, &want)
		var got bool
		json.Unmarshal(obj["hasAttachment"], &got)
		return got == want, nil
	case "allInThreadHaveKeyword", "someInThreadHaveKeyword", "noneInThreadHaveKeyword":
		all, some, err := f.threadKeyword(ctx, acct, obj, keywordArg(value))
		if err != nil {
			return false, err
		}
		switch name {
		case "allInThreadHaveKeyword":
			return all, nil
		case "someInThreadHaveKeyword":
			return some, nil
		default:
			return !some, nil
		}
	case "text", "from", "to", "cc", "bcc", "subject", "body", "header":
		return f.searcher.Match(ctx, acct, obj, name, value)
	}
	return false, nil
}

// threadKeyword reports whether all / some Emails in obj's Thread carry the
// keyword (the section 4.4.1 thread-scoped conditions). It reads the Thread
// members through the threadId index.
func (f emailFilter) threadKeyword(ctx context.Context, acct jmap.Id, obj objectdb.Object, keyword string) (all, some bool, err error) {
	tid := threadIdOf(obj)
	ids, err := f.db.IdsWhereEqual(ctx, acct, TypeEmail, "threadId", mustJSON(tid))
	if err != nil {
		return false, false, err
	}
	all = true
	for _, id := range ids {
		member, err := f.db.Get(ctx, acct, TypeEmail, id)
		if err != nil {
			return false, false, err
		}
		if objectKeys(member["keywords"])[keyword] {
			some = true
		} else {
			all = false
		}
	}
	return all, some, nil
}

// ---- sort (PROVISIONAL runtime.SortSemantics) ----

type emailSort struct {
	db *objectdb.DB
}

// ParseSort validates the section 4.4.2 sort comparators and returns a
// total-order comparison. A keyword or thread-keyword sort MUST carry a
// "keyword" property. thread-keyword lookups are cached per (thread,
// keyword) within this query; a lookup failure there (near-impossible on a
// committed read) degrades to "keyword absent" rather than failing the sort,
// a naive limitation acceptable for the PROVISIONAL sort surface.
func (s emailSort) ParseSort(ctx context.Context, acct jmap.Id, raws []json.RawMessage) (func(a, b objectdb.Object) int, string, string) {
	type cmp struct {
		property   string
		keyword    string
		descending bool
	}
	cmps := make([]cmp, 0, len(raws))
	for _, raw := range raws {
		var c struct {
			Property    string `json:"property"`
			IsAscending *bool  `json:"isAscending"`
			Collation   string `json:"collation"`
			Keyword     string `json:"keyword"`
		}
		if json.Unmarshal(raw, &c) != nil || c.Property == "" {
			return nil, jmap.ErrInvalidArguments, "each Comparator needs a property"
		}
		if !contains(emailSortProps, c.Property) {
			return nil, jmap.ErrUnsupportedSort, fmt.Sprintf("cannot sort on %q", c.Property)
		}
		if (c.Property == "hasKeyword" || threadKeywordSorts[c.Property]) && !validKeyword(c.Keyword) {
			return nil, jmap.ErrInvalidArguments, fmt.Sprintf("sort on %q needs a keyword", c.Property)
		}
		cmps = append(cmps, cmp{property: c.Property, keyword: strings.ToLower(c.Keyword), descending: c.IsAscending != nil && !*c.IsAscending})
	}

	cache := map[string]bool{}
	threadHas := func(obj objectdb.Object, keyword string, all bool) bool {
		tid := threadIdOf(obj)
		key := string(tid) + "\x00" + keyword + boolKey(all)
		if v, ok := cache[key]; ok {
			return v
		}
		ids, err := s.db.IdsWhereEqual(ctx, acct, TypeEmail, "threadId", mustJSON(tid))
		res := all
		if err == nil {
			for _, id := range ids {
				m, err := s.db.Get(ctx, acct, TypeEmail, id)
				if err != nil {
					continue
				}
				has := objectKeys(m["keywords"])[keyword]
				if all && !has {
					res = false
				}
				if !all && has {
					res = true
				}
			}
		}
		cache[key] = res
		return res
	}

	// A comparison sort calls its comparator O(N log N) times, but each of
	// these per-record values only ever depends on the record itself: without
	// memoizing them, a Thread or Email visited by many comparisons (any N
	// large enough for the sort to matter) redecodes and recomputes its date,
	// address, subject, or keyword set from scratch every single time it is
	// compared. Caching by id turns that into one computation per record.
	dateCache := map[string]time.Time{}
	addrCache := map[string]string{}
	subjectCache := map[string]string{}
	kwCache := map[string]map[string]bool{}
	getDate := func(obj objectdb.Object, name string) time.Time {
		key := string(obj["id"]) + "\x00" + name
		if v, ok := dateCache[key]; ok {
			return v
		}
		v := emailDate(obj, name)
		dateCache[key] = v
		return v
	}
	getAddr := func(obj objectdb.Object, field string) string {
		key := string(obj["id"]) + "\x00" + field
		if v, ok := addrCache[key]; ok {
			return v
		}
		v := foldKey(firstAddr(obj, field))
		addrCache[key] = v
		return v
	}
	getSubject := func(obj objectdb.Object) string {
		id := string(obj["id"])
		if v, ok := subjectCache[id]; ok {
			return v
		}
		v := foldKey(baseSubject(storedSubject(obj)))
		subjectCache[id] = v
		return v
	}
	getKeywords := func(obj objectdb.Object) map[string]bool {
		id := string(obj["id"])
		if v, ok := kwCache[id]; ok {
			return v
		}
		v := objectKeys(obj["keywords"])
		kwCache[id] = v
		return v
	}

	return func(a, b objectdb.Object) int {
		for _, c := range cmps {
			r := 0
			switch c.property {
			case "receivedAt", "sentAt":
				r = compareTime(getDate(a, c.property), getDate(b, c.property))
			case "size":
				r = compareUint(emailUint(a, "size"), emailUint(b, "size"))
			case "from", "to":
				r = strings.Compare(getAddr(a, c.property), getAddr(b, c.property))
			case "subject":
				r = strings.Compare(getSubject(a), getSubject(b))
			case "hasKeyword":
				r = compareBool(getKeywords(a)[c.keyword], getKeywords(b)[c.keyword])
			case "allInThreadHaveKeyword", "someInThreadHaveKeyword":
				all := c.property == "allInThreadHaveKeyword"
				r = compareBool(threadHas(a, c.keyword, all), threadHas(b, c.keyword, all))
			}
			if r != 0 {
				if c.descending {
					return -r
				}
				return r
			}
		}
		return 0
	}, "", ""
}

// ---- value helpers ----

// keywordArg decodes a keyword FilterCondition/sort value and folds its case.
// Keywords are case-insensitive (section 4.1.1) and stored lowercased, so
// every keyword comparison - index probe, membership test, thread scan - must
// fold to match. ValidateCondition/ParseSort have already checked the syntax.
func keywordArg(raw json.RawMessage) string {
	s, _ := decodeString(raw)
	return strings.ToLower(s)
}

func validId(raw json.RawMessage) bool {
	var id jmap.Id
	return json.Unmarshal(raw, &id) == nil && id.Valid()
}

func validUTCDate(raw json.RawMessage) bool {
	s, ok := decodeString(raw)
	if !ok {
		return false
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

func validUnsignedInt(raw json.RawMessage) bool {
	var n int64
	return json.Unmarshal(raw, &n) == nil && n >= 0 && jmap.ValidUnsignedInt(n)
}

func utcDate(raw json.RawMessage) time.Time {
	s, _ := decodeString(raw)
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func uintVal(raw json.RawMessage) uint64 {
	var n uint64
	json.Unmarshal(raw, &n)
	return n
}

func emailDate(obj objectdb.Object, name string) time.Time {
	s, _ := decodeString(obj[name])
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func emailUint(obj objectdb.Object, name string) uint64 {
	var n uint64
	json.Unmarshal(obj[name], &n)
	return n
}

// addressText concatenates the names and emails of a stored EmailAddress[]
// property (from/to/cc/bcc) for substring search.
func addressText(obj objectdb.Object, field string) string {
	var addrs []message.Address
	json.Unmarshal(obj[field], &addrs)
	var b strings.Builder
	for _, a := range addrs {
		if a.Name != nil {
			b.WriteString(*a.Name)
			b.WriteByte(' ')
		}
		b.WriteString(a.Email)
		b.WriteByte(' ')
	}
	return b.String()
}

// firstAddr is the sort key for "from"/"to" (section 4.4.2): the name, or if
// null/empty the email, of the first EmailAddress; the empty string if none.
func firstAddr(obj objectdb.Object, field string) string {
	var addrs []message.Address
	json.Unmarshal(obj[field], &addrs)
	if len(addrs) == 0 {
		return ""
	}
	if addrs[0].Name != nil && *addrs[0].Name != "" {
		return *addrs[0].Name
	}
	return addrs[0].Email
}

func containsFold(hay, needle string) bool {
	return strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

// foldKey folds case for the default i;ascii-casemap sort collation.
func foldKey(s string) string { return strings.ToLower(s) }

func compareTime(a, b time.Time) int {
	switch {
	case a.Before(b):
		return -1
	case a.After(b):
		return 1
	}
	return 0
}

func compareUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// compareBool orders false before true (section 4.4.2 keyword sorts).
func compareBool(a, b bool) int {
	return int(b2i(a) - b2i(b))
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func boolKey(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
