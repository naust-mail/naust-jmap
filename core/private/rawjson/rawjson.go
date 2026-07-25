// Package rawjson reads single values out of raw JSON bytes without
// the reflection, intermediate maps, or allocations of encoding/json's
// generic paths. It is the public face of the scanner the runtime uses
// internally, exposed so datatype modules can read stored property
// values at the same cost as the runtime itself.
//
// This is a supporting toolkit for naust-jmap datatype modules, NOT a
// stable public API - the "private" path segment is the warning. It
// carries no compatibility promise: functions may change signature,
// move, or disappear in any release (for example if a future
// encoding/json makes them redundant). Depend on it from outside this
// project at your own risk.
//
// Behavior is pinned to encoding/json equivalence by differential
// tests against the stdlib, with one deliberate divergence: a literal
// JSON null reports false / makes no calls, where json.Unmarshal
// would silently leave its target untouched. Callers that admit null
// must guard it explicitly. Inputs are untrusted: malformed bytes
// produce a false report or an error, never a panic or unbounded
// work; everything is linear time with a nesting cap.
//
// Only functions with a consumer outside core are exported; more of
// the internal scanner surfaces here if and when a consumer appears.
package rawjson

import "github.com/naust-mail/naust-jmap/core/internal/jsonscan"

// String decodes one complete JSON string value, allowing surrounding
// whitespace. It reports false for anything else - including null,
// which json.Unmarshal into *string would silently skip.
func String(raw []byte) (string, bool) { return jsonscan.String(raw) }

// Int decodes one complete JSON number value that is an exact int64,
// with the same acceptance as json.Unmarshal into *int64: fractions
// and exponents are refused even when integral. Allocates nothing.
func Int(raw []byte) (int64, bool) { return jsonscan.Int(raw) }

// Bool decodes one complete JSON boolean value.
func Bool(raw []byte) (bool, bool) { return jsonscan.Bool(raw) }

// EachKey calls fn with each member name of a JSON object value, in
// document order, duplicates included; member values are validated but
// never materialized. A literal null makes no calls and no error,
// mirroring json.Unmarshal's null-to-nil-map.
func EachKey(raw []byte, fn func(key string)) error { return jsonscan.EachKey(raw, fn) }

// HasKey reports whether a JSON object value has a member named key,
// without materializing the member set; acceptance matches EachKey,
// including validation of the whole value and null-as-empty.
func HasKey(raw []byte, key string) (bool, error) { return jsonscan.HasKey(raw, key) }
