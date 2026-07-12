package objectdb

import (
	"bytes"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/internal/keyenc"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// Key layout. Every key starts with the account segment, so one account
// is one contiguous key range: copying, migrating, or deleting an
// account is a range operation (the tenancy founding requirement).
//
//	{acct} o {type} {id}                 object record (JSON)
//	{acct} x {type} {prop} {value} {id}  property index (empty value)
//	{acct} g {seq}                       change log entry (JSON)
//	{acct} q                             sequence counter
//	{acct} s {type}                      per-type state (8-byte seq)
//	{acct} u {blobId}                    blob upload record (JSON)
//	{acct} r {blobId} {type} {id}        blob reference index (empty value)
//
// The encoding lives in internal/keyenc; segments are escaped so
// arbitrary bytes cannot forge separators, and the encoding preserves
// bytes.Compare order (a prefix segment sorts before any longer
// segment). The "B" tag is reserved: the KV blob store (blob/kvstore)
// keeps blob content under {acct} B {blobId} when sharing a backend.
// The top-level "!P" range is reserved for push subscription records
// (package pushsub); "!" is outside the jmap.Id alphabet, so that
// range can never be an account's.

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

func seqKey(acct jmap.Id) []byte { return key(seg(string(acct)), seg("q")) }

func typeStateKey(acct jmap.Id, typeName string) []byte {
	return key(seg(string(acct)), seg("s"), seg(typeName))
}

func uploadKey(acct, blobID jmap.Id) []byte {
	return key(seg(string(acct)), seg("u"), seg(string(blobID)))
}

func refKey(acct, blobID jmap.Id, typeName string, id jmap.Id) []byte {
	return key(seg(string(acct)), seg("r"), seg(string(blobID)), seg(typeName), seg(string(id)))
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
		var s string
		if err := unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []byte(strings.ToLower(s)), nil // ASCII casemap fold
	case descriptor.KindBool:
		var b bool
		if err := unmarshal(raw, &b); err != nil {
			return nil, err
		}
		if b {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case descriptor.KindInt, descriptor.KindUnsignedInt:
		var n int64
		if err := unmarshal(raw, &n); err != nil {
			return nil, err
		}
		return backend.EncodeInt64(n), nil
	case descriptor.KindDate:
		var s string
		if err := unmarshal(raw, &s); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, err
		}
		return backend.EncodeInt64(t.UnixNano()), nil
	case descriptor.KindId:
		var id jmap.Id
		if err := unmarshal(raw, &id); err != nil {
			return nil, err
		}
		return []byte(id), nil
	}
	return nil, errUnknownKind
}
