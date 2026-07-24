package objectdb

// Codec for stored records, backed by the shared scanner in
// internal/jsonscan. A record is decoded on every Get and encoded on
// every commit, and encoding/json's generic map path costs a validity
// pre-scan plus a reflect-driven walk plus a copy per property value;
// this codec makes one validating pass and returns values as sub-slices
// of the input buffer. decodeObject is deliberately equivalent to
// encoding/json over map[string]json.RawMessage - it accepts an input if
// and only if json.Unmarshal does, with an identical result - so the
// stdlib remains the executable specification, enforced by the
// differential fuzzer in objcodec_test.go.
//
// The bytes being decoded normally come from encodeObject, but the
// backend is not trusted: corrupted or hostile stored bytes must produce
// an error - never a panic, unbounded recursion, or a structurally
// wrong record.

import (
	"github.com/naust-mail/naust-jmap/core/internal/jsonscan"
)

// maxCodecDepth is the shared container nesting cap (jsonscan.MaxDepth).
const maxCodecDepth = jsonscan.MaxDepth

// decodeObject parses one stored record: a JSON object of property name
// to raw value. The returned Object's values are sub-slices of raw, so
// the caller must own raw and never modify it afterwards. Mirroring
// json.Unmarshal into a map exactly, a literal null input yields a nil
// Object with no error, and a duplicated member name keeps the last
// value.
func decodeObject(raw []byte) (Object, error) {
	m, err := jsonscan.DecodeObject(raw, nil)
	return Object(m), err
}

// decodeStored is decodeObject with the DB's property-name dictionary:
// names declared by any registered descriptor come back as the shared
// registration-time strings instead of per-record copies. Undeclared
// names (a record written under a since-removed descriptor) still decode
// identically, just unshared.
func (db *DB) decodeStored(raw []byte) (Object, error) {
	m, err := jsonscan.DecodeObject(raw, db.propNames)
	return Object(m), err
}

// encodeObject encodes a record deterministically (member names sorted),
// refusing any property value that is not exactly one valid JSON value -
// see jsonscan.EncodeObject for the injection and depth guarantees.
func encodeObject(obj Object) ([]byte, error) {
	return jsonscan.EncodeObject(obj)
}
