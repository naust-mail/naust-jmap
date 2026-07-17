package mail

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// TypeEmail is the Email type name (RFC 8621 section 4).
const TypeEmail = "Email"

// maxKeywordsPerEmail bounds an Email's keyword set. RFC 8621 defines
// tooManyKeywords but advertises no capability field for the limit: it is
// server-defined. 100 is generous for real mail.
const maxKeywordsPerEmail = 100

// keywordForbidden is the set of characters a keyword MUST NOT contain
// (RFC 8621 section 4.1.1): ( ) { ] % * " \. Note the spec lists the
// closing bracket and opening brace only.
const keywordForbidden = `(){]%*"\`

// EmailType returns the Email descriptor. Storage split:
// the RFC 8621 section 4.2 "expected fast" properties are stored on the
// record (extracted once at delivery); bodyStructure, bodyValues,
// headers, and the header:{name} forms are computed on demand from the
// blob (see the Email/get Computed resolver). Only mailboxIds and
// keywords are client-mutable; every derived header property is immutable
// and server-set (RFC 8621 section 4.1).
func EmailType() *descriptor.Type {
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
			// contribution (RFC 8621 sections 3 and 2.1.1).
			"threadId": {Kind: descriptor.KindId, Immutable: true, ServerSet: true, Indexed: true},
			// mailboxIds ("Id[Boolean]") and keywords ("String[Boolean]")
			// are the only mutable properties; both patchable member-wise.
			// Both are set-indexed for membership lookups: mailboxIds for
			// mailboxHasEmail / the onDestroyRemoveEmails cascade / the
			// Email/query inMailbox condition, keywords for the hasKeyword
			// condition (RFC 8621 section 4.4.1).
			"mailboxIds": {Kind: descriptor.KindObject, SetIndexed: true},
			"keywords":   {Kind: descriptor.KindObject, Default: json.RawMessage(`{}`), SetIndexed: true},
			"size":       {Kind: descriptor.KindUnsignedInt, Immutable: true, ServerSet: true},
			"receivedAt": {Kind: descriptor.KindDate, Immutable: true, ServerSet: true},
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

// RegisterEmail registers the Email type, its method extensions, and
// SearchSnippet/get. store is where message blobs live; the Email/get computed
// resolver reads it to derive body properties on demand. cap supplies the
// enforced limits, which MUST match the AccountCapability advertised for the
// account so the wire never promises what the server does not enforce.
// searcher is the text-search socket (RFC 8621 section 4.4.1 text conditions
// and section 5 snippets); a nil searcher installs the naive in-process
// default.
func RegisterEmail(p *runtime.Processor, db *objectdb.DB, store blob.Store, core jmap.CoreCapabilities, acctCap AccountCapability, searcher Searcher) error {
	if searcher == nil {
		searcher = naiveSearcher{store: store}
	}
	ext := &runtime.Extensions{
		// Email/copy is deferred: it is a cross-account ingest that must
		// run threading, counters, and the mailboxIds invariant through
		// insertEmail, not the generic derived copy. Messages enter the
		// store through Email/import and delivery (RFC 8621 section 4.8).
		Methods:              []string{"get", "changes", "set", "query", "queryChanges"},
		DefaultGetProperties: emailDefaultGetProperties,
		Computed:             &emailComputed{store: store},
		ExtraArgs: map[string]runtime.MethodArgs{
			"get": {Names: emailGetArgNames, Check: checkEmailGetArgs},
		},
		Set: &runtime.SetHooks{
			Validate: emailValidate(acctCap.MaxMailboxesPerEmail),
			Destroy:  emailDestroy,
		},
		// Email/query (RFC 8621 section 4.4): the FilterCondition semantics
		// (with an index producer for inMailbox/hasKeyword), the mail sort
		// comparators, and collapseThreads keyed on threadId.
		Query: &runtime.QueryHooks{
			Filter:      emailFilter{db: db, searcher: searcher},
			Sort:        emailSort{db: db},
			CollapseKey: "threadId",
		},
	}
	if err := runtime.RegisterStandardTypeExt(p, db, EmailType(), core, ext); err != nil {
		return err
	}
	// EmailDelivery (RFC 8621 section 1.5) is registered as a method-less
	// push-only type so clients can subscribe to new-mail notifications;
	// insertEmail advances its state via BumpState on each new Email.
	if err := db.RegisterType(EmailDeliveryType()); err != nil {
		return err
	}
	// SearchSnippet/get (RFC 8621 section 5.1) is a custom method: SearchSnippet
	// is not a stored type, so it is not derived from a descriptor.
	p.Register("SearchSnippet/get", CapabilityURI, searchSnippet{db: db, searcher: searcher, core: core}.get)
	// Email/import (section 4.8) and Email/parse (section 4.9) are custom
	// methods: import ingests an uploaded blob through the shared insertEmail
	// path, parse renders a blob as an Email without storing it.
	p.Register("Email/import", CapabilityURI, emailImport{db: db, store: store, core: core, maxMailboxes: acctCap.MaxMailboxesPerEmail}.handle)
	p.Register("Email/parse", CapabilityURI, emailParse{db: db, store: store, core: core}.handle)
	return nil
}

// emailValidate enforces the Email/set rules for M2: creation is not
// supported (message composition is M3), and updates may only change the
// mutable mailboxIds/keywords, which the descriptor already restricts;
// this hook adds the value semantics the descriptor cannot express.
func emailValidate(maxMailboxes *int64) func(*objectdb.Update, objectdb.Object, objectdb.Object, map[string]json.RawMessage) (*jmap.SetError, error) {
	return func(u *objectdb.Update, old, new objectdb.Object, _ map[string]json.RawMessage) (*jmap.SetError, error) {
		if old == nil {
			// Create composes a new message, which needs MIME generation
			// (M3). Delivery and Email/import create records directly, not
			// through Email/set, so this blocks only client composition.
			return &jmap.SetError{
				Type:        jmap.SetErrForbidden,
				Description: "Email creation is not supported; deliver or import the message instead",
			}, nil
		}
		if serr, err := validateMailboxIds(u, new, maxMailboxes); serr != nil || err != nil {
			return serr, err
		}
		if serr, err := validateKeywords(new); serr != nil || err != nil {
			return serr, err
		}
		// The mailboxIds/keywords change moves counters (moving between
		// Mailboxes, marking read); apply the delta in the same commit,
		// before the runtime stages the updated record.
		if err := adjustCounters(u, old, new); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

// validateMailboxIds enforces the "belongs to >= 1 Mailbox" invariant
// (RFC 8621 section 4.1.1), that every value is true, that every key is an
// existing Mailbox, and the tooManyMailboxes limit. It normalizes nothing.
func validateMailboxIds(u *objectdb.Update, new objectdb.Object, maxMailboxes *int64) (*jmap.SetError, error) {
	members, ok := decodeBoolMap(new["mailboxIds"])
	if !ok {
		return invalidProp("mailboxIds", "must be an object of Mailbox id to true"), nil
	}
	if len(members) == 0 {
		return invalidProp("mailboxIds", "an Email must be in at least one Mailbox"), nil
	}
	if maxMailboxes != nil && int64(len(members)) > *maxMailboxes {
		return &jmap.SetError{Type: "tooManyMailboxes", Description: "too many Mailboxes for one Email"}, nil
	}
	for id, val := range members {
		if !val {
			return invalidProp("mailboxIds", "each value must be true"), nil
		}
		obj, err := u.Get(TypeMailbox, jmap.Id(id))
		if errors.Is(err, objectdb.ErrNotFound) || (err == nil && obj == nil) {
			return invalidProp("mailboxIds", fmt.Sprintf("Mailbox %q does not exist", id)), nil
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// validateKeywords enforces keyword syntax (RFC 8621 section 4.1.1),
// lowercases keys in place (servers MUST return keywords in lowercase),
// requires every value be true, and enforces tooManyKeywords.
func validateKeywords(new objectdb.Object) (*jmap.SetError, error) {
	members, ok := decodeBoolMap(new["keywords"])
	if !ok {
		return invalidProp("keywords", "must be an object of keyword to true"), nil
	}
	if len(members) > maxKeywordsPerEmail {
		return &jmap.SetError{Type: "tooManyKeywords", Description: "too many keywords on one Email"}, nil
	}
	lowered := make(map[string]bool, len(members))
	for kw, val := range members {
		if !val {
			return invalidProp("keywords", "each value must be true"), nil
		}
		if !validKeyword(kw) {
			return invalidProp("keywords", fmt.Sprintf("%q is not a valid keyword", kw)), nil
		}
		lowered[strings.ToLower(kw)] = true
	}
	raw, err := json.Marshal(lowered)
	if err != nil {
		return nil, err
	}
	new["keywords"] = raw
	return nil, nil
}

// validKeyword reports whether s is a valid IMAP/JMAP keyword: 1-255
// characters in ASCII %x21-%x7e with none of keywordForbidden.
func validKeyword(s string) bool {
	if len(s) < 1 || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r > 0x7e || strings.ContainsRune(keywordForbidden, r) {
			return false
		}
	}
	return true
}

// decodeBoolMap decodes a JSON object of string to bool. ok is false if
// the value is absent, null, or not an object of booleans.
func decodeBoolMap(raw json.RawMessage) (map[string]bool, bool) {
	if isNullRaw(raw) {
		return nil, false
	}
	var m map[string]bool
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}
