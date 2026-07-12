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
		if p.Kind < KindString || p.Kind > KindId {
			return fmt.Errorf("descriptor: %s.%s has unknown kind", t.Name, name)
		}
		if p.BlobRef && p.Kind != KindId {
			return fmt.Errorf("descriptor: %s.%s is BlobRef but not KindId", t.Name, name)
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
	default:
		return fmt.Errorf("unknown kind")
	}
	return nil
}
