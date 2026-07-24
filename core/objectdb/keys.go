package objectdb

import (
	"bytes"
	"errors"
	"strings"
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

func idxKey(acct jmap.Id, typeName, prop string, value []byte, id jmap.Id) []byte {
	return key(seg(string(acct)), seg("x"), seg(typeName), seg(prop), value, seg(string(id)))
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
		s, ok := jsonscan.String(raw)
		if !ok {
			return nil, errBadIndexValue
		}
		return []byte(strings.ToLower(s)), nil // ASCII casemap fold
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
		s, ok := jsonscan.String(raw)
		if !ok {
			return nil, errBadIndexValue
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, err
		}
		return backend.EncodeInt64(t.UnixNano()), nil
	case descriptor.KindId:
		s, ok := jsonscan.String(raw)
		if !ok {
			return nil, errBadIndexValue
		}
		return []byte(s), nil
	}
	return nil, errUnknownKind
}
