package objectdb

import (
	"bytes"
	"errors"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/internal/jsonscan"
	"github.com/naust-mail/naust-jmap/core/internal/keyenc"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// errBadIndexValue reports a value that cannot be encoded for its
// property's kind. Note the decoders behind indexValue refuse a literal
// null outright (see jsonscan), where json.Unmarshal would no-op into a
// zero value: a null on a non-Nullable property fails closed here
// instead of silently indexing as the kind's zero.
var errBadIndexValue = errors.New("objectdb: value does not match the property's kind")

// Key layout. Every key starts with the account segment, so one account
// is one contiguous key range: copying, migrating, or deleting an
// account is a range operation (the tenancy founding requirement).
//
//	{acct} o {type} {id}                 object record (JSON)
//	{acct} x {type} {prop} {value} {id}  property index (empty value)
//	{acct} g {seq}                       change log entry (JSON)
//	{acct} f                             change log floor (8-byte seq)
//	{acct} q                             sequence counter
//	{acct} s {type}                      per-type state (8-byte seq)
//	{acct} u {blobId}                    blob upload record (JSON)
//	{acct} r {blobId} {type} {id}        blob reference index (empty value)
//	{acct} p {blobId}                    blob pending-collection hint (empty value)
//
// The encoding lives in internal/keyenc; segments are escaped so
// arbitrary bytes cannot forge separators, and the encoding preserves
// bytes.Compare order (a prefix segment sorts before any longer
// segment). The "B" tag is reserved: the KV blob store (blob/kvstore)
// keeps blob content under {acct} B {blobId} when sharing a backend.
// The top-level "!P" range is reserved for push subscription records
// (package pushsub), and "!tag" holds the account-tag sets (tagKey
// below); "!" is outside the jmap.Id alphabet, so those ranges can
// never be an account's.

// key concatenates encoded segments.
func key(segs ...[]byte) []byte { return keyenc.Key(segs...) }

// prefixRange returns the [start, end) scan bounds covering every key
// that starts with the given segments.
func prefixRange(segs ...[]byte) (start, end []byte) {
	return keyenc.PrefixRange(segs...)
}

func seg(s string) []byte { return []byte(s) }

func objKey(acct jmap.Id, typeName string, id jmap.Id) []byte {
	return key(seg(string(acct)), seg("o"), seg(typeName), seg(string(id)))
}

// idxKey builds a property index key. order holds the encoded values of
// the property's OrderBy siblings (empty for the common no-OrderBy
// case): they sit between the value and the id, so records sharing an
// indexed value scan back ordered by them, then by id. An absent
// ordering value is an empty segment, which sorts before every present
// value.
func idxKey(acct jmap.Id, typeName, prop string, value []byte, order [][]byte, id jmap.Id) []byte {
	segs := make([][]byte, 0, 5+len(order))
	segs = append(segs, seg(string(acct)), seg("x"), seg(typeName), seg(prop), value)
	segs = append(segs, order...)
	segs = append(segs, seg(string(id)))
	return key(segs...)
}

func logKey(acct jmap.Id, sequence int64) []byte {
	return key(seg(string(acct)), seg("g"), backend.EncodeInt64(sequence))
}

// logFloorKey holds the oldest sequence the change log can still answer
// from: TrimChanges writes it in the same batch that deletes everything
// below it, so a crash can never leave deleted entries that Changes would
// scan past silently. Absent means nothing has ever been trimmed.
func logFloorKey(acct jmap.Id) []byte { return key(seg(string(acct)), seg("f")) }

func seqKey(acct jmap.Id) []byte { return key(seg(string(acct)), seg("q")) }

func typeStateKey(acct jmap.Id, typeName string) []byte {
	return key(seg(string(acct)), seg("s"), seg(typeName))
}

// tagExists is the built-in account tag every commit sets: its tag set
// is the account registry (see DB.Accounts).
const tagExists = "exists"

// tagKey is an account's entry in one account-tag set. Tags are named
// sets of account ids ("!tag" {tag} {acct}, value empty): "exists" is
// the registry every commit maintains, and datatypes maintain worklist
// tags of their own through Update.SetAccountTag/ClearAccountTag. The
// range starts with "!", a byte outside the RFC 8620 section 1.2 Id
// alphabet, so it can never collide with a key of an account's own
// range (whose first segment is the account id).
func tagKey(tag string, acct jmap.Id) []byte {
	return key(seg("!tag"), seg(tag), seg(string(acct)))
}

func uploadKey(acct, blobID jmap.Id) []byte {
	return key(seg(string(acct)), seg("u"), seg(string(blobID)))
}

func refKey(acct, blobID jmap.Id, typeName string, id jmap.Id) []byte {
	return key(seg(string(acct)), seg("r"), seg(string(blobID)), seg(typeName), seg(string(id)))
}

// pendingKey marks a blob as a garbage-collection candidate: every path
// that can leave a blob unreferenced (the upload itself, a reference
// removal) sets it in the same batch as the fact, and SweepBlobs reads
// only this range instead of scanning every upload record. The hint is
// a superset, never truth - the sweep still verifies the reference
// index before deleting, so a stale hint costs one check and a hint can
// never delete a live blob.
func pendingKey(acct, blobID jmap.Id) []byte {
	return key(seg(string(acct)), seg("p"), seg(string(blobID)))
}

// utcDateMicros reads the canonical RFC 3339 UTC form a stored date
// nearly always has ("2026-08-04T12:30:00Z") straight from the JSON
// bytes, returning microseconds since the epoch as the date encoding
// counts them. ok is false for every other shape - an offset, a
// fractional second, anything malformed - and the caller falls back to
// time.Parse, so the accepted set only ever shrinks the work, never the
// meaning. It exists because the fallback materializes a string for
// every value it reads, which on a sort or an index rebuild is one
// string per record.
func utcDateMicros(raw []byte) (int64, bool) {
	if len(raw) != 22 || raw[0] != '"' || raw[21] != '"' {
		return 0, false
	}
	s := raw[1:21]
	if s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' || s[19] != 'Z' {
		return 0, false
	}
	num := func(at, width int) (int, bool) {
		v := 0
		for i := at; i < at+width; i++ {
			c := s[i]
			if c < '0' || c > '9' {
				return 0, false
			}
			v = v*10 + int(c-'0')
		}
		return v, true
	}
	year, ok1 := num(0, 4)
	month, ok2 := num(5, 2)
	day, ok3 := num(8, 2)
	hour, ok4 := num(11, 2)
	minute, ok5 := num(14, 2)
	second, ok6 := num(17, 2)
	if !(ok1 && ok2 && ok3 && ok4 && ok5 && ok6) {
		return 0, false
	}
	if month < 1 || month > 12 || day < 1 || hour > 23 || minute > 59 || second > 60 {
		return 0, false
	}
	t := time.Date(year, time.Month(month), day, hour, minute, second, 0, time.UTC)
	// time.Date normalizes a day past the end of its month (April 31
	// becomes May 1) where time.Parse refuses it; refusing the mismatch
	// keeps this path and the fallback answering identically.
	if t.Year() != year || int(t.Month()) != month || t.Day() != day {
		return 0, false
	}
	return t.UnixMicro(), true
}

// indexValue encodes a property value so that bytes.Compare on index
// keys matches the type's comparison rules (RFC 8620 section 5.5:
// booleans false<true, numbers numerically, dates chronologically;
// strings under the i;ascii-casemap collation). Values of a Nullable
// property carry a tag byte so the literal null has an encoding of its
// own, sorting before every non-null value; non-nullable properties
// keep the bare encoding.
func indexValue(p descriptor.Property, raw []byte) ([]byte, error) {
	if p.Nullable {
		if string(bytes.TrimSpace(raw)) == "null" {
			return []byte{0}, nil
		}
		bare := p
		bare.Nullable = false
		v, err := indexValue(bare, raw)
		if err != nil {
			return nil, err
		}
		return append([]byte{1}, v...), nil
	}
	switch p.Kind {
	case descriptor.KindString:
		k, ok := jsonscan.StringFolded(raw) // casemap fold, one allocation
		if !ok {
			return nil, errBadIndexValue
		}
		return k, nil
	case descriptor.KindBool:
		b, ok := jsonscan.Bool(raw)
		if !ok {
			return nil, errBadIndexValue
		}
		if b {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case descriptor.KindInt, descriptor.KindUnsignedInt:
		n, ok := jsonscan.Int(raw)
		if !ok {
			return nil, errBadIndexValue
		}
		return backend.EncodeInt64(n), nil
	case descriptor.KindDate:
		if micros, ok := utcDateMicros(raw); ok {
			return backend.EncodeInt64(micros), nil
		}
		s, ok := jsonscan.String(raw)
		if !ok {
			return nil, errBadIndexValue
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, err
		}
		// Microseconds since the Unix epoch: an int64 of microseconds
		// represents every RFC 3339 date (years 0000-9999) with room to
		// spare, where nanoseconds would silently overflow outside
		// 1677-2262 and file legal-but-extreme dates in the wrong place.
		// Sub-microsecond digits are dropped (floor, so ordering stays
		// monotonic across the epoch); ties fall to the id that follows
		// the value in every index key.
		return backend.EncodeInt64(t.UnixMicro()), nil
	case descriptor.KindId:
		s, ok := jsonscan.String(raw)
		if !ok {
			return nil, errBadIndexValue
		}
		return []byte(s), nil
	}
	return nil, errUnknownKind
}
