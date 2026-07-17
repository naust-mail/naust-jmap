package chunkstore

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// Store implements blob.Store.
var _ blob.Store = (*Store)(nil)

// loadManifest reads and decodes a blob's manifest, mapping an absent blob
// to blob.ErrNotFound and a value that is not a valid manifest to a decode
// error rather than a silent misread.
func (s *Store) loadManifest(ctx context.Context, acct, blobID jmap.Id) (manifest, error) {
	v, err := s.be.Get(ctx, manifestKey(acct, blobID))
	if errors.Is(err, backend.ErrNotFound) {
		return manifest{}, blob.ErrNotFound
	}
	if err != nil {
		return manifest{}, err
	}
	return decodeManifest(v)
}

// Open returns the blob's content as a stream and its exact size in octets
// (RFC 8620 section 6). The returned reader fetches one piece at a time, so
// reading never holds the whole blob in memory.
func (s *Store) Open(ctx context.Context, acct, blobID jmap.Id) (io.ReadCloser, int64, error) {
	m, err := s.loadManifest(ctx, acct, blobID)
	if err != nil {
		return nil, 0, err
	}
	return &reader{s: s, ctx: ctx, acct: acct, run: m.run, count: m.count}, m.size, nil
}

// reader streams a blob's pieces in order. It holds at most one piece.
type reader struct {
	s     *Store
	ctx   context.Context
	acct  jmap.Id
	run   runID
	count uint32
	idx   uint32 // next piece to fetch
	cur   []byte // bytes of the current piece not yet returned
	err   error  // sticky: once set, every Read returns it
}

func (r *reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	for len(r.cur) == 0 {
		if r.idx >= r.count {
			return 0, io.EOF
		}
		data, err := r.s.be.Get(r.ctx, pieceKey(r.acct, r.run, r.idx))
		if errors.Is(err, backend.ErrNotFound) {
			// The manifest promised this piece but it is gone: report a
			// clean error instead of silently returning a short blob.
			r.err = fmt.Errorf("chunkstore: piece %d of %d missing", r.idx, r.count)
			return 0, r.err
		}
		if err != nil {
			r.err = err
			return 0, err
		}
		r.idx++
		r.cur = data
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

func (r *reader) Close() error { return nil }

// Delete removes a blob's manifest and pieces. Deleting a blob that is not
// present succeeds, so garbage collection can be idempotent.
func (s *Store) Delete(ctx context.Context, acct, blobID jmap.Id) error {
	m, err := s.loadManifest(ctx, acct, blobID)
	if errors.Is(err, blob.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	b := &backend.Batch{}
	for i := uint32(0); i < m.count; i++ {
		b.Delete(pieceKey(acct, m.run, i))
	}
	b.Delete(manifestKey(acct, blobID))
	return s.be.WriteBatch(ctx, b)
}

// Put stores content whose id is already known (the Blob/copy path, where
// the bytes are in hand rather than streamed). Because the whole value is
// available, the pieces and manifest are written in a single atomic batch:
// no run marker is needed, and a crash leaves either the whole blob or
// nothing. The manifest write asserts the id is absent, so a Put racing an
// identical Put stores exactly one copy and the loser's pieces never apply
// (the batch is atomic). Content is immutable and content-addressed, so a
// Put of an existing blob is a no-op.
func (s *Store) Put(ctx context.Context, acct, blobID jmap.Id, data []byte) error {
	switch _, err := s.be.Get(ctx, manifestKey(acct, blobID)); {
	case err == nil:
		return nil
	case !errors.Is(err, backend.ErrNotFound):
		return err
	}
	var run runID
	if _, err := rand.Read(run[:]); err != nil {
		return err
	}
	b := &backend.Batch{}
	b.Assert(manifestKey(acct, blobID), nil)
	var count uint32
	for off := 0; off < len(data); off += tuning.BlobPieceSize {
		end := off + tuning.BlobPieceSize
		if end > len(data) {
			end = len(data)
		}
		b.Set(pieceKey(acct, run, count), data[off:end])
		count++
	}
	b.Set(manifestKey(acct, blobID), encodeManifest(manifest{run: run, count: count, size: int64(len(data))}))
	if err := s.be.WriteBatch(ctx, b); errors.Is(err, backend.ErrAssertFailed) {
		return nil // a concurrent identical Put stored it first
	} else if err != nil {
		return err
	}
	return nil
}
