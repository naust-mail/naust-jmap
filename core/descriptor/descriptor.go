// Package descriptor defines object types for the runtime: the schema
// vocabulary that datatype plugins register instead of implementing
// methods. Per-property attributes carry the RFC 8620 semantics
// (immutable, server-set, default) that the runtime enforces before any
// plugin code runs; this is where protocol compliance lives.
package descriptor

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// isNull reports whether raw is the literal JSON null.
func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// Kind is a property's JSON value type.
type Kind uint8

const (
	// KindString is a JSON string.
	KindString Kind = iota + 1
	// KindBool is true or false.
	KindBool
	// KindInt is an integer within the RFC 8620 section 1.3 Int range.
	KindInt
	// KindUnsignedInt additionally requires the value be >= 0.
	KindUnsignedInt
	// KindDate is a section 1.4 Date string.
	KindDate
	// KindId is a section 1.2 Id string.
	KindId
	// KindObject is a JSON object with opaque contents: the descriptor
	// validates only that the value is an object (RFC 8620 has these
	// throughout - PushSubscription.keys, Email's "Id[Boolean]"
	// mailboxIds). A plugin's Set.Validate hook enforces what the members
	// mean; a PatchObject pointer may address one member (section 5.3).
	KindObject
	// KindArray is a JSON array with opaque contents: validated only as an
	// array (Email's "EmailAddress[]" from, "String[]" references).
	KindArray
)

// Property describes one property of an object type.
type Property struct {
	Kind Kind
	// Immutable properties may only be set at create (RFC 8620
	// section 5.3: including one in an update is invalidProperties
	// unless the value is identical).
	Immutable bool
	// ServerSet properties are set only by the server; a client create
	// must omit them, and updates may include them only with the current
	// value (section 5.3).
	ServerSet bool
	// Indexed properties get an order-preserving index maintained
	// in-commit, making them cheap /query filters and sorts.
	Indexed bool
	// SetIndexed marks a composite property (KindObject or KindArray)
	// whose members are reverse-indexed in-commit: one index entry per
	// member, so "which records contain member X" is a range scan
	// (objectdb.IdsWhereMember). Members are the object's keys for a
	// KindObject (the Id[Boolean]/String[Boolean] maps: mailboxIds,
	// keywords) and the string elements for a KindArray (Email's msgid
	// lists). Unlike Indexed, there is no ordering - membership only -
	// so it is the composite counterpart of Indexed, not a substitute
	// for it. The blob reference index is the same shape specialized to
	// BlobRef properties.
	SetIndexed bool
	// BlobRef marks a KindId property whose value is a blobId (RFC 8620
	// section 6). The runtime rejects creates/updates referencing a blob
	// that does not exist in the account (a dangling foreign key is
	// invalidProperties per section 5.3) and maintains the blob
	// reference index in-commit, which is what keeps referenced blobs
	// safe from garbage collection.
	BlobRef bool
	// Nullable admits the literal JSON null as a stored value, for the
	// spec's "Kind|null" properties (Mailbox's parentId is "Id|null").
	// A stored null is distinct from an absent property: it appears as
	// null in /get responses and sorts before every non-null value.
	Nullable bool
	// Internal marks a property the runtime maintains for its own use -
	// typically an Indexed or SetIndexed value a datatype derives to make
	// a lookup cheap (Email's threadKeys) - that is not part of the type's
	// public schema. It is invisible to the client-facing method layer:
	// never returned or requestable in /get, not settable in /set (create
	// or patch), and not a valid /query filter property. The object
	// database still indexes and stores it like any other property; only
	// the protocol surface hides it.
	Internal bool
	// Default, if non-nil, is the value used when a create omits the
	// property and the value restored when a patch sets it to null
	// (section 5.3). Properties without a default are removed by null.
	Default json.RawMessage
}

// Type is a registered object type. The "id" property is implicit on
// every type (server-set, immutable, KindId) and must not be declared.
type Type struct {
	// Name is the type name as used in method names ("Foo" of Foo/get).
	Name string
	// Capability is the URI that must be in a request's "using" for this
	// type's methods to exist (section 3.3).
	Capability string
	Properties map[string]Property
}

// Validate checks the descriptor at registration time.
func (t *Type) Validate() error {
	if t.Name == "" || t.Capability == "" {
		return fmt.Errorf("descriptor: type needs Name and Capability")
	}
	if _, ok := t.Properties["id"]; ok {
		return fmt.Errorf("descriptor: %s declares \"id\"; it is implicit on every type", t.Name)
	}
	for name, p := range t.Properties {
		if name == "" {
			return fmt.Errorf("descriptor: %s has an empty property name", t.Name)
		}
		if p.Kind < KindString || p.Kind > KindArray {
			return fmt.Errorf("descriptor: %s.%s has unknown kind", t.Name, name)
		}
		if p.BlobRef && p.Kind != KindId {
			return fmt.Errorf("descriptor: %s.%s is BlobRef but not KindId", t.Name, name)
		}
		if p.Indexed && (p.Kind == KindObject || p.Kind == KindArray) {
			// Composite values have no order-preserving encoding; the
			// index machinery would have nothing to sort them by.
			return fmt.Errorf("descriptor: %s.%s is a composite kind and cannot be Indexed", t.Name, name)
		}
		if p.SetIndexed && p.Kind != KindObject && p.Kind != KindArray {
			// A set index reverse-indexes members; only composite kinds
			// have members to enumerate.
			return fmt.Errorf("descriptor: %s.%s is SetIndexed but not a composite kind", t.Name, name)
		}
		if p.Indexed && p.SetIndexed {
			return fmt.Errorf("descriptor: %s.%s cannot be both Indexed and SetIndexed", t.Name, name)
		}
		if p.Default != nil {
			if err := p.CheckValue(p.Default); err != nil {
				return fmt.Errorf("descriptor: %s.%s default: %w", t.Name, name, err)
			}
		}
	}
	return nil
}

// CheckValue reports whether raw is a valid value for the property's
// kind. Mechanical validation only; invalid values map to the
// invalidProperties SetError (section 5.3).
func (p Property) CheckValue(raw json.RawMessage) error {
	if isNull(raw) {
		// Guarded explicitly: json.Unmarshal treats null as a no-op
		// success for every Go type, which would let null through each
		// kind check below.
		if p.Nullable {
			return nil
		}
		return fmt.Errorf("null is not allowed")
	}
	switch p.Kind {
	case KindString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("not a string")
		}
	case KindBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("not a boolean")
		}
	case KindInt, KindUnsignedInt:
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil {
			return fmt.Errorf("not an integer")
		}
		if p.Kind == KindUnsignedInt && !jmap.ValidUnsignedInt(n) {
			return fmt.Errorf("out of UnsignedInt range")
		}
		if p.Kind == KindInt && !jmap.ValidInt(n) {
			return fmt.Errorf("out of Int range")
		}
	case KindDate:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || !jmap.ValidDate(s) {
			return fmt.Errorf("not a Date")
		}
	case KindId:
		var id jmap.Id
		if err := json.Unmarshal(raw, &id); err != nil || !id.Valid() {
			return fmt.Errorf("not an Id")
		}
	case KindObject:
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("not an object")
		}
	case KindArray:
		var a []json.RawMessage
		if err := json.Unmarshal(raw, &a); err != nil {
			return fmt.Errorf("not an array")
		}
	default:
		return fmt.Errorf("unknown kind")
	}
	return nil
}
