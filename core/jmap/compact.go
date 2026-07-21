package jmap

import (
	"encoding/json"
	"io"
	"slices"
	"sync"
)

// CompactJSON is a JSON value already known to be syntactically valid and
// compact (RFC 8259: no insignificant whitespace) - the exact form
// json.Marshal produces. It exists so a value already proven compact can be
// embedded verbatim into a larger encode (Response.WriteJSON) without a
// second full syntax scan over content that has already passed through one.
//
// CompactJSON proves only JSON syntax: valid structure, valid UTF-8,
// correct escaping. It proves nothing about the content past that - not
// semantic validity against whatever schema the bytes claim to encode, not
// that the caller was authorized to include it, not that it carries no
// sensitive data. Treat it as "safe to concatenate", never as "safe to
// disclose" or "safe to act on".
type CompactJSON []byte

// MarshalCompactJSON marshals v and wraps the result, so the returned
// CompactJSON's syntactic-validity guarantee holds by construction: the only
// way to produce one is through encoding/json's own Marshal.
func MarshalCompactJSON(v any) (CompactJSON, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return CompactJSON(b), nil
}

// appendJSONString appends s as a JSON string literal by delegating to
// encoding/json rather than reimplementing escaping: quoting, control
// characters, and the '<','>','&' HTML-safe escaping json.Marshal applies by
// default (matching what json.NewEncoder(w).Encode used before it, so
// switching to WriteJSON does not change wire output for these fields).
func appendJSONString(dst []byte, s string) ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return dst, err
	}
	return append(dst, b...), nil
}

// responseBufPool recycles the byte slice WriteJSON builds a response into,
// so a server handling many requests amortizes it to near-zero allocation
// per call instead of a fresh buffer (and its growth reallocations for
// anything bigger than the initial capacity) every time. Pooling *[]byte,
// not []byte: sync.Pool.Put/Get box their argument as any, and boxing a
// plain []byte value allocates on every call, which would claw back most of
// what pooling exists to save - a pointer avoids that.
var responseBufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 4096)
		return &buf
	},
}

// maxPooledResponseBuf bounds what WriteJSON returns to responseBufPool. An
// outlier response (a large Email/get page) grows its buffer well past
// 4096 - keeping that buffer in the pool forever would trade the per-call
// allocation this exists to avoid for permanently inflating the pool's
// steady-state size instead. A buffer bigger than this is left for the
// garbage collector rather than pooled.
const maxPooledResponseBuf = 1 << 20 // 1 MiB

// WriteJSON writes r's JSON encoding (RFC 8620 section 3.4) to w, followed
// by a newline (matching json.Encoder.Encode's convention, so switching a
// caller from json.NewEncoder(w).Encode(r) to r.WriteJSON(w) changes nothing
// on the wire). Unlike json.Marshal(r), it never re-validates or re-compacts
// each Invocation's Args: Args is CompactJSON by the time it reaches a
// response Invocation (every construction path - reply(), ErrorInvocation,
// and Core/echo, which relays a client's own Args back after CheckIJSON has
// already validated the whole request body it came from - goes through
// MarshalCompactJSON or an equally-validated source; see wire_test.go's
// TestResponseConstructionSitesAreAudited for the enforced list). Name,
// CallID, and SessionState are still escaped through encoding/json (see
// appendJSONString): CallID in particular is the client's own chosen string,
// echoed back unaudited, so it is never treated as pre-trusted content.
func (r *Response) WriteJSON(w io.Writer) error {
	bufp := responseBufPool.Get().(*[]byte)
	buf, err := r.AppendJSON((*bufp)[:0])
	if err != nil {
		*bufp = buf
		putResponseBuf(bufp)
		return err
	}
	buf = append(buf, '\n')
	*bufp = buf
	_, err = w.Write(buf)
	putResponseBuf(bufp)
	return err
}

// putResponseBuf returns bufp to the pool - the same pointer WriteJSON got
// from it, never a freshly taken address, so returning a buffer never costs
// an allocation of its own.
func putResponseBuf(bufp *[]byte) {
	if cap(*bufp) > maxPooledResponseBuf {
		return
	}
	responseBufPool.Put(bufp)
}

// AppendJSON appends r's JSON encoding to dst and returns the extended
// buffer, the way to reuse a buffer across responses instead of allocating
// fresh output each call. See WriteJSON for what it does and does not
// re-validate.
func (r *Response) AppendJSON(dst []byte) ([]byte, error) {
	var err error
	dst = append(dst, `{"methodResponses":[`...)
	for i, inv := range r.MethodResponses {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '[')
		if dst, err = appendJSONString(dst, inv.Name); err != nil {
			return nil, err
		}
		dst = append(dst, ',')
		args := inv.Args
		if len(args) == 0 {
			args = json.RawMessage("{}")
		}
		dst = append(dst, args...)
		dst = append(dst, ',')
		if dst, err = appendJSONString(dst, inv.CallID); err != nil {
			return nil, err
		}
		dst = append(dst, ']')
	}
	dst = append(dst, ']')
	if len(r.CreatedIds) > 0 {
		// encoding/json sorts map keys for deterministic output; match that
		// (and it happens to make responses reproducible for logging/tests).
		ids := make([]Id, 0, len(r.CreatedIds))
		for id := range r.CreatedIds {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		dst = append(dst, `,"createdIds":{`...)
		for i, id := range ids {
			if i > 0 {
				dst = append(dst, ',')
			}
			if dst, err = appendJSONString(dst, string(id)); err != nil {
				return nil, err
			}
			dst = append(dst, ':')
			newID := r.CreatedIds[id]
			if dst, err = appendJSONString(dst, string(newID)); err != nil {
				return nil, err
			}
		}
		dst = append(dst, '}')
	}
	dst = append(dst, `,"sessionState":`...)
	if dst, err = appendJSONString(dst, r.SessionState); err != nil {
		return nil, err
	}
	dst = append(dst, '}')
	return dst, nil
}
