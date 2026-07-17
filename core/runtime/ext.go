package runtime

// Per-type extensions to the derived standard methods. RFC 8621-class
// datatypes extend the six RFC 8620 methods with additional arguments
// (Email/get's bodyProperties), properties that are not stored on the
// record (Email's bodyStructure, Thread's emailIds), additional
// response fields (Mailbox/changes' updatedProperties), and a
// non-"all" default property list (Email/get). Extensions is that
// seam: additive hooks around the derived machinery, never a
// replacement for it. A genuinely new verb (Email/import) is a custom
// method registered with Processor.Register instead.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// MethodArgs declares the additional arguments one derived method
// accepts for a type, beyond its standard RFC 8620 arguments. Unknown
// arguments outside this list remain invalidArguments (section 3.6.2).
type MethodArgs struct {
	// Names are the accepted extra argument names.
	Names []string
	// Check validates the supplied extras before the method runs; a
	// non-nil error rejects the call as invalidArguments with the
	// error text as the description. Arguments the client did not send
	// are absent from extra. May be nil.
	Check func(extra map[string]json.RawMessage) error
}

// ComputedProperties resolves a type's properties that exist on the
// wire but not on the stored record. /get consults Accepts only for
// names that are not declared in the descriptor and not "id", so a
// stored property can never be shadowed.
type ComputedProperties interface {
	// Accepts reports whether name is a valid property name for the
	// type. Dynamic grammars (RFC 8621's "header:{name}:as{Form}") are
	// matched here; a rejected name is invalidArguments per RFC 8620
	// section 5.1.
	Accepts(name string) bool
	// Resolve computes values for the requested accepted names on one
	// record. stored is the full stored object regardless of which
	// properties the client asked for; extra carries the decoded extra
	// /get arguments (nil when none are declared). A name absent from
	// the result is omitted from the response object; returning an
	// explicit JSON null includes the property as null. An error fails
	// the whole call as serverFail.
	Resolve(ctx context.Context, acct jmap.Id, stored objectdb.Object, names []string, extra map[string]json.RawMessage) (map[string]json.RawMessage, error)
}

// ChangesView is the derived /changes response as computed, offered to
// ExtraResponse.Changes read-only.
type ChangesView struct {
	OldState       string
	NewState       string
	HasMoreChanges bool
	Created        []jmap.Id
	Updated        []jmap.Id
	Destroyed      []jmap.Id
	// UpdatedProps is the union of property names that may have changed
	// across the Updated records; nil when unknown (see
	// objectdb.ChangeSet.UpdatedProps). Mailbox/changes derives its
	// updatedProperties response field from this.
	UpdatedProps []string
}

// SetHooks adds type semantics to the derived /set. The mechanical
// RFC 8620 validation (kinds, server-set, immutable, patch rules) has
// already passed when a hook runs; hooks enforce what the descriptor
// cannot express - cross-record invariants, value constraints, destroy
// preconditions - and stage any type-mandated side effects on u.
type SetHooks struct {
	// Validate runs for each create and update before it is staged; a
	// non-nil SetError rejects that record and processing continues
	// with the rest of the call, while a non-nil error aborts the whole
	// method as serverFail (infrastructure failure, not a bad record).
	// old is nil for a create. new is the final object as it would be
	// stored (defaults applied, patch evaluated). extra carries the
	// decoded extra /set arguments.
	Validate func(u *objectdb.Update, old, new objectdb.Object, extra map[string]json.RawMessage) (*jmap.SetError, error)
	// Destroy runs for each destroy before the record is removed; a
	// non-nil SetError rejects it (RFC 8621's mailboxHasChild), a
	// non-nil error aborts the method as serverFail. A hook may stage
	// cascading changes on u. A missing record needs no handling: the
	// runtime reports notFound whether or not the hook ran. extra
	// carries the decoded extra /set arguments.
	Destroy func(u *objectdb.Update, id jmap.Id, extra map[string]json.RawMessage) (*jmap.SetError, error)
}

// QueryRecord is one matched record offered to QueryHooks.Arrange.
type QueryRecord struct {
	Id  jmap.Id
	Obj objectdb.Object
}

// FilterSemantics replaces the core equality FilterCondition language
// for a type whose spec defines its own condition properties (RFC 8621
// Mailbox: substring match on name, the virtual hasAnyRole). Operators
// AND/OR/NOT still combine conditions structurally; only the leaves
// change meaning. With semantics in place the planner does not use
// property indexes for candidate selection - conditions no longer mean
// index equality.
type FilterSemantics interface {
	// ValidateCondition checks one condition property/value pair. An
	// unknown name must return jmap.ErrUnsupportedFilter as
	// UnsupportedFilterError; any other non-nil error rejects the call
	// as invalidArguments.
	ValidateCondition(name string, value json.RawMessage) error
	// MatchCondition reports whether a record matches one condition
	// property/value pair; a FilterCondition object matches when every
	// pair does. Called only for pairs ValidateCondition accepted. It
	// takes ctx and acct because a condition may require I/O to decide
	// (RFC 8621 Email's *InThreadHaveKeyword read the record's Thread and
	// the text conditions read the message blob); a non-nil error fails
	// the whole /query as serverFail.
	MatchCondition(ctx context.Context, acct jmap.Id, obj objectdb.Object, name string, value json.RawMessage) (bool, error)
}

// UnsupportedFilterError marks a ValidateCondition failure that maps to
// the unsupportedFilter method error rather than invalidArguments.
type UnsupportedFilterError struct{ Description string }

func (e UnsupportedFilterError) Error() string { return e.Description }

// ConditionSetProducer is an OPTIONAL capability a FilterSemantics may
// also implement to accelerate its conditions with an index. The /query
// planner (RFC 8620 section 5.5) composes the per-condition sets it returns
// with the filter's AND/OR/NOT operators (intersection / union / universe)
// to narrow the candidate set, then verifies every candidate with
// MatchCondition - so a producer only ever needs to return a SUPERSET of
// the matching ids, never the exact set. It is the type-specific
// counterpart of the generic Indexed-property equality producer the planner
// applies to types without FilterSemantics: for RFC 8621's Email/query
// (section 4.4.1) it answers inMailbox / hasKeyword by set membership and
// the thread links from the threadId index. A type that does not implement
// it, or returns ok=false for a condition, simply has that condition
// evaluated by a full scan plus MatchCondition - correct, just not
// narrowed.
type ConditionSetProducer interface {
	// ConditionSet returns candidate ids for one condition property/value
	// pair. ok=false means the producer cannot narrow this condition (treat
	// as the universe: fall back to scan + predicate). exact=true promises
	// the returned set is precisely the matching set for this pair, not
	// merely a superset - it lets the planner count matches without loading
	// records (the fast-total path); return exact=false whenever unsure.
	// ids need not be sorted or deduplicated. Called only for pairs
	// ValidateCondition accepted.
	ConditionSet(ctx context.Context, acct jmap.Id, name string, value json.RawMessage) (ids []jmap.Id, exact bool, ok bool, err error)
}

// SortSemantics is a PROVISIONAL type-level override of the RFC 8620
// section 5.5 sort Comparator parsing and comparison, for types whose
// sortable values are not plain declared properties - RFC 8621 Email/query
// (section 4.4.2) sorts on a first-address extraction, an RFC 5256 base
// subject, and virtual per-query keyword predicates that carry their own
// "keyword" comparator argument, none of which the core declared-property
// comparators can express. When a type supplies one, the derived /query
// uses it instead of the core comparators.
//
// PROVISIONAL: single consumer (Email) today, so unlike Filter/Arrange
// this contract is NOT frozen and may change shape when a second query
// datatype needs it.
type SortSemantics interface {
	// ParseSort validates the sort comparators and returns a total-order
	// comparison function over stored objects (negative/zero/positive; the
	// planner appends an id tiebreak so ties stay deterministic). An
	// undeclared sort property or unusable collation returns
	// jmap.ErrUnsupportedSort as errType; a malformed comparator returns
	// jmap.ErrInvalidArguments. An empty sort returns a nil comparator and
	// no error (the planner then keeps id order).
	ParseSort(ctx context.Context, acct jmap.Id, sort []json.RawMessage) (less func(a, b objectdb.Object) int, errType, desc string)
}

// QueryHooks customizes the derived /query for a type.
type QueryHooks struct {
	// Filter, when non-nil, supplies the type's FilterCondition
	// semantics. It may also implement ConditionSetProducer to accelerate
	// its conditions via an index.
	Filter FilterSemantics
	// Sort, when non-nil, overrides sort comparator parsing and comparison
	// (PROVISIONAL; see SortSemantics).
	Sort SortSemantics
	// CollapseKey, when non-empty, names the declared property whose value
	// groups records for the collapseThreads /query argument (RFC 8621
	// section 4.4.3). It makes the derived /query accept collapseThreads
	// and, when true, keep only the first record of each distinct key value
	// in the sorted result. Collapse is core behaviour (RFC 8621 keeps it
	// out of plugin code); a type supplies only the grouping-key name.
	CollapseKey string
	// Arrange, when non-nil, runs after filtering, sorting, and any
	// collapse and before windowing: it receives the matched set already in
	// standard sort order plus the comparator chain as a total order
	// (id tiebreak included), and returns the final ordered ids. It may
	// drop records, never add (RFC 8621 Mailbox/query's sortAsTree and
	// filterAsTree). extra carries the decoded extra /query arguments.
	Arrange func(ctx context.Context, acct jmap.Id, matched []QueryRecord, compare func(a, b objectdb.Object) int, extra map[string]json.RawMessage) ([]jmap.Id, error)
}

// ResponseExtras adds type-specific fields to derived method
// responses. Each derived method gets a field here when its first
// consumer arrives; only /changes has one so far.
type ResponseExtras struct {
	// Changes returns additional /changes response fields (RFC 8621
	// section 2.2 adds updatedProperties to Mailbox/changes). extra
	// carries the decoded extra /changes arguments (nil when none are
	// declared). A returned name colliding with a standard response
	// field fails the call as serverFail; an error fails it likewise.
	Changes func(ctx context.Context, acct jmap.Id, view *ChangesView, extra map[string]json.RawMessage) (map[string]json.RawMessage, error)
}

// Extensions customizes the methods RegisterStandardTypeExt derives
// for one type. Every field is optional; the zero value derives the
// plain RFC 8620 behavior.
type Extensions struct {
	// Methods, when non-nil, is the allow-list of standard method
	// suffixes the type actually supports, from get/changes/set/copy/
	// query/queryChanges. nil (the default) derives all six. A type that
	// does not support a method omits it: RFC 8621's Thread has only
	// get+changes, and /copy is defined only for Email. Custom methods
	// (registered separately with Processor.Register) are unaffected.
	Methods []string
	// ExtraArgs declares additional accepted arguments per derived
	// method, keyed by method suffix ("get", "changes", "set", "copy",
	// "query", "queryChanges"). Each method's extras must have a hook
	// that consumes them - /get extras go to Computed.Resolve, /changes
	// extras to ExtraResponse.Changes - and declaring extras for a
	// method with no consuming hook is a registration error: an
	// argument that is accepted but ignored would be a silent lie to
	// the client.
	ExtraArgs map[string]MethodArgs
	// DefaultGetProperties replaces /get's "omitted or null means all
	// properties" default with a fixed list (RFC 8621 section 4.2
	// mandates one for Email; Thread needs one for its computed
	// emailIds to appear by default). Names may be stored or computed;
	// id is always returned and need not be listed. When nil, a
	// properties:null /get returns the stored properties only -
	// computed properties appear only when requested by name or via
	// this list.
	DefaultGetProperties []string
	// Computed resolves the type's non-stored properties on /get.
	Computed ComputedProperties
	// ExtraResponse adds fields to derived method responses.
	ExtraResponse *ResponseExtras
	// Set adds type semantics to the derived /set.
	Set *SetHooks
	// Query customizes the derived /query.
	Query *QueryHooks
}

// standardMethodArgs lists each derived method's own argument names;
// extra argument names must not collide with them.
var standardMethodArgs = map[string][]string{
	"get":          {"accountId", "ids", "properties"},
	"changes":      {"accountId", "sinceState", "maxChanges"},
	"set":          {"accountId", "ifInState", "create", "update", "destroy"},
	"copy":         {"fromAccountId", "ifFromInState", "accountId", "ifInState", "create", "onSuccessDestroyOriginal", "destroyFromIfInState"},
	"query":        {"accountId", "filter", "sort", "position", "anchor", "anchorOffset", "limit", "calculateTotal"},
	"queryChanges": {"accountId", "filter", "sort", "sinceQueryState", "maxChanges", "upToId", "calculateTotal"},
}

// derives reports whether a type with these extensions registers the
// given standard method suffix. A nil Extensions or nil Methods derives
// all six standard methods (the default).
func (e *Extensions) derives(suffix string) bool {
	if e == nil || e.Methods == nil {
		return true
	}
	for _, m := range e.Methods {
		if m == suffix {
			return true
		}
	}
	return false
}

// validate checks the extensions against the descriptor at
// registration time, so misconfiguration fails at startup rather than
// per request.
func (e *Extensions) validate(t *descriptor.Type) error {
	// Methods restricts which standard methods the type derives. Entries
	// must be known suffixes, and no hook may target a method the type
	// does not derive - a hook that can never run is a silent config bug.
	for _, m := range e.Methods {
		if _, known := standardMethodArgs[m]; !known {
			return fmt.Errorf("runtime: %s: Methods lists unknown method suffix %q", t.Name, m)
		}
	}
	if e.Set != nil && !e.derives("set") {
		return fmt.Errorf("runtime: %s: Set hooks declared but /set is not derived", t.Name)
	}
	if e.Query != nil && !e.derives("query") {
		return fmt.Errorf("runtime: %s: Query hooks declared but /query is not derived", t.Name)
	}
	if e.Computed != nil && !e.derives("get") {
		return fmt.Errorf("runtime: %s: Computed hook declared but /get is not derived", t.Name)
	}
	if e.ExtraResponse != nil && e.ExtraResponse.Changes != nil && !e.derives("changes") {
		return fmt.Errorf("runtime: %s: ExtraResponse.Changes declared but /changes is not derived", t.Name)
	}
	for suffix, ma := range e.ExtraArgs {
		if !e.derives(suffix) {
			return fmt.Errorf("runtime: %s: extra args declared for /%s, which the type does not derive", t.Name, suffix)
		}
		std, known := standardMethodArgs[suffix]
		if !known {
			return fmt.Errorf("runtime: %s: extra args declared for unknown method suffix %q", t.Name, suffix)
		}
		switch suffix {
		case "get":
			if e.Computed == nil {
				return fmt.Errorf("runtime: %s: extra /get arguments need a Computed hook to consume them", t.Name)
			}
		case "changes":
			if e.ExtraResponse == nil || e.ExtraResponse.Changes == nil {
				return fmt.Errorf("runtime: %s: extra /changes arguments need an ExtraResponse.Changes hook to consume them", t.Name)
			}
		case "set":
			if e.Set == nil || (e.Set.Validate == nil && e.Set.Destroy == nil) {
				return fmt.Errorf("runtime: %s: extra /set arguments need a Set hook to consume them", t.Name)
			}
		case "query":
			if e.Query == nil || e.Query.Arrange == nil {
				return fmt.Errorf("runtime: %s: extra /query arguments need a Query.Arrange hook to consume them", t.Name)
			}
		default:
			return fmt.Errorf("runtime: %s: no hook consumes extra /%s arguments yet", t.Name, suffix)
		}
		if len(ma.Names) == 0 {
			return fmt.Errorf("runtime: %s: extra args for /%s declare no names", t.Name, suffix)
		}
		seen := make(map[string]bool, len(ma.Names))
		for _, name := range ma.Names {
			if name == "" {
				return fmt.Errorf("runtime: %s: extra args for /%s include an empty name", t.Name, suffix)
			}
			if seen[name] {
				return fmt.Errorf("runtime: %s: extra /%s argument %q declared twice", t.Name, suffix, name)
			}
			seen[name] = true
			for _, s := range std {
				if name == s {
					return fmt.Errorf("runtime: %s: extra argument %q collides with a standard /%s argument", t.Name, name, suffix)
				}
			}
		}
	}
	for _, name := range e.DefaultGetProperties {
		if name == "id" {
			continue
		}
		if _, declared := t.Properties[name]; declared {
			continue
		}
		if e.Computed != nil && e.Computed.Accepts(name) {
			continue
		}
		return fmt.Errorf("runtime: %s: DefaultGetProperties includes unknown property %q", t.Name, name)
	}
	if e.Query != nil && e.Query.CollapseKey != "" {
		if _, declared := t.Properties[e.Query.CollapseKey]; !declared {
			return fmt.Errorf("runtime: %s: Query.CollapseKey names unknown property %q", t.Name, e.Query.CollapseKey)
		}
	}
	return nil
}

// decodeWithExtras decodes method arguments, splitting off the extra
// argument names the type declared for this method and running their
// Check. With no extensions or no declared extras, decoding is the
// plain strict kind. Any error maps to invalidArguments.
func (st *stdType) decodeWithExtras(method string, raw json.RawMessage, v any) (map[string]json.RawMessage, error) {
	if st.ext == nil {
		return nil, decodeArgs(raw, v)
	}
	ma, ok := st.ext.ExtraArgs[method]
	if !ok {
		return nil, decodeArgs(raw, v)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	extra := make(map[string]json.RawMessage)
	for _, name := range ma.Names {
		if val, has := all[name]; has {
			extra[name] = val
			delete(all, name)
		}
	}
	// Re-encoding the remainder cannot mask duplicate keys: the request
	// envelope's strict I-JSON validation already rejected them.
	rest, err := json.Marshal(all)
	if err != nil {
		return nil, err
	}
	if err := decodeArgs(rest, v); err != nil {
		return nil, err
	}
	if ma.Check != nil {
		if err := ma.Check(extra); err != nil {
			return nil, err
		}
	}
	return extra, nil
}

// replyExtra is reply with type-provided response fields merged in. A
// field colliding with a standard response field is a plugin bug and
// fails the call rather than corrupting the response.
func replyExtra(name, callID string, args any, fields map[string]json.RawMessage) []jmap.Invocation {
	if len(fields) == 0 {
		return reply(name, callID, args)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return fail(callID, jmap.ErrServerFail, err.Error())
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fail(callID, jmap.ErrServerFail, err.Error())
	}
	for k, v := range fields {
		if _, dup := m[k]; dup {
			return fail(callID, jmap.ErrServerFail, fmt.Sprintf("extra response field %q collides with a standard field", k))
		}
		m[k] = v
	}
	return reply(name, callID, m)
}
