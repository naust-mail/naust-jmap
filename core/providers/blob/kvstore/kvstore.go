// Package kvstore is the reference blob.Store: blob content held in a
// backend.Backend under {acct} B {blobId}. It may share a backend with
// objectdb - the "B" tag is reserved for it in objectdb's key layout -
// which keeps the whole account, blobs included, in one key range and
// makes blobs exactly as durable as the objects referencing them.
//
// Values are whole blobs in single keys, which is fine at JMAP upload
// sizes (maxSizeUpload); a store for very large blobs should implement
// blob.Store over object storage instead.
//
// Choose this over chunkstore when blobs are reliably small: one value
// written once beats a manifest plus pieces, and nothing is streamed, so
// there is less to do per blob. It gives that back as blobs grow, because
// an arriving blob is held whole in memory and the peak scales with blob
// size times concurrent writers, where chunkstore's stays flat. Where the
// two cross over depends on the backend and the write concurrency, so it
// is worth measuring rather than assuming.
//
// "Reliably small" is a requirement, not a preference, and it is the one
// thing to get right before choosing this store. Both factors in that peak
// are normally set by the CLIENT: blob size up to maxSizeUpload, and
// concurrency up to whatever the ingress allows. Peak memory is therefore
// attacker-controlled, which makes an unbounded pairing a denial-of-service
// vector rather than a performance question. Measured at 16 concurrent
// deliveries of a 16 MiB message: about 162 MiB peak RSS on chunkstore
// against about 1.1 GiB here. With the shipped mail defaults (64 concurrent
// LMTP connections, a 50 MB size cap) the same arithmetic reaches several
// gigabytes.
//
// So an embedder choosing this store owns that product: cap blob size well
// below maxSizeUpload's default, cap ingress concurrency, or size the host
// for size x concurrency. An embedder who cannot bound BOTH wants
// chunkstore, whose peak does not depend on either.
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

// Create implements blob.Store. This backend keeps a whole blob in one key,
// so the writer accumulates the bytes and stores them in a single WriteBatch
// at Commit; a store over object storage would instead flush chunks as they
// arrive. The content address is computed from the buffered bytes at Commit
// (blob.IdFor), so there is one definition of the id.
func (s *Store) Create(ctx context.Context, acct jmap.Id) (blob.BlobWriter, error) {
	return &writer{s: s, ctx: ctx, acct: acct}, nil
}

// writer is the reference BlobWriter: bytes are buffered, then written under
// their content address in one batch at Commit.
type writer struct {
	s    *Store
	ctx  context.Context
	acct jmap.Id
	buf  bytes.Buffer
	done bool
}

var errWriterDone = errors.New("kvstore: blob writer already finalized")

func (w *writer) Write(p []byte) (int, error) {
	if w.done {
		return 0, errWriterDone
	}
	return w.buf.Write(p) // bytes.Buffer.Write never returns an error
}

// ID returns the content address of the buffered bytes without storing
// them, so an upload can be recorded before Commit makes it durable.
func (w *writer) ID() jmap.Id {
	return blob.IdFor(w.buf.Bytes())
}

func (w *writer) Commit() (jmap.Id, error) {
	if w.done {
		return "", errWriterDone
	}
	w.done = true
	id := blob.IdFor(w.buf.Bytes())
	b := &backend.Batch{}
	b.Set(contentKey(w.acct, id), w.buf.Bytes())
	if err := w.s.be.WriteBatch(w.ctx, b); err != nil {
		return "", err
	}
	return id, nil
}

func (w *writer) Abort() error {
	if w.done {
		return errWriterDone
	}
	w.done = true
	w.buf.Reset()
	return nil
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
