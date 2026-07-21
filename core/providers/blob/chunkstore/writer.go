package chunkstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"hash"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

var (
	errWriterDone = errors.New("chunkstore: blob writer already finalized")
	// errUploadReclaimed reports that a run's marker vanished before Commit -
	// a Sweep reclaimed its pieces (after a stall past UploadReclaimWindow, or
	// a cross-process clock skew). The upload must be retried, not published
	// over the now-deleted pieces.
	errUploadReclaimed = errors.New("chunkstore: upload run was reclaimed before commit")
)

// initialBufSize is the capacity a piece buffer starts at, before append grows
// it toward the current piece's target size. It is deliberately not a tunable:
// it changes no stored format and no backend write pattern, only how much
// capacity a writer holds on the way to a piece it may never fill.
const initialBufSize = 64 << 10

// firstPieceSize is the target size of a blob's FIRST piece. Pieces double from
// here up to tuning.BlobPieceSize (see pieceSize).
const firstPieceSize = 256 << 10

// pieceSize returns the target size of piece number `index`, doubling from
// firstPieceSize up to tuning.BlobPieceSize.
//
// Pieces need not be uniform: a manifest records only the run, the piece COUNT
// and the total size, and reads walk the pieces in order (there is no offset
// table and no ranged read), so nothing downstream can observe how the bytes
// were divided. Ramping uses that freedom to escape a real bind in a single
// fixed size, where the only thing that decides peak memory is whether a blob
// exceeds one piece:
//
//   - Sized for few round trips (a large piece), every blob smaller than one
//     piece is buffered WHOLE, which is what the whole-value store does. Peak
//     then tracks blob size times concurrent writers.
//   - Sized to cap that memory (a small piece), a large blob pays a backend
//     round trip per piece all the way up.
//
// Neither is a compromise, because a fixed size cannot be: its effect is a step
// at "blob exceeds one piece", so a value between two others behaves exactly
// like the larger one for every blob that still fits. Ramping makes the two
// ends independent - a small blob is one piece and one round trip, a mid-size
// blob's buffer is capped well below its own length, and a large blob still
// reaches full-size pieces so its round trips stay proportional to
// BlobPieceSize rather than to the first piece.
func pieceSize(index uint32) int {
	size := firstPieceSize
	for i := uint32(0); i < index; i++ {
		if size >= tuning.BlobPieceSize {
			return tuning.BlobPieceSize
		}
		size *= 2
	}
	if size > tuning.BlobPieceSize {
		return tuning.BlobPieceSize
	}
	return size
}

// Create begins a streaming write. A random run id is chosen and a marker,
// stamped with the current time, is recorded before any content, so every
// piece this writer goes on to store is covered by a marker Sweep can age and
// reclaim if the process dies mid-upload. The content address is computed
// from the bytes as they are written, so the whole blob is never held in
// memory.
func (s *Store) Create(ctx context.Context, acct jmap.Id) (blob.BlobWriter, error) {
	var run runID
	if _, err := rand.Read(run[:]); err != nil {
		return nil, err
	}
	stamp := s.now()
	mv := markerValue(acct, run, stamp)
	b := &backend.Batch{}
	b.Set(markerKey(run), mv)
	if err := s.be.WriteBatch(ctx, b); err != nil {
		return nil, err
	}
	return &writer{s: s, ctx: ctx, acct: acct, run: run, hash: sha256.New(), lastStamp: stamp, lastMarker: mv}, nil
}

// writer is the streaming BlobWriter. Bytes accumulate into one piece
// buffer; a full piece is flushed to the backend and the buffer replaced,
// so memory use stays bounded to a single piece regardless of blob size.
type writer struct {
	s    *Store
	ctx  context.Context
	acct jmap.Id
	run  runID
	hash hash.Hash

	buf   []byte // current partial piece; handed to the backend at flush
	count uint32 // pieces flushed so far
	size  int64  // total bytes written
	done  bool

	// lastStamp and lastMarker track the run marker as this writer last wrote
	// it: lastStamp throttles refreshes, and lastMarker is asserted on every
	// piece and at Commit so a run a Sweep reclaimed is detected rather than
	// written over.
	lastStamp  time.Time
	lastMarker []byte
}

// Write appends bytes to the blob, flushing whole pieces as the buffer
// fills. It updates the running content hash so Commit needs no second
// pass over the data.
func (w *writer) Write(p []byte) (int, error) {
	if w.done {
		return 0, errWriterDone
	}
	total := len(p)
	w.hash.Write(p)
	w.size += int64(total)
	for len(p) > 0 {
		if w.buf == nil {
			// Start small and let append grow it, rather than reserving a
			// whole BlobPieceSize up front. A blob shorter than one piece -
			// which most mail is - would otherwise hold the full piece size
			// for its whole write while using a fraction of it, and that
			// reservation is per CONCURRENT WRITER, so it is the term that
			// decides peak RSS under load rather than anything about blob
			// size. It buys nothing even in principle: a blob that never
			// fills a piece is flushed once at Commit either way.
			//
			// Growth is capped at BlobPieceSize below, so a large blob still
			// reaches full-size pieces and the same number of backend writes.
			w.buf = make([]byte, 0, min(initialBufSize, pieceSize(w.count)))
		}
		target := pieceSize(w.count)
		take := target - len(w.buf)
		if take > len(p) {
			take = len(p)
		}
		w.buf = append(w.buf, p[:take]...)
		p = p[take:]
		if len(w.buf) == target {
			if err := w.flush(); err != nil {
				return 0, err
			}
		}
	}
	// Re-stamp the marker on a wall-clock schedule, not per piece, so even a
	// slow trickle that has not filled a piece keeps proving liveness.
	if err := w.maybeRefresh(); err != nil {
		return 0, err
	}
	return total, nil
}

// flush stores the current buffer as the next piece. The write asserts the
// run marker still holds this writer's last value: if a Sweep reclaimed the
// run (deleting the marker and the pieces so far), the assert fails and the
// write stops rather than leaving a piece behind a deleted marker that no
// later Sweep could find. The same batch re-stamps the marker, so every
// piece write changes the marker's value: a Sweep that judged the run stale
// before this piece landed then fails its own delete-time assert and spares
// the run, instead of deleting the marker around a piece it never scanned
// and leaving that piece unreachable by any later Sweep. The piece bytes are
// never mutated after this (a fresh buffer is taken), so one atomic batch
// suffices.
func (w *writer) flush() error {
	now := w.s.now()
	mv := markerValue(w.acct, w.run, now)
	b := &backend.Batch{}
	b.Assert(markerKey(w.run), w.lastMarker)
	b.Set(pieceKey(w.acct, w.run, w.count), w.buf)
	b.Set(markerKey(w.run), mv)
	if err := w.s.be.WriteBatch(w.ctx, b); err != nil {
		if errors.Is(err, backend.ErrAssertFailed) {
			return errUploadReclaimed
		}
		return err
	}
	w.count++
	w.buf = nil
	w.lastStamp = now
	w.lastMarker = mv
	return nil
}

// maybeRefresh re-stamps the run marker if UploadRefreshInterval has elapsed
// since the last stamp, proving to Sweep the run is still being written. It asserts
// the marker is still this writer's before rewriting, so it never resurrects
// one a Sweep deleted; a failed assert means the run was reclaimed and the
// upload must stop. It is throttled so an upload does not rewrite the marker
// per piece.
func (w *writer) maybeRefresh() error {
	now := w.s.now()
	if now.Sub(w.lastStamp) < tuning.UploadRefreshInterval {
		return nil
	}
	mv := markerValue(w.acct, w.run, now)
	b := &backend.Batch{}
	b.Assert(markerKey(w.run), w.lastMarker)
	b.Set(markerKey(w.run), mv)
	if err := w.s.be.WriteBatch(w.ctx, b); err != nil {
		if errors.Is(err, backend.ErrAssertFailed) {
			return errUploadReclaimed
		}
		return err
	}
	w.lastStamp = now
	w.lastMarker = mv
	return nil
}

// ID returns the blob's content address from the running hash without
// finalizing the writer or publishing the manifest, so the upload can be
// recorded before Commit makes the content addressable.
func (w *writer) ID() jmap.Id {
	var sum [sha256.Size]byte
	w.hash.Sum(sum[:0])
	return blob.IdFromDigest(sum)
}

// Commit stores the final piece and publishes the manifest under the
// blob's content address (RFC 8620 section 6: the id is the address of the
// immutable bytes). Identical content already present is deduplicated:
// this writer's pieces are discarded and the existing id returned (section
// 6.1). The manifest write asserts the id is still absent so two identical
// uploads racing here resolve to a single stored copy.
func (w *writer) Commit() (jmap.Id, error) {
	if w.done {
		return "", errWriterDone
	}
	w.done = true
	if len(w.buf) > 0 {
		if err := w.flush(); err != nil {
			return "", err
		}
	}
	var sum [sha256.Size]byte
	w.hash.Sum(sum[:0])
	id := blob.IdFromDigest(sum)

	switch _, err := w.s.be.Get(w.ctx, manifestKey(w.acct, id)); {
	case err == nil:
		// Content already stored: keep the existing copy, drop ours.
		_ = w.s.discard(w.ctx, w.acct, w.run)
		return id, nil
	case !errors.Is(err, backend.ErrNotFound):
		return "", err
	}

	b := &backend.Batch{}
	// Publish atomically under two guards: the id must still be absent (dedup
	// against a concurrent identical upload) and this run's marker must still
	// be ours (a Sweep has not reclaimed the pieces since the last one was
	// written). Both are checked in the same batch that writes the manifest
	// and deletes the marker.
	b.Assert(manifestKey(w.acct, id), nil)
	b.Assert(markerKey(w.run), w.lastMarker)
	b.Set(manifestKey(w.acct, id), encodeManifest(manifest{run: w.run, count: w.count, size: w.size}))
	b.Delete(markerKey(w.run))
	if err := w.s.be.WriteBatch(w.ctx, b); err != nil {
		if errors.Is(err, backend.ErrAssertFailed) {
			return w.commitAssertFailed(id)
		}
		return "", err
	}
	return id, nil
}

// commitAssertFailed resolves which of Commit's two asserts failed. If the
// manifest now exists, a concurrent identical upload won the dedup race:
// discard this run and return the shared id (RFC 8620 section 6.1). Otherwise
// this run's marker is gone - a Sweep reclaimed the pieces before Commit - so
// the upload must be retried rather than published over deleted content.
func (w *writer) commitAssertFailed(id jmap.Id) (jmap.Id, error) {
	switch _, err := w.s.be.Get(w.ctx, manifestKey(w.acct, id)); {
	case err == nil:
		_ = w.s.discard(w.ctx, w.acct, w.run)
		return id, nil
	case errors.Is(err, backend.ErrNotFound):
		return "", errUploadReclaimed
	default:
		return "", err
	}
}

// Abort discards a blob that was never committed, removing its pieces and
// marker. A finalized writer refuses further use, so a deferred Abort
// after a successful Commit is a harmless no-op error.
func (w *writer) Abort() error {
	if w.done {
		return errWriterDone
	}
	w.done = true
	return w.s.deleteRun(w.ctx, w.acct, w.run)
}

// discard removes a run's pieces and marker on the dedup path. Cleanup is
// best effort: any bytes left by a transient error are reclaimed by a later
// Sweep once the marker's stamp goes stale.
func (s *Store) discard(ctx context.Context, acct jmap.Id, run runID) error {
	return s.deleteRun(ctx, acct, run)
}
