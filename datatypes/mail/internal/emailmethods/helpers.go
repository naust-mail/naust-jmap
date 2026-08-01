package emailmethods

// Small helpers duplicated from root rather than exported across the
// package boundary for these alone (same precedent as internal/emailstore's
// cloneObject and internal/addr's tokenSafe): trivial, generic, and used
// throughout this package's files.

import (
	"bytes"
	"encoding/json"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/private/rawjson"
)

// isNullRaw reports whether raw is the literal JSON null.
func isNullRaw(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// decodeString decodes a raw JSON string; ok is false for null, a
// missing value, or a non-string (exactly rawjson.String's contract).
func decodeString(raw json.RawMessage) (string, bool) {
	return rawjson.String(raw)
}

// invalidProp builds an invalidProperties SetError for one property.
func invalidProp(name, desc string) *jmap.SetError {
	return &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: []string{name}, Description: desc}
}
