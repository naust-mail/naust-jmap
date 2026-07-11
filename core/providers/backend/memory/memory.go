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

// Store implements backend.Backend over a mutex-guarded map.
type Store struct {
	mu   sync.RWMutex
	data map[string][]byte
}

// New returns an empty in-memory backend.
func New() *Store {
	return &Store{data: make(map[string][]byte)}
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

// Scan implements backend.Backend.
func (s *Store) Scan(_ context.Context, start, end []byte, reverse bool, fn func(key, value []byte) bool) error {
	s.mu.RLock()
	type kv struct {
		k string
		v []byte
	}
	entries := make([]kv, 0, len(s.data))
	for k, v := range s.data {
		if k >= string(start) && (end == nil || k < string(end)) {
			entries = append(entries, kv{k, bytes.Clone(v)})
		}
	}
	s.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool {
		if reverse {
			return entries[i].k > entries[j].k
		}
		return entries[i].k < entries[j].k
	})
	for _, e := range entries {
		if !fn([]byte(e.k), e.v) {
			return nil
		}
	}
	return nil
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
	for k, st := range pending {
		if st.deleted {
			delete(s.data, k)
		} else {
			s.data[k] = st.value
		}
	}
	return nil
}

// Close implements backend.Backend.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = nil
	return nil
}
