// Package kvstore is the reference blob.Store: blob content held in a
// backend.Backend under {acct} B {blobId}. It may share a backend with
// objectdb - the "B" tag is reserved for it in objectdb's key layout -
// which keeps the whole account, blobs included, in one key range and
// makes blobs exactly as durable as the objects referencing them.
//
// Values are whole blobs in single keys, which is fine at JMAP upload
// sizes (maxSizeUpload); a store for very large blobs should implement
// blob.Store over object storage instead.
package kvstore

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/naust-mail/naust-jmap/core/internal/keyenc"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
)

// Store implements blob.Store over a backend.Backend.
type Store struct {
	be backend.Backend
}

// New wraps a backend.
func New(be backend.Backend) *Store { return &Store{be: be} }

func contentKey(acct, blobID jmap.Id) []byte {
	return keyenc.Key([]byte(acct), []byte("B"), []byte(blobID))
}

// Put implements blob.Store.
func (s *Store) Put(ctx context.Context, acct, blobID jmap.Id, data []byte) error {
	b := &backend.Batch{}
	b.Set(contentKey(acct, blobID), data)
	return s.be.WriteBatch(ctx, b)
}

// Open implements blob.Store.
func (s *Store) Open(ctx context.Context, acct, blobID jmap.Id) (io.ReadCloser, int64, error) {
	data, err := s.be.Get(ctx, contentKey(acct, blobID))
	if errors.Is(err, backend.ErrNotFound) {
		return nil, 0, blob.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// Delete implements blob.Store. Deleting a missing blob succeeds.
func (s *Store) Delete(ctx context.Context, acct, blobID jmap.Id) error {
	b := &backend.Batch{}
	b.Delete(contentKey(acct, blobID))
	return s.be.WriteBatch(ctx, b)
}
