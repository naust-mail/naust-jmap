// Package quotas implements the JMAP Quotas capability (RFC 9425):
// read-only Quota records reporting an account's resource limits and
// current usage, with the standard get/changes/query/queryChanges
// methods derived from the core runtime.
//
// Quota definitions (limits, names, scopes) come from one of two
// places: hand-written records maintained through Service.Upsert and
// Service.Delete, or an embedder-supplied Source pulled and mirrored by
// Service.Refresh - the socket for systems that keep quota rules
// elsewhere (a billing tier database, a fleet controller). Usage is
// never pulled: RFC 9425 section 4.1 makes "used" a server-computed
// value, and this package maintains it through the Service.AddUsed and
// Service.SetUsed counter paths. Wire the capability by registering it
// on the processor and advertising the capability's empty object
// (RFC 9425 section 2.1) on the server:
//
//	svc, err := quotas.Register(proc, quotas.Config{DB: db, Core: core})
//	...
//	err = srv.Capability(quotas.CapabilityURI).Advertise(struct{}{}, struct{}{}).Err()
package quotas

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// CapabilityURI identifies the Quotas capability (RFC 9425 section
// 2.1). Its value in both the session capabilities object and each
// account's accountCapabilities is an empty object.
const CapabilityURI = "urn:ietf:params:jmap:quota"

// TypeName is the Quota data type name (RFC 9425 section 7.2).
const TypeName = "Quota"

// typesProperty is the internal property holding the quota's full
// types list (RFC 9425 section 4.1). The wire-visible "types" property
// is computed from it per request: the server MUST filter out types
// whose capability the client did not request in "using", so the raw
// list never reaches the wire directly.
const typesProperty = "allTypes"

// sourceIdProperty is the internal property carrying the Source's own
// stable identifier for a mirrored record. Records written through
// Service.Upsert do not carry it; Service.Refresh manages exactly the
// records that do, so the two kinds coexist in one account and a
// refresh can never destroy a record it does not own.
const sourceIdProperty = "sourceId"

// quotaType returns the Quota descriptor. Every property is
// server-set: RFC 9425 defines no /set method, and definitions change
// only through this package's write paths.
func quotaType() *descriptor.Type {
	null := json.RawMessage("null")
	limit := descriptor.Property{Kind: descriptor.KindUnsignedInt, ServerSet: true, Nullable: true, Default: null}
	return &descriptor.Type{
		Name:       TypeName,
		Capability: CapabilityURI,
		Properties: map[string]descriptor.Property{
			"resourceType":   {Kind: descriptor.KindString, ServerSet: true},
			"used":           {Kind: descriptor.KindUnsignedInt, ServerSet: true, Default: json.RawMessage("0")},
			"hardLimit":      {Kind: descriptor.KindUnsignedInt, ServerSet: true},
			"scope":          {Kind: descriptor.KindString, ServerSet: true},
			"name":           {Kind: descriptor.KindString, ServerSet: true},
			"warnLimit":      limit,
			"softLimit":      limit,
			"description":    {Kind: descriptor.KindString, ServerSet: true, Nullable: true, Default: null},
			typesProperty:    {Kind: descriptor.KindArray, Internal: true},
			sourceIdProperty: {Kind: descriptor.KindString, Internal: true, Nullable: true, Default: null},
		},
	}
}

// Config configures Register.
type Config struct {
	// DB is the object database Quota records live in. Required.
	DB *objectdb.DB
	// Core is the RFC 8620 capability object whose limits bound the
	// derived methods.
	Core jmap.CoreCapabilities
	// Source, when non-nil, is the external system quota definitions
	// are pulled from; Service.Refresh mirrors it into DB. Nil means
	// definitions are hand-written records maintained through
	// Service.Upsert and Service.Delete.
	Source Source
	// TypeCapabilities maps JMAP type names to the capability URI that
	// defines them, for the RFC 9425 section 4.1 types filtering: a
	// type is recognized by a client only when the client's request
	// opted in to the type's capability. Nil means
	// DefaultTypeCapabilities(). Values are used verbatim.
	TypeCapabilities map[string]string
	// ScopeVisible decides whether a quota of the given scope may be
	// shown to the caller of the request ctx belongs to. RFC 9425
	// section 8 requires that quotas covering shared resources -
	// "domain" and "global" scope - be kept from general users,
	// because a shared counter reports other people's activity: a
	// reader who watches it move learns how many accounts a mailing
	// list has, or that some other user is busy.
	//
	// Nil selects that rule: "account" scope is visible, "domain" and
	// "global" are not. Supply a function to widen it - deciding who
	// counts as an administrator is the embedder's knowledge, not this
	// package's, and an embedder's own middleware can carry that
	// verdict on the context the hook receives:
	//
	//	ScopeVisible: func(ctx context.Context, _ jmap.Id, scope string) bool {
	//		return scope == "account" || isAdmin(ctx)
	//	}
	//
	// The decision is per request, so one identity may see a global
	// quota while another does not. Hidden records are withheld the
	// way section 4.1's unrecognized-type rule withholds them: absent
	// from /query, reported in /get's notFound.
	ScopeVisible func(ctx context.Context, acct jmap.Id, scope string) bool
}

// accountScopeOnly is the default ScopeVisible: shared-resource quotas
// stay hidden (RFC 9425 section 8).
func accountScopeOnly(_ context.Context, _ jmap.Id, scope string) bool {
	return scope == "account"
}

// Service is the write and reconciliation API for Quota records; it is
// returned by Register. Reads go through the derived JMAP methods.
type Service struct {
	db       *objectdb.DB
	source   Source
	typeCaps map[string]string
}

// Register registers the Quota type and its RFC 9425 method behavior -
// get/changes/query/queryChanges only, per-request types filtering,
// the Quota/changes updatedProperties response field - and returns the
// Service maintaining the records. Advertising the capability's empty
// session object is the embedder's step (see the package example).
func Register(p *runtime.Processor, cfg Config) (*Service, error) {
	if cfg.DB == nil {
		return nil, errors.New("quotas: Register: Config missing required field: DB")
	}
	tc := cfg.TypeCapabilities
	if tc == nil {
		tc = DefaultTypeCapabilities()
	}
	scopeVisible := cfg.ScopeVisible
	if scopeVisible == nil {
		scopeVisible = accountScopeOnly
	}
	ext := &runtime.Extensions{
		// RFC 9425 section 4 lists get/changes/query/queryChanges; the
		// type has no /set (all properties are server-set) and /copy is
		// defined only for Email (RFC 8621).
		Methods: []string{"get", "changes", "query", "queryChanges"},
		DefaultGetProperties: []string{
			"resourceType", "used", "hardLimit", "warnLimit", "softLimit",
			"scope", "name", "types", "description",
		},
		Computed:      quotaComputed{typeCaps: tc},
		ExtraResponse: &runtime.ResponseExtras{Changes: quotaChangesExtra},
		Query:         &runtime.QueryHooks{Filter: quotaFilter{typeCaps: tc}},
		// Two independent reasons to withhold a record: section 4.1
		// forbids returning a Quota with no type the client
		// recognizes, and section 8 keeps shared-resource scopes from
		// general users.
		Visible: func(ctx context.Context, acct jmap.Id, obj objectdb.Object) bool {
			scope, ok := decodeString(obj["scope"])
			if !ok || !scopeVisible(ctx, acct, scope) {
				return false
			}
			return len(recognizedTypes(ctx, tc, obj)) > 0
		},
	}
	if err := runtime.RegisterStandardTypeExt(p, cfg.DB, quotaType(), cfg.Core, ext); err != nil {
		return nil, err
	}
	return &Service{db: cfg.DB, source: cfg.Source, typeCaps: tc}, nil
}
