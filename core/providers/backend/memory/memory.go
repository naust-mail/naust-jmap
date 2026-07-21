// Package memory is the in-memory Backend: the trivial reference
// implementation and the default for tests and development. Nothing
// persists across Close.
package memory

import (
	"bytes"
	"context"
	"sort"
	"sync"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// Store implements backend.Backend over a mutex-guarded map plus a
// sorted key slice, so scans seek to their range instead of visiting
// and sorting the whole keyspace per call.
type Store struct {
	mu       sync.RWMutex
	data     map[string][]byte
	keys     []string // sorted ascending; always exactly the keys of data
	capacity int64    // 0 = unlimited
	used     int64    // sum of stored value sizes, maintained by WriteBatch
}

// Option configures a Store.
type Option func(*Store)

// WithCapacity bounds the total stored value bytes: a WriteBatch that would
// push the total over n fails with backend.ErrNoSpace and applies nothing,
// so the in-memory backend rejects rather than growing without limit. n <= 0
// leaves it unlimited.
func WithCapacity(n int64) Option {
	return func(s *Store) { s.capacity = n }
}

// New returns an empty in-memory backend.
func New(opts ...Option) *Store {
	s := &Store{data: make(map[string][]byte)}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Get implements backend.Backend.
func (s *Store) Get(_ context.Context, key []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[string(key)]
	if !ok {
		return nil, backend.ErrNotFound
	}
	return bytes.Clone(v), nil
}

// MultiGet implements backend.MultiGetter. There is no round trip to save
// in-process, but one lock acquisition for the whole batch instead of one
// per key is still cheaper, and every backend needs to satisfy the same
// differential tests (backendtest) regardless of what it has to gain.
func (s *Store) MultiGet(_ context.Context, keys [][]byte) ([][]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([][]byte, len(keys))
	for i, key := range keys {
		if v, ok := s.data[string(key)]; ok {
			out[i] = bytes.Clone(v)
		}
	}
	return out, nil
}

// Scan implements backend.Backend: binary-search to the range edge,
// then walk only the keys actually yielded. The lock is never held
// while fn runs (fn may re-enter the store), so the walk copies a
// bounded chunk at a time and resumes strictly past the last yielded
// key; like the sqlite driver's row streaming, a long scan can observe
// concurrent commits - the contract promises order and bounds, not a
// point-in-time snapshot.
func (s *Store) Scan(_ context.Context, start, end []byte, reverse bool, fn func(key, value []byte) bool) error {
	const chunk = 64
	type kv struct {
		k string
		v []byte
	}
	buf := make([]kv, 0, chunk)
	var cursor string // last yielded key; valid once started
	started := false
	for {
		buf = buf[:0]
		s.mu.RLock()
		if reverse {
			// The upper bound is exclusive throughout: end (nil = +inf)
			// on the first chunk, the last yielded key after.
			hi := len(s.keys)
			if started {
				hi = sort.SearchStrings(s.keys, cursor)
			} else if end != nil {
				hi = sort.SearchStrings(s.keys, string(end))
			}
			for i := hi - 1; i >= 0 && len(buf) < chunk; i-- {
				k := s.keys[i]
				if k < string(start) {
					break
				}
				buf = append(buf, kv{k, bytes.Clone(s.data[k])})
			}
		} else {
			lo := sort.SearchStrings(s.keys, string(start))
			if started {
				lo = sort.SearchStrings(s.keys, cursor)
				if lo < len(s.keys) && s.keys[lo] == cursor {
					lo++
				}
			}
			for i := lo; i < len(s.keys) && len(buf) < chunk; i++ {
				k := s.keys[i]
				if end != nil && k >= string(end) {
					break
				}
				buf = append(buf, kv{k, bytes.Clone(s.data[k])})
			}
		}
		s.mu.RUnlock()
		for _, e := range buf {
			if !fn([]byte(e.k), e.v) {
				return nil
			}
		}
		if len(buf) < chunk {
			return nil
		}
		cursor, started = buf[len(buf)-1].k, true
	}
}

// WriteBatch implements backend.Backend: asserts and Add operands are
// validated before any mutation, so a failing batch applies nothing.
func (s *Store) WriteBatch(_ context.Context, b *backend.Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, op := range b.Ops {
		if op.Kind != backend.OpAssert {
			continue
		}
		current, exists := s.data[string(op.Key)]
		if op.Value == nil {
			if exists {
				return backend.ErrAssertFailed
			}
		} else if !exists || !bytes.Equal(current, op.Value) {
			return backend.ErrAssertFailed
		}
	}
	type staged struct {
		value   []byte
		deleted bool
	}
	pending := make(map[string]staged)
	lookup := func(k string) ([]byte, bool) {
		if st, ok := pending[k]; ok {
			return st.value, !st.deleted
		}
		v, ok := s.data[k]
		return v, ok
	}
	for _, op := range b.Ops {
		k := string(op.Key)
		switch op.Kind {
		case backend.OpSet:
			pending[k] = staged{value: bytes.Clone(op.Value)}
		case backend.OpDelete:
			pending[k] = staged{deleted: true}
		case backend.OpAdd:
			base := int64(0)
			if v, ok := lookup(k); ok {
				n, err := backend.DecodeInt64(v)
				if err != nil {
					return err
				}
				base = n
			}
			pending[k] = staged{value: backend.EncodeInt64(base + op.Delta)}
		}
	}
	// Capacity check: sum the net change in stored value bytes and reject the
	// whole batch if it would exceed the limit, so nothing is applied (like a
	// failed Assert). Only value bytes are counted; keys are small relative to
	// the blob values this bounds.
	if s.capacity > 0 {
		var delta int64
		for k, st := range pending {
			old := int64(len(s.data[k]))
			var next int64
			if !st.deleted {
				next = int64(len(st.value))
			}
			delta += next - old
		}
		if s.used+delta > s.capacity {
			return backend.ErrNoSpace
		}
		s.used += delta
	}

	for k, st := range pending {
		_, existed := s.data[k]
		if st.deleted {
			if existed {
				delete(s.data, k)
				s.removeKey(k)
			}
		} else {
			s.data[k] = st.value
			if !existed {
				s.insertKey(k)
			}
		}
	}
	return nil
}

// insertKey and removeKey keep s.keys sorted; both are called with the
// write lock held and only when membership actually changes.
func (s *Store) insertKey(k string) {
	i := sort.SearchStrings(s.keys, k)
	s.keys = append(s.keys, "")
	copy(s.keys[i+1:], s.keys[i:])
	s.keys[i] = k
}

func (s *Store) removeKey(k string) {
	i := sort.SearchStrings(s.keys, k)
	if i < len(s.keys) && s.keys[i] == k {
		s.keys = append(s.keys[:i], s.keys[i+1:]...)
	}
}

// Close implements backend.Backend.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = nil
	s.keys = nil
	return nil
}
