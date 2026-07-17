// Package backend defines the storage socket: a small ordered
// key-value contract that any engine can implement in a weekend.
// Everything JMAP-shaped (collections, indexes, change log) is built
// once ABOVE this interface by the objectdb package; backends never see
// protocol concepts.
//
// Consistency contract: the runtime serializes all writes to an account
// through a per-account lease and fences every batch with an Assert on
// the lease key. Backends therefore need atomic batches but NO
// interactive transactions and NO isolation between concurrent batches
// to different accounts.
package backend

import (
	"context"
	"encoding/binary"
	"errors"
)

// ErrNotFound is returned by Get for absent keys.
var ErrNotFound = errors.New("backend: key not found")

// ErrAssertFailed is returned by WriteBatch when an Assert op does not
// match; the batch must have applied nothing.
var ErrAssertFailed = errors.New("backend: assertion failed")

// ErrNoSpace is returned by WriteBatch when applying the batch would exceed
// a backend's configured capacity; like a failed Assert, the batch must have
// applied nothing. A backend without a capacity limit never returns it.
var ErrNoSpace = errors.New("backend: capacity exceeded")

// Backend is the six-operation storage contract. Keys are ordered by
// bytes.Compare; values are opaque.
type Backend interface {
	// Get returns the value at key, or ErrNotFound.
	Get(ctx context.Context, key []byte) ([]byte, error)
	// Scan visits keys in [start, end) in ascending order (descending
	// when reverse), calling fn for each; fn returns false to stop
	// early. Key and value slices are only valid during the call.
	Scan(ctx context.Context, start, end []byte, reverse bool, fn func(key, value []byte) bool) error
	// WriteBatch applies all ops atomically: either every op takes
	// effect or none does. A failing Assert aborts with ErrAssertFailed.
	WriteBatch(ctx context.Context, b *Batch) error
	// Close releases resources; the backend is unusable afterwards.
	Close() error
}

// OpKind enumerates batch operations.
type OpKind uint8

const (
	OpSet OpKind = iota + 1
	OpDelete
	OpAdd
	OpAssert
)

// Op is one operation in a batch.
type Op struct {
	Kind  OpKind
	Key   []byte
	Value []byte // Set: value; Assert: expected value (nil = key absent)
	Delta int64  // Add
}

// Batch accumulates operations for one atomic WriteBatch.
type Batch struct {
	Ops []Op
}

// Set stores value at key, overwriting any existing value.
func (b *Batch) Set(key, value []byte) {
	b.Ops = append(b.Ops, Op{Kind: OpSet, Key: key, Value: value})
}

// Delete removes key; deleting an absent key is a no-op.
func (b *Batch) Delete(key []byte) { b.Ops = append(b.Ops, Op{Kind: OpDelete, Key: key}) }

// Add atomically adds delta to the counter at key, creating it at delta
// if absent. Counter values are the 8-byte encoding of EncodeInt64.
func (b *Batch) Add(key []byte, delta int64) {
	b.Ops = append(b.Ops, Op{Kind: OpAdd, Key: key, Delta: delta})
}

// Assert requires the value at key to equal expect (nil = key must be
// absent) for the batch to apply. This is the runtime's lease fencing.
func (b *Batch) Assert(key, expect []byte) {
	b.Ops = append(b.Ops, Op{Kind: OpAssert, Key: key, Value: expect})
}

// EncodeInt64 encodes a counter value as stored by OpAdd: 8 bytes,
// big-endian, offset so that byte order matches numeric order.
func EncodeInt64(v int64) []byte {
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], uint64(v)^(1<<63))
	return out[:]
}

// DecodeInt64 reverses EncodeInt64.
func DecodeInt64(value []byte) (int64, error) {
	if len(value) != 8 {
		return 0, errors.New("backend: counter value is not 8 bytes")
	}
	return int64(binary.BigEndian.Uint64(value) ^ (1 << 63)), nil
}
