package submit

import (
	"bytes"
	"encoding/json"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/private/rawjson"
)

// cloneObject is a shallow copy of an object's property map, safe to
// mutate without affecting the caller's copy. Duplicated from root's
// cloneObject (same trivial, generic helper; root cannot be imported
// here without a cycle).
func cloneObject(obj objectdb.Object) objectdb.Object {
	next := make(objectdb.Object, len(obj))
	for k, v := range obj {
		next[k] = v
	}
	return next
}

// decodeString decodes a stored JSON string value; ok is false for a
// missing value, or a non-string (exactly rawjson.String's contract).
// Duplicated from root's decodeString (same trivial wrapper).
func decodeString(raw json.RawMessage) (string, bool) {
	return rawjson.String(raw)
}

// isNullRaw reports whether a raw JSON value is the null literal.
// Duplicated from root's isNullRaw (same trivial helper).
func isNullRaw(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// invalidProp builds an invalidProperties SetError for one property.
// Duplicated from root's invalidProp (same trivial helper).
func invalidProp(name, desc string) *jmap.SetError {
	return &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: []string{name}, Description: desc}
}
