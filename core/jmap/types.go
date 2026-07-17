// Package jmap defines the wire types of the JMAP protocol (RFC 8620):
// the request/response envelope, the session resource, identifiers, and
// the error taxonomy. It is pure data - no I/O, no state - and is the
// shared contract between the runtime, datatype plugins, and embedders.
package jmap

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
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

// crockfordAlphabet is Crockford's base32 alphabet: 32 characters drawn from
// the section 1.2 set, in ascending ASCII order. Because it ascends and
// base32 encodes most-significant bits first, the lexical order of a
// fixed-length encoded id equals the numeric order of the bytes it encodes -
// this is what lets the time- and sequence-based ids below sort by creation
// order. Being all uppercase, it also sidesteps the section 1.2 caution about
// ids that differ only by ASCII case.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var crockford = base32.NewEncoding(crockfordAlphabet).WithPadding(base32.NoPadding)

// idPrefix is the single leading letter every server-assigned id carries.
// Section 1.2 advises a leading alphabetical character so an id never starts
// with a dash or digit, is never digit-only, and never spells "NIL".
const idPrefix = "N"

func randomBytes(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("jmap: crypto/rand failed: " + err.Error())
	}
}

// NewId returns a random Id: the prefix followed by 128 random bits. It has no
// structure and reveals nothing. Section 1.2 does not require ids to be
// random; this is the scheme for deployments that want ids to leak neither
// creation time nor ordering.
func NewId() Id {
	var b [16]byte
	randomBytes(b[:])
	return Id(idPrefix + crockford.EncodeToString(b[:]))
}

// NewULID returns a lexically sortable Id in the ULID layout: the prefix
// followed by a 48-bit millisecond timestamp and 80 random bits. Ids sort by
// creation time and cluster by time for index locality; the trade-off is that
// the timestamp is readable by anyone who sees the id.
func NewULID(now time.Time) Id {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	randomBytes(b[6:])
	return Id(idPrefix + crockford.EncodeToString(b[:]))
}

// NewSequenceId returns a lexically sortable Id with no embedded wall-clock:
// the prefix followed by a per-account sequence number, a within-commit index,
// and 64 random bits. Ids sort by in-account creation order (sequence then
// index) like a database sequence, while the random tail keeps them from being
// enumerable and nothing reveals when the record was created. seq and index
// must be non-negative.
func NewSequenceId(seq, index int64) Id {
	var b [20]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(seq))
	binary.BigEndian.PutUint32(b[8:12], uint32(index))
	randomBytes(b[12:])
	return Id(idPrefix + crockford.EncodeToString(b[:]))
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
