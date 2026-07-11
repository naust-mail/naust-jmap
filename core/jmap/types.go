// Package jmap defines the wire types of the JMAP protocol (RFC 8620):
// the request/response envelope, the session resource, identifiers, and
// the error taxonomy. It is pure data - no I/O, no state - and is the
// shared contract between the runtime, datatype plugins, and embedders.
package jmap

import (
	"crypto/rand"
	"time"
)

// Id is a record or account identifier per RFC 8620 section 1.2:
// 1-255 characters from the URL-safe base64 alphabet (A-Za-z0-9, -, _).
type Id string

// Valid reports whether the Id satisfies the section 1.2 constraints.
func (id Id) Valid() bool {
	if len(id) < 1 || len(id) > 255 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

const idAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// NewId returns a random 17-character Id. Section 1.2 recommends defensive
// allocation: the first character is always a letter, so ids never start
// with a dash or digit, are never digit-only, and never spell "NIL".
func NewId() Id {
	var b [17]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("jmap: crypto/rand failed: " + err.Error())
	}
	b[0] = idAlphabet[b[0]%52] // letters only
	for i := 1; i < len(b); i++ {
		b[i] = idAlphabet[b[i]%64]
	}
	return Id(b[:])
}

// MaxInt is the upper bound of the Int and UnsignedInt types (RFC 8620
// section 1.3): the largest integer exactly representable in a float64.
const MaxInt = 1<<53 - 1

// MinInt is the lower bound of the Int type.
const MinInt = -(1<<53 - 1)

// ValidInt reports whether v is within the JMAP Int range.
func ValidInt(v int64) bool { return v >= MinInt && v <= MaxInt }

// ValidUnsignedInt reports whether v is within the JMAP UnsignedInt range.
func ValidUnsignedInt(v int64) bool { return v >= 0 && v <= MaxInt }

// ValidDate reports whether s is a JMAP Date (RFC 8620 section 1.4):
// RFC 3339 date-time with zero fractional seconds omitted and all
// letters uppercase.
func ValidDate(s string) bool {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return false
	}
	// RFC 3339 permits lowercase t/z and a zero secfrac; JMAP does not.
	for i := 0; i < len(s); i++ {
		if s[i] == 't' || s[i] == 'z' {
			return false
		}
	}
	if t.Nanosecond() == 0 {
		for i := 0; i < len(s); i++ {
			if s[i] == '.' {
				return false
			}
		}
	}
	return true
}

// ValidUTCDate reports whether s is a JMAP UTCDate: a ValidDate whose
// time offset is literally "Z".
func ValidUTCDate(s string) bool {
	return ValidDate(s) && s[len(s)-1] == 'Z'
}
