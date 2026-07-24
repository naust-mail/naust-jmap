// Package jsonscan is the shared single-pass JSON scanner behind the
// record codec and the hot value checks. Every function is deliberately
// equivalent to the corresponding encoding/json behavior - the stdlib is
// the executable specification, enforced by differential tests - but
// runs in one validating left-to-right pass with no reflection and no
// intermediate maps or slices.
//
// Inputs are untrusted (they may come from a corrupted or hostile
// store): malformed bytes must produce an error or a false report,
// never a panic or unbounded work. Everything is linear time with a
// nesting cap.
package jsonscan

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxDepth caps how many containers (objects and arrays) may be open at
// once, matching encoding/json's nesting limit so equivalence with the
// stdlib holds at the boundary; without a cap, hostile bytes ("[[[["...)
// would recurse without bound.
const MaxDepth = 10000

// DecodeObject parses one stored record: a JSON object of property name
// to raw value. The returned map's values are sub-slices of raw, so the
// caller must own raw and never modify it afterwards. Mirroring
// json.Unmarshal into a map exactly, a literal null input yields a nil
// map with no error, and a duplicated member name keeps the last value.
//
// names, when non-nil, is an interning dictionary: a member name found
// in it is returned as the dictionary's string instead of a fresh copy,
// so decoding a record whose names are all known allocates nothing for
// them. The dictionary is read only, never grown - a caller-fixed
// vocabulary, so hostile inputs cannot inflate it. Names absent from it
// decode identically, just without the sharing.
func DecodeObject(raw []byte, names map[string]string) (map[string]json.RawMessage, error) {
	s := &scanner{buf: raw}
	s.skipSpace()
	if s.pos >= len(s.buf) {
		return nil, s.errAt("empty input")
	}
	if s.buf[s.pos] == 'n' {
		if err := s.literal("null"); err != nil {
			return nil, err
		}
		if err := s.end(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if s.buf[s.pos] != '{' {
		return nil, s.errAt("record is not a JSON object")
	}
	s.pos++
	obj := make(map[string]json.RawMessage, 16)
	s.skipSpace()
	if s.pos < len(s.buf) && s.buf[s.pos] == '}' {
		s.pos++
		if err := s.end(); err != nil {
			return nil, err
		}
		return obj, nil
	}
	for {
		s.skipSpace()
		key, err := s.parseName(names)
		if err != nil {
			return nil, err
		}
		s.skipSpace()
		if s.pos >= len(s.buf) || s.buf[s.pos] != ':' {
			return nil, s.errAt("expected ':' after member name")
		}
		s.pos++
		s.skipSpace()
		start := s.pos
		if err := s.skipValue(1); err != nil {
			return nil, err
		}
		obj[key] = s.buf[start:s.pos]
		s.skipSpace()
		if s.pos >= len(s.buf) {
			return nil, s.errAt("unterminated object")
		}
		switch s.buf[s.pos] {
		case ',':
			s.pos++
		case '}':
			s.pos++
			if err := s.end(); err != nil {
				return nil, err
			}
			return obj, nil
		default:
			return nil, s.errAt("expected ',' or '}' in object")
		}
	}
}

// EncodeObject encodes a record deterministically (member names sorted).
// Every value is fully validated as exactly one JSON value before being
// copied verbatim: a malformed value - trailing bytes, unbalanced
// nesting - could otherwise splice extra members into the record or
// corrupt its structure, a JSON injection into the store, so it is an
// error here just as encoding/json would refuse to marshal it. The
// nesting cap is applied as if the value were already inside the record
// object, so every record this encoder accepts is decodable (stricter
// than json.Marshal, which would let a value at the exact depth limit
// produce an unreadable record).
func EncodeObject(obj map[string]json.RawMessage) ([]byte, error) {
	names := make([]string, 0, len(obj))
	size := 2
	for name, v := range obj {
		names = append(names, name)
		size += len(name) + len(v) + 4
	}
	sort.Strings(names)
	out := make([]byte, 0, size)
	out = append(out, '{')
	for i, name := range names {
		if i > 0 {
			out = append(out, ',')
		}
		out = appendString(out, name)
		out = append(out, ':')
		v := obj[name]
		s := &scanner{buf: v}
		s.skipSpace()
		if s.pos >= len(s.buf) {
			return nil, fmt.Errorf("jsonscan: property %q has an empty value", name)
		}
		if err := s.skipValue(1); err != nil {
			return nil, fmt.Errorf("jsonscan: property %q: %w", name, err)
		}
		if err := s.end(); err != nil {
			return nil, fmt.Errorf("jsonscan: property %q: %w", name, err)
		}
		out = append(out, v...)
	}
	out = append(out, '}')
	return out, nil
}

// String decodes one complete JSON string value, allowing surrounding
// whitespace. It reports false for anything else - including null, which
// json.Unmarshal into *string would silently skip.
func String(raw []byte) (string, bool) {
	s := &scanner{buf: raw}
	s.skipSpace()
	v, err := s.parseString()
	if err != nil || s.end() != nil {
		return "", false
	}
	return v, true
}

// ValidString reports whether raw is one complete JSON string value,
// without materializing it - what validating a KindString needs, minus
// the copy String would make.
func ValidString(raw []byte) bool {
	s := &scanner{buf: raw}
	s.skipSpace()
	return s.skipString() == nil && s.end() == nil
}

// Int decodes one complete JSON number value that is an exact int64,
// with the same acceptance as json.Unmarshal into *int64: the token must
// satisfy the RFC 8259 number grammar and parse as a base-10 integer, so
// fractions and exponents are refused even when integral. The digits are
// accumulated directly (strconv.ParseInt equivalence pinned by the
// differential tests) so validation allocates nothing.
func Int(raw []byte) (int64, bool) {
	s := &scanner{buf: raw}
	s.skipSpace()
	if s.pos >= len(s.buf) {
		return 0, false
	}
	if c := s.buf[s.pos]; c != '-' && (c < '0' || c > '9') {
		return 0, false
	}
	start := s.pos
	if s.skipNumber() != nil {
		return 0, false
	}
	tok := s.buf[start:s.pos]
	if s.end() != nil {
		return 0, false
	}
	neg := tok[0] == '-'
	if neg {
		tok = tok[1:]
	}
	// Accumulate capped at 1<<63, the magnitude of math.MinInt64: the
	// largest value either sign can need.
	const cutoff = uint64(1) << 63
	var u uint64
	for _, c := range tok {
		if c < '0' || c > '9' {
			return 0, false // fraction or exponent: not an integer token
		}
		d := uint64(c - '0')
		if u > (cutoff-d)/10 {
			return 0, false
		}
		u = u*10 + d
	}
	if neg {
		// int64(-u) is the two's-complement negation, exact for every
		// magnitude up to and including 1<<63.
		return int64(-u), true
	}
	if u > cutoff-1 {
		return 0, false
	}
	return int64(u), true
}

// Bool decodes one complete JSON boolean value.
func Bool(raw []byte) (bool, bool) {
	s := &scanner{buf: raw}
	s.skipSpace()
	if s.pos < len(s.buf) && s.buf[s.pos] == 't' {
		if s.literal("true") == nil && s.end() == nil {
			return true, true
		}
		return false, false
	}
	if s.literal("false") == nil && s.end() == nil {
		return false, true
	}
	return false, false
}

// ValidObject reports whether raw is one complete, valid JSON object
// value (not null - callers guard null themselves). Nothing is built:
// this is what validating a KindObject value needs, without the map that
// json.Unmarshal would allocate and discard.
func ValidObject(raw []byte) bool { return validContainer(raw, '{') }

// ValidArray is ValidObject for arrays.
func ValidArray(raw []byte) bool { return validContainer(raw, '[') }

func validContainer(raw []byte, open byte) bool {
	s := &scanner{buf: raw}
	s.skipSpace()
	if s.pos >= len(s.buf) || s.buf[s.pos] != open {
		return false
	}
	// The container is the outermost value, so it opens depth 1 exactly
	// as the stdlib counts a standalone value.
	return s.skipValue(0) == nil && s.end() == nil
}

// EachKey calls fn with each member name of a JSON object value, in
// document order, duplicates included. A literal null makes no calls and
// no error, mirroring json.Unmarshal's null-to-nil-map. Values are
// validated but not materialized.
func EachKey(raw []byte, fn func(key string)) error {
	s := &scanner{buf: raw}
	s.skipSpace()
	if s.pos < len(s.buf) && s.buf[s.pos] == 'n' {
		if err := s.literal("null"); err != nil {
			return err
		}
		return s.end()
	}
	if s.pos >= len(s.buf) || s.buf[s.pos] != '{' {
		return s.errAt("not an object")
	}
	s.pos++
	s.skipSpace()
	if s.pos < len(s.buf) && s.buf[s.pos] == '}' {
		s.pos++
		return s.end()
	}
	for {
		s.skipSpace()
		key, err := s.parseString()
		if err != nil {
			return err
		}
		s.skipSpace()
		if s.pos >= len(s.buf) || s.buf[s.pos] != ':' {
			return s.errAt("expected ':' after member name")
		}
		s.pos++
		s.skipSpace()
		if err := s.skipValue(1); err != nil {
			return err
		}
		fn(key)
		s.skipSpace()
		if s.pos >= len(s.buf) {
			return s.errAt("unterminated object")
		}
		switch s.buf[s.pos] {
		case ',':
			s.pos++
		case '}':
			s.pos++
			return s.end()
		default:
			return s.errAt("expected ',' or '}' in object")
		}
	}
}

// EachString calls fn with each element of a JSON array value whose
// elements must all be strings, in order - the acceptance of
// json.Unmarshal into []string. A literal null makes no calls and no
// error.
func EachString(raw []byte, fn func(elem string)) error {
	s := &scanner{buf: raw}
	s.skipSpace()
	if s.pos < len(s.buf) && s.buf[s.pos] == 'n' {
		if err := s.literal("null"); err != nil {
			return err
		}
		return s.end()
	}
	if s.pos >= len(s.buf) || s.buf[s.pos] != '[' {
		return s.errAt("not an array")
	}
	s.pos++
	s.skipSpace()
	if s.pos < len(s.buf) && s.buf[s.pos] == ']' {
		s.pos++
		return s.end()
	}
	for {
		s.skipSpace()
		// A null element is the empty string, as json.Unmarshal into
		// []string leaves the element at its zero value.
		if s.pos < len(s.buf) && s.buf[s.pos] == 'n' {
			if err := s.literal("null"); err != nil {
				return err
			}
			fn("")
		} else {
			elem, err := s.parseString()
			if err != nil {
				return err
			}
			fn(elem)
		}
		s.skipSpace()
		if s.pos >= len(s.buf) {
			return s.errAt("unterminated array")
		}
		switch s.buf[s.pos] {
		case ',':
			s.pos++
		case ']':
			s.pos++
			return s.end()
		default:
			return s.errAt("expected ',' or ']' in array")
		}
	}
}

// scanner walks one buffer left to right; pos never moves backwards.
type scanner struct {
	buf []byte
	pos int
}

func (s *scanner) errAt(msg string) error {
	return fmt.Errorf("jsonscan: bad JSON at offset %d: %s", s.pos, msg)
}

// skipSpace advances past JSON whitespace (RFC 8259 section 2: space,
// tab, newline, carriage return - nothing else, so a BOM is an error).
func (s *scanner) skipSpace() {
	for s.pos < len(s.buf) {
		switch s.buf[s.pos] {
		case ' ', '\t', '\n', '\r':
			s.pos++
		default:
			return
		}
	}
}

// end asserts nothing but whitespace remains.
func (s *scanner) end() error {
	s.skipSpace()
	if s.pos != len(s.buf) {
		return s.errAt("trailing data")
	}
	return nil
}

func (s *scanner) literal(want string) error {
	if len(s.buf)-s.pos < len(want) || string(s.buf[s.pos:s.pos+len(want)]) != want {
		return s.errAt("invalid literal")
	}
	s.pos += len(want)
	return nil
}

// skipValue validates one JSON value starting at s.pos, leaving pos just
// past its last byte. open is how many containers already enclose the
// value; opening another beyond MaxDepth is an error.
func (s *scanner) skipValue(open int) error {
	if s.pos >= len(s.buf) {
		return s.errAt("missing value")
	}
	switch c := s.buf[s.pos]; {
	case c == '{':
		if open+1 > MaxDepth {
			return s.errAt("exceeded max nesting depth")
		}
		s.pos++
		s.skipSpace()
		if s.pos < len(s.buf) && s.buf[s.pos] == '}' {
			s.pos++
			return nil
		}
		for {
			s.skipSpace()
			if err := s.skipString(); err != nil {
				return err
			}
			s.skipSpace()
			if s.pos >= len(s.buf) || s.buf[s.pos] != ':' {
				return s.errAt("expected ':' after member name")
			}
			s.pos++
			s.skipSpace()
			if err := s.skipValue(open + 1); err != nil {
				return err
			}
			s.skipSpace()
			if s.pos >= len(s.buf) {
				return s.errAt("unterminated object")
			}
			switch s.buf[s.pos] {
			case ',':
				s.pos++
			case '}':
				s.pos++
				return nil
			default:
				return s.errAt("expected ',' or '}' in object")
			}
		}
	case c == '[':
		if open+1 > MaxDepth {
			return s.errAt("exceeded max nesting depth")
		}
		s.pos++
		s.skipSpace()
		if s.pos < len(s.buf) && s.buf[s.pos] == ']' {
			s.pos++
			return nil
		}
		for {
			s.skipSpace()
			if err := s.skipValue(open + 1); err != nil {
				return err
			}
			s.skipSpace()
			if s.pos >= len(s.buf) {
				return s.errAt("unterminated array")
			}
			switch s.buf[s.pos] {
			case ',':
				s.pos++
			case ']':
				s.pos++
				return nil
			default:
				return s.errAt("expected ',' or ']' in array")
			}
		}
	case c == '"':
		return s.skipString()
	case c == 't':
		return s.literal("true")
	case c == 'f':
		return s.literal("false")
	case c == 'n':
		return s.literal("null")
	case c == '-' || ('0' <= c && c <= '9'):
		return s.skipNumber()
	default:
		return s.errAt("invalid value")
	}
}

// skipString validates a string without decoding it: escapes must be
// well formed and unescaped control characters are an error (RFC 8259
// section 7). Bytes >= 0x20 pass through unexamined - like the stdlib
// scanner, raw invalid UTF-8 is not rejected here.
func (s *scanner) skipString() error {
	if s.pos >= len(s.buf) || s.buf[s.pos] != '"' {
		return s.errAt("expected string")
	}
	s.pos++
	for s.pos < len(s.buf) {
		switch c := s.buf[s.pos]; {
		case c == '"':
			s.pos++
			return nil
		case c == '\\':
			if err := s.skipEscape(); err != nil {
				return err
			}
		case c < 0x20:
			return s.errAt("control character in string")
		default:
			s.pos++
		}
	}
	return s.errAt("unterminated string")
}

func (s *scanner) skipEscape() error {
	if s.pos+1 >= len(s.buf) {
		return s.errAt("unterminated escape")
	}
	switch s.buf[s.pos+1] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		s.pos += 2
		return nil
	case 'u':
		if s.pos+6 > len(s.buf) {
			return s.errAt("truncated \\u escape")
		}
		for _, c := range s.buf[s.pos+2 : s.pos+6] {
			if !isHex(c) {
				return s.errAt("invalid \\u escape")
			}
		}
		s.pos += 6
		return nil
	default:
		return s.errAt("invalid escape character")
	}
}

// parseString validates a string and returns its decoded value. The
// decoding matches encoding/json's unquote exactly: surrogate pairs
// combine, a lone surrogate becomes U+FFFD, and each invalid UTF-8 byte
// becomes U+FFFD.
func (s *scanner) parseString() (string, error) {
	start := s.pos
	if err := s.skipString(); err != nil {
		return "", err
	}
	span := s.buf[start+1 : s.pos-1]
	clean := true
	for _, c := range span {
		if c == '\\' || c >= utf8.RuneSelf {
			clean = false
			break
		}
	}
	if clean {
		return string(span), nil
	}
	return unquote(span), nil
}

// parseName is parseString with interning: a decoded name found in
// names is returned as the dictionary's string, with no allocation for
// an escape-free name (the map lookup on string(span) does not copy).
func (s *scanner) parseName(names map[string]string) (string, error) {
	start := s.pos
	if err := s.skipString(); err != nil {
		return "", err
	}
	span := s.buf[start+1 : s.pos-1]
	clean := true
	for _, c := range span {
		if c == '\\' || c >= utf8.RuneSelf {
			clean = false
			break
		}
	}
	if clean {
		if interned, ok := names[string(span)]; ok {
			return interned, nil
		}
		return string(span), nil
	}
	v := unquote(span)
	if interned, ok := names[v]; ok {
		return interned, nil
	}
	return v, nil
}

// unquote decodes an escape-validated string body.
func unquote(span []byte) string {
	out := make([]byte, 0, len(span))
	for i := 0; i < len(span); {
		switch c := span[i]; {
		case c == '\\':
			switch span[i+1] {
			case '"':
				out = append(out, '"')
				i += 2
			case '\\':
				out = append(out, '\\')
				i += 2
			case '/':
				out = append(out, '/')
				i += 2
			case 'b':
				out = append(out, '\b')
				i += 2
			case 'f':
				out = append(out, '\f')
				i += 2
			case 'n':
				out = append(out, '\n')
				i += 2
			case 'r':
				out = append(out, '\r')
				i += 2
			case 't':
				out = append(out, '\t')
				i += 2
			case 'u':
				r := rune(hex4(span[i+2 : i+6]))
				i += 6
				if utf16.IsSurrogate(r) {
					if i+6 <= len(span) && span[i] == '\\' && span[i+1] == 'u' {
						if r2 := rune(hex4(span[i+2 : i+6])); utf16.IsSurrogate(r2) {
							if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
								out = utf8.AppendRune(out, dec)
								i += 6
								continue
							}
						}
					}
					r = utf8.RuneError
				}
				out = utf8.AppendRune(out, r)
			}
		case c < utf8.RuneSelf:
			out = append(out, c)
			i++
		default:
			r, size := utf8.DecodeRune(span[i:])
			if r == utf8.RuneError && size == 1 {
				out = utf8.AppendRune(out, utf8.RuneError)
				i++
			} else {
				out = append(out, span[i:i+size]...)
				i += size
			}
		}
	}
	return string(out)
}

// skipNumber validates the RFC 8259 section 6 number grammar. The first
// byte is known to be '-' or a digit.
func (s *scanner) skipNumber() error {
	if s.buf[s.pos] == '-' {
		s.pos++
		if s.pos >= len(s.buf) || !isDigit(s.buf[s.pos]) {
			return s.errAt("invalid number")
		}
	}
	if s.buf[s.pos] == '0' {
		s.pos++
	} else {
		for s.pos < len(s.buf) && isDigit(s.buf[s.pos]) {
			s.pos++
		}
	}
	if s.pos < len(s.buf) && s.buf[s.pos] == '.' {
		s.pos++
		if s.pos >= len(s.buf) || !isDigit(s.buf[s.pos]) {
			return s.errAt("invalid number")
		}
		for s.pos < len(s.buf) && isDigit(s.buf[s.pos]) {
			s.pos++
		}
	}
	if s.pos < len(s.buf) && (s.buf[s.pos] == 'e' || s.buf[s.pos] == 'E') {
		s.pos++
		if s.pos < len(s.buf) && (s.buf[s.pos] == '+' || s.buf[s.pos] == '-') {
			s.pos++
		}
		if s.pos >= len(s.buf) || !isDigit(s.buf[s.pos]) {
			return s.errAt("invalid number")
		}
		for s.pos < len(s.buf) && isDigit(s.buf[s.pos]) {
			s.pos++
		}
	}
	return nil
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// hex4 decodes four validated hex digits.
func hex4(b []byte) uint32 {
	var v uint32
	for _, c := range b {
		v <<= 4
		switch {
		case '0' <= c && c <= '9':
			v |= uint32(c - '0')
		case 'a' <= c && c <= 'f':
			v |= uint32(c-'a') + 10
		default:
			v |= uint32(c-'A') + 10
		}
	}
	return v
}

const hexDigits = "0123456789abcdef"

// appendString appends s as a JSON string: quote and backslash escaped,
// control characters escaped, invalid UTF-8 coerced to U+FFFD so the
// output is always valid UTF-8 JSON.
func appendString(out []byte, s string) []byte {
	out = append(out, '"')
	for _, r := range s {
		switch {
		case r == '"':
			out = append(out, '\\', '"')
		case r == '\\':
			out = append(out, '\\', '\\')
		case r == '\n':
			out = append(out, '\\', 'n')
		case r == '\r':
			out = append(out, '\\', 'r')
		case r == '\t':
			out = append(out, '\\', 't')
		case r < 0x20:
			out = append(out, '\\', 'u', '0', '0', hexDigits[r>>4], hexDigits[r&0xF])
		default:
			out = utf8.AppendRune(out, r)
		}
	}
	return append(out, '"')
}
