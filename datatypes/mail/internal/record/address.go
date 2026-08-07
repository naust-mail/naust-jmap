package record

import (
	"strings"

	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/private/rawjson"
)

// Address is the decode shape of a stored EmailAddress property (RFC 8621
// section 4.1.2.3): {name, email}. Name is nil when absent or null.
type Address struct {
	Name  *string
	Email string
}

// Addresses decodes a stored EmailAddress[] property (from/to/cc/bcc) into
// its address values. A missing, null, or malformed property yields no
// addresses, matching the field's absence.
//
// This walks the JSON directly instead of decoding into []Address via
// encoding/json, because it runs once per candidate record on the
// Email/query text-search path (AddressText, and emailmethods.firstAddr's
// sort key): a filter or sort touching from/to/cc/bcc/text visits every
// candidate the planner cannot narrow away, so this is one of the hottest
// per-record decodes in the runtime. Benchmarked at roughly 2.6x
// json.Unmarshal's cost with a third of its allocations for the shapes
// this path actually sees.
//
// The input is always server-produced - objectdb's own record encoding, or
// the message parser's address decode at delivery time - never raw wire
// bytes, so this does not carry jsonscan's full defensive posture against
// adversarial input. It fails closed (returns no addresses, or an address
// with fewer fields than intended) on anything unexpected rather than
// panicking, but it is not fuzzed the way jsonscan is. A future caller
// reading untrusted bytes through logic like this would need that
// hardening first, and should use core/private/rawjson instead.
func Addresses(obj objectdb.Object, field string) []Address {
	var out []Address
	eachElement(obj[field], func(elem []byte) {
		var a Address
		eachMember(elem, func(key, value []byte) {
			switch string(key) {
			case `"name"`:
				if s, ok := rawjson.String(value); ok {
					a.Name = &s
				}
			case `"email"`:
				a.Email, _ = rawjson.String(value)
			}
		})
		out = append(out, a)
	})
	return out
}

// AddressText concatenates the names and emails of a stored EmailAddress[]
// property (from/to/cc/bcc) for substring search.
func AddressText(obj objectdb.Object, field string) string {
	var b strings.Builder
	for _, a := range Addresses(obj, field) {
		if a.Name != nil {
			b.WriteString(*a.Name)
			b.WriteByte(' ')
		}
		b.WriteString(a.Email)
		b.WriteByte(' ')
	}
	return b.String()
}

// eachElement calls fn with each raw element of a top-level JSON array, in
// document order. A missing, null, or non-array value calls fn zero times.
func eachElement(raw []byte, fn func(elem []byte)) {
	i, n := skipWS(raw, 0), len(raw)
	if i >= n || raw[i] != '[' {
		return
	}
	i++
	for {
		i = skipWSAndCommas(raw, i)
		if i >= n || raw[i] == ']' {
			return
		}
		start := i
		i = skipJSONValue(raw, i)
		fn(raw[start:i])
	}
}

// eachMember calls fn with each top-level "key":value pair of a JSON
// object, in document order. key is still quoted - callers compare
// against the quoted form rather than unquoting every key, which this
// package's small, known field set (name, email) never needs to do. A
// missing, null, or non-object value calls fn zero times.
func eachMember(raw []byte, fn func(key, value []byte)) {
	i, n := skipWS(raw, 0), len(raw)
	if i >= n || raw[i] != '{' {
		return
	}
	i++
	for {
		i = skipWSAndCommas(raw, i)
		if i >= n || raw[i] == '}' {
			return
		}
		if raw[i] != '"' {
			return
		}
		keyStart := i
		i = skipJSONString(raw, i)
		key := raw[keyStart:i]
		i = skipWS(raw, i)
		if i >= n || raw[i] != ':' {
			return
		}
		i = skipWS(raw, i+1)
		valStart := i
		i = skipJSONValue(raw, i)
		fn(key, raw[valStart:i])
	}
}

// skipWS returns the offset of the first non-whitespace byte at or after i.
func skipWS(raw []byte, i int) int {
	for i < len(raw) {
		switch raw[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipWSAndCommas is skipWS also passing over the comma separating one
// array element or object member from the next.
func skipWSAndCommas(raw []byte, i int) int {
	for i < len(raw) {
		switch raw[i] {
		case ' ', '\t', '\n', '\r', ',':
			i++
		default:
			return i
		}
	}
	return i
}

// skipJSONString returns the offset just past the quoted string starting
// at i (which must hold '"'), honoring backslash escapes so an escaped
// quote never ends the string early. Malformed input (no closing quote)
// returns the input length rather than looping past it.
func skipJSONString(raw []byte, i int) int {
	n := len(raw)
	if i >= n || raw[i] != '"' {
		return i
	}
	i++
	for i < n && raw[i] != '"' {
		if raw[i] == '\\' {
			i++
		}
		i++
	}
	if i < n {
		i++ // closing quote
	}
	return i
}

// skipJSONValue returns the offset just past the JSON value starting at i:
// a string, or a run ended by a balanced object/array close, a top-level
// comma, or the enclosing container's own close (neither of the last two
// consumed, so the caller sees them). This is what lets eachElement and
// eachMember find one value's span without decoding it.
func skipJSONValue(raw []byte, i int) int {
	n := len(raw)
	if i >= n {
		return i
	}
	if raw[i] == '"' {
		return skipJSONString(raw, i)
	}
	depth := 0
	inStr := false
	for i < n {
		c := raw[i]
		switch {
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			if depth == 0 {
				return i // belongs to the enclosing container, not this value
			}
			depth--
			if depth == 0 {
				return i + 1 // closed this value's own outer bracket
			}
		case depth == 0 && c == ',':
			return i
		}
		i++
	}
	return i
}
