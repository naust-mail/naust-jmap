package mail

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailmethods"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// TypeEmail is the Email type name (RFC 8621 section 4).
const TypeEmail = record.TypeEmail

// emailType returns the Email descriptor. Storage split:
// the RFC 8621 section 4.2 "expected fast" properties are stored on the
// record (extracted once at delivery); bodyStructure, bodyValues,
// headers, and the header:{name} forms are computed on demand from the
// blob (see the Email/get Computed resolver). Only mailboxIds and
// keywords are client-mutable; every derived header property is immutable
// and server-set (RFC 8621 section 4.1).
func emailType() *descriptor.Type {
	// addr is a "EmailAddress[]|null" convenience header property; msgids
	// is a "String[]|null" one. All are immutable and server-set: derived
	// from the message, never set by a client.
	addr := descriptor.Property{Kind: descriptor.KindArray, Nullable: true, Immutable: true, ServerSet: true}
	// The msgid lists are convenience header properties (section 4.1.3).
	// Threading does not look Emails up by these directly; it uses the
	// internal threadKeys index below, which folds the base subject in so
	// a join needs no record loads.
	msgids := descriptor.Property{Kind: descriptor.KindArray, Nullable: true, Immutable: true, ServerSet: true}
	return &descriptor.Type{
		Name:       TypeEmail,
		Capability: CapabilityURI,
		Properties: map[string]descriptor.Property{
			// Metadata (section 4.1.1).
			"blobId": {Kind: descriptor.KindId, BlobRef: true, Immutable: true, ServerSet: true},
			// threadId is indexed so Thread/get can gather a thread's Emails
			// and the counter maintenance can recompute a thread's per-mailbox
			// contribution (RFC 8621 sections 3 and 2.1.1). OrderBy makes the
			// index the Thread's emailIds answer: members scan back sorted by
			// receivedAt then id, exactly the section 3 order, with no record
			// loads. Both properties are immutable, so an entry files into its
			// position once, at create, forever.
			"threadId": {Kind: descriptor.KindId, Immutable: true, ServerSet: true, Indexed: true, OrderBy: []string{"receivedAt"}},
			// mailboxIds ("Id[Boolean]") and keywords ("String[Boolean]")
			// are the only mutable properties; both patchable member-wise.
			// Both are set-indexed for membership lookups: mailboxIds for
			// mailboxHasEmail / the onDestroyRemoveEmails cascade / the
			// Email/query inMailbox condition, keywords for the hasKeyword
			// condition (RFC 8621 section 4.4.1).
			"mailboxIds": {Kind: descriptor.KindObject, SetIndexed: true},
			"keywords":   {Kind: descriptor.KindObject, Default: json.RawMessage(`{}`), SetIndexed: true},
			"size":       {Kind: descriptor.KindUnsignedInt, Immutable: true, ServerSet: true},
			// receivedAt is indexed: the property is immutable, so each
			// entry is written once at create, and the ordered index
			// answers the section 4.4.1 before/after conditions as range
			// reads instead of full scans.
			"receivedAt": {Kind: descriptor.KindDate, Immutable: true, ServerSet: true, Indexed: true},
			// Convenience header properties (section 4.1.3).
			"messageId":  msgids,
			"inReplyTo":  msgids,
			"references": msgids,
			"sender":     addr,
			"from":       addr,
			"to":         addr,
			"cc":         addr,
			"bcc":        addr,
			"replyTo":    addr,
			"subject":    {Kind: descriptor.KindString, Nullable: true, Immutable: true, ServerSet: true},
			"sentAt":     {Kind: descriptor.KindDate, Nullable: true, Immutable: true, ServerSet: true},
			// Derived body metadata (section 4.1.1 / 4.1.4).
			"hasAttachment": {Kind: descriptor.KindBool, Immutable: true, ServerSet: true, Default: json.RawMessage(`false`)},
			"preview":       {Kind: descriptor.KindString, Immutable: true, ServerSet: true, Default: json.RawMessage(`""`)},
			// threadKeys is an internal set index (never on the wire, RFC
			// 8621 defines no such property): one hashed member per
			// (message-id, base subject) pair, so assignThread finds the
			// Emails that satisfy both section 3 join conditions with a
			// single membership lookup and no candidate record loads.
			"threadKeys": {Kind: descriptor.KindArray, Immutable: true, ServerSet: true, SetIndexed: true, Internal: true},
		},
	}
}

// emailDefaultGetProperties is the RFC 8621 section 4.2 default property
// list Email/get uses in place of "all" when properties is omitted/null.
// bodyValues/textBody/htmlBody/attachments are computed, not stored.
var emailDefaultGetProperties = []string{
	"id", "blobId", "threadId", "mailboxIds", "keywords", "size",
	"receivedAt", "messageId", "inReplyTo", "references", "sender", "from",
	"to", "cc", "bcc", "replyTo", "subject", "sentAt", "hasAttachment",
	"preview", "bodyValues", "textBody", "htmlBody", "attachments",
}

// EmailConfig configures RegisterEmail.
type EmailConfig struct {
	// DB is the object database Email records live in. Required.
	DB *objectdb.DB
	// Store is where message blobs live; the Email/get computed resolver
	// reads it to derive body properties on demand. Required.
	Store blob.Store
	// Core is the RFC 8620 capability object whose limits the derived
	// methods enforce.
	Core jmap.CoreCapabilities
	// AccountCapability supplies the enforced mail limits, which MUST
	// match the AccountCapability advertised for the account so the wire
	// never promises what the server does not enforce.
	AccountCapability AccountCapability
	// Searcher is the text-search socket (RFC 8621 section 4.4.1 text
	// conditions and section 5 snippets). Required, since the built-in
	// substring implementation (datatypes/mail/search) is a separate
	// package a host chooses to import rather than a default this package
	// can fall back to; pass search.New(blobs, search.DefaultConfig()) for it.
	Searcher Searcher
	// MessageIDDomain is the domain synthesized Message-IDs are scoped
	// to when a creation omits one (RFC 5322 section 3.6.4) -
	// conventionally the host's mail name, the same value an ingest
	// greeting would use. Empty falls back to the creation's From
	// address, then "localhost".
	MessageIDDomain string
	// InternalProperties declares additional stored properties on the
	// Email record for packages built on this one. Each is forced
	// Internal: never on the wire, invisible to clients, writable only
	// through objectdb's internal-write path. A name colliding with an
	// Email property is a registration error.
	InternalProperties map[string]descriptor.Property
}

// RegisterEmail registers the Email type, its method extensions, and
// SearchSnippet/get.
//
// The Email method implementation (get/query/create/copy/import/parse/
// generation) lives in internal/emailmethods; this function's job is
// wiring those hooks onto the descriptor and the runtime, not implementing
// them.
func RegisterEmail(p *runtime.Processor, cfg EmailConfig) error {
	var missing []string
	if cfg.DB == nil {
		missing = append(missing, "DB")
	}
	if cfg.Store == nil {
		missing = append(missing, "Store")
	}
	if cfg.Searcher == nil {
		missing = append(missing, "Searcher")
	}
	if len(missing) > 0 {
		return fmt.Errorf("mail: RegisterEmail: EmailConfig missing required fields: %s", strings.Join(missing, ", "))
	}
	db, store, acctCap := cfg.DB, cfg.Store, cfg.AccountCapability
	t := emailType()
	for name, prop := range cfg.InternalProperties {
		if _, exists := t.Properties[name]; exists {
			return fmt.Errorf("mail: RegisterEmail: InternalProperties[%q] collides with an Email property", name)
		}
		prop.Internal = true
		t.Properties[name] = prop
	}
	searcher := cfg.Searcher
	gen := emailmethods.GenConfig{MsgIDDomain: cfg.MessageIDDomain}
	mat := emailmethods.Materializer{DB: db, Store: store, MaxMailboxes: acctCap.MaxMailboxesPerEmail}
	creator := emailmethods.EmailCreate{Mat: mat, Cfg: gen, MaxAttachBytes: acctCap.MaxSizeAttachmentsPerEmail}
	ext := &runtime.Extensions{
		// Email/copy is not derived: it is a cross-account ingest that must
		// run threading, counters, and the mailboxIds invariant through the
		// materialize seam, not the generic derived copy. It is registered
		// as a custom method below (internal/emailmethods/emailcopy.go).
		Methods:              []string{"get", "changes", "set", "query", "queryChanges"},
		DefaultGetProperties: emailDefaultGetProperties,
		Computed:             &emailmethods.EmailComputed{Store: store},
		ExtraArgs: map[string]runtime.MethodArgs{
			"get": {Names: emailmethods.EmailGetArgNames, Check: emailmethods.CheckEmailGetArgs},
		},
		Set: &runtime.SetHooks{
			Validate: emailValidate(acctCap.MaxMailboxesPerEmail),
			Destroy:  emailstore.EmailDestroy,
			// Creation is a message generation, not a property store: the
			// create override runs it through emailmethods' generator + the
			// materialize seam (RFC 8621 section 4.6), preparing outside the
			// account lease like every other Email producer.
			PrepareCreate: creator.Prepare,
			CommitCreate:  creator.Commit,
		},
		// Email/query (RFC 8621 section 4.4): the FilterCondition semantics
		// (with index producers for inMailbox/hasKeyword and the
		// before/after date window), the mail sort comparators, and
		// collapseThreads keyed on threadId.
		Query: emailmethods.EmailQueryHooks(db, searcher),
	}
	if err := runtime.RegisterStandardTypeExt(p, db, t, cfg.Core, ext); err != nil {
		return err
	}
	// EmailDelivery (RFC 8621 section 1.5) is registered as a method-less
	// push-only type so clients can subscribe to new-mail notifications;
	// insertEmail advances its state via BumpState on each new Email.
	if err := db.RegisterType(emailDeliveryType()); err != nil {
		return err
	}
	// SearchSnippet/get (RFC 8621 section 5.1) is a custom method: SearchSnippet
	// is not a stored type, so it is not derived from a descriptor.
	p.Register("SearchSnippet/get", CapabilityURI, searchSnippet{db: db, searcher: searcher, core: cfg.Core}.get)
	// Email/import (section 4.8), Email/copy (section 4.7), and Email/parse
	// (section 4.9) are custom methods: import ingests an uploaded blob
	// through the materialize seam, copy ingests another account's message
	// through it, parse renders a blob as an Email without storing it.
	p.Register("Email/import", CapabilityURI, emailmethods.EmailImport{Mat: mat, Core: cfg.Core}.Handle)
	p.Register("Email/copy", CapabilityURI, emailmethods.EmailCopy{Mat: mat, Proc: p, Core: cfg.Core}.Handle)
	p.Register("Email/parse", CapabilityURI, emailmethods.EmailParse{DB: db, Store: store, Core: cfg.Core, EmailType: t}.Handle)
	return nil
}

// emailValidate enforces the Email/set update rules: only the mutable
// mailboxIds/keywords change (the descriptor already restricts that);
// this hook adds the value semantics the descriptor cannot express.
// Creates never reach it - the create override (internal/emailmethods)
// owns creation entirely.
func emailValidate(maxMailboxes *int64) func(*objectdb.Update, objectdb.Object, objectdb.Object, map[string]json.RawMessage) (*jmap.SetError, error) {
	return func(u *objectdb.Update, old, new objectdb.Object, _ map[string]json.RawMessage) (*jmap.SetError, error) {
		if serr, err := emailmethods.ValidateMailboxIds(u, new, maxMailboxes); serr != nil || err != nil {
			return serr, err
		}
		if serr, err := emailmethods.ValidateKeywords(new); serr != nil || err != nil {
			return serr, err
		}
		// The mailboxIds/keywords change moves counters (moving between
		// Mailboxes, marking read); apply the delta in the same commit,
		// before the runtime stages the updated record.
		if err := emailstore.AdjustCounters(u, old, new); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

// cloneObject is a shallow copy of an object's property map, safe to
// mutate without disturbing the staged or committed original. Generic
// object-map plumbing, not mutation-engine logic, so root keeps its own
// copy alongside internal/emailstore's rather than exporting one across
// the boundary for this alone.
func cloneObject(obj objectdb.Object) objectdb.Object {
	next := make(objectdb.Object, len(obj))
	for k, v := range obj {
		next[k] = v
	}
	return next
}
