package chunkstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/internal/keyenc"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// storeAt makes a Store over be whose clock reads *clk, so a test controls
// time by assigning through clk. It bypasses New's construction sweep so the
// clock is in force before any sweep runs.
func storeAt(be backend.Backend, clk *time.Time) *Store {
	return &Store{be: be, now: func() time.Time { return *clk }}
}

// acctPieceRange covers every piece of every run in one account, so a test
// can count the pieces the account holds regardless of how many uploads
// produced them.
func acctPieceRange(acct jmap.Id) (start, end []byte) {
	return keyenc.PrefixRange([]byte(acct), []byte("C"))
}

// smallPieces shrinks the piece size for a test so that modest fixtures
// still exercise the multi-piece streaming paths, restoring the default
// afterwards. Package tests run sequentially, so this is safe.
func smallPieces(t *testing.T, n int) {
	t.Helper()
	orig := tuning.BlobPieceSize
	t.Cleanup(func() { tuning.BlobPieceSize = orig })
	tuning.BlobPieceSize = n
}

func newStore(t *testing.T, opts ...memory.Option) (*Store, backend.Backend) {
	t.Helper()
	be := memory.New(opts...)
	s, err := New(be)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, be
}

// writeStream stores data through the streaming writer, deliberately
// feeding it in awkward 7-byte bursts so piece boundaries fall mid-burst.
func writeStream(t *testing.T, s *Store, acct jmap.Id, data []byte) jmap.Id {
	t.Helper()
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for off := 0; off < len(data); off += 7 {
		end := off + 7
		if end > len(data) {
			end = len(data)
		}
		if n, err := w.Write(data[off:end]); err != nil || n != end-off {
			t.Fatalf("Write: n=%d err=%v", n, err)
		}
	}
	id, err := w.Commit()
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return id
}

func readAll(t *testing.T, s *Store, acct, id jmap.Id) []byte {
	t.Helper()
	rc, size, err := s.Open(context.Background(), acct, id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if int64(len(got)) != size {
		t.Fatalf("Open size = %d, read %d bytes", size, len(got))
	}
	return got
}

func countRange(t *testing.T, be backend.Backend, start, end []byte) int {
	t.Helper()
	n := 0
	if err := be.Scan(context.Background(), start, end, false, func(_, _ []byte) bool {
		n++
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return n
}

func fill(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

// TestRoundTripSizes covers the RFC 8620 section 6 guarantees across the
// piece-boundary cases most likely to break a chunker: empty, sub-piece,
// exactly one and several pieces, and one byte either side of a boundary.
// Each blob must read back byte-for-byte with its exact octet size, and the
// committed id must be the content address IdFor computes over the whole.
// TestWriterIDBeforeCommit: ID reports the content address of the streamed
// bytes without publishing the manifest, so an upload can be recorded before
// Commit makes the content addressable (the record-first ordering the upload
// path relies on). A writer abandoned after ID (a crash before Commit)
// publishes nothing, and once its marker goes stale a sweep reclaims the
// parked pieces.
func TestWriterIDBeforeCommit(t *testing.T) {
	smallPieces(t, 64)
	be := memory.New()
	clk := time.Unix(1_720_000_000, 0)
	s := storeAt(be, &clk)
	acct := jmap.Id("Aone")
	data := fill(200) // several pieces

	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if id := w.ID(); id != blob.IdFor(data) {
		t.Fatalf("ID() = %q, want content address %q", id, blob.IdFor(data))
	}
	// ID must not publish: with no manifest the blob is not readable.
	if _, _, err := s.Open(context.Background(), acct, w.ID()); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("ID() published content early: Open err = %v", err)
	}
	// The abandoned run's pieces are reclaimed once its marker goes stale.
	clk = clk.Add(tuning.UploadReclaimWindow + time.Second)
	if _, err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	ps, pe := acctPieceRange(acct)
	if got := countRange(t, be, ps, pe); got != 0 {
		t.Fatalf("orphaned pieces after sweep: %d", got)
	}
}

func TestRoundTripSizes(t *testing.T) {
	smallPieces(t, 64)
	s, _ := newStore(t)
	acct := jmap.Id("Aone")
	for _, size := range []int{0, 1, 63, 64, 65, 128, 200, 641} {
		data := fill(size)
		id := writeStream(t, s, acct, data)
		if want := blob.IdFor(data); id != want {
			t.Fatalf("size %d: id %q, want content address %q", size, id, want)
		}
		if got := readAll(t, s, acct, id); !bytes.Equal(got, data) {
			t.Fatalf("size %d: round trip mismatch", size)
		}
	}
}

// TestExactPieceMultipleHasNoTrailingPiece: a blob whose length is an exact
// multiple of the piece size must store exactly that many pieces, never an
// extra empty one.
func TestExactPieceMultipleHasNoTrailingPiece(t *testing.T) {
	smallPieces(t, 64)
	s, be := newStore(t)
	acct := jmap.Id("Aone")
	id := writeStream(t, s, acct, fill(128)) // exactly two pieces
	m, err := s.loadManifest(context.Background(), acct, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.count != 2 {
		t.Fatalf("piece count = %d, want 2", m.count)
	}
	start, end := pieceRange(acct, m.run)
	if got := countRange(t, be, start, end); got != 2 {
		t.Fatalf("stored pieces = %d, want 2", got)
	}
}

// TestRealPieceSizeBoundary exercises the true default piece size once, so
// the shrunk-piece tests are not the only coverage of the boundary logic.
func TestRealPieceSizeBoundary(t *testing.T) {
	s, _ := newStore(t)
	acct := jmap.Id("Aone")
	data := fill(tuning.BlobPieceSize + 1) // two pieces at the real size
	id := writeStream(t, s, acct, data)
	if got := readAll(t, s, acct, id); !bytes.Equal(got, data) {
		t.Fatal("round trip mismatch at the real piece size")
	}
}

// TestDedupStoresOneCopy: uploading identical content twice returns the same
// id and leaves exactly one stored copy with no leftover markers (RFC 8620
// section 6.1).
func TestDedupStoresOneCopy(t *testing.T) {
	smallPieces(t, 64)
	s, be := newStore(t)
	acct := jmap.Id("Aone")
	data := fill(200) // four pieces
	id1 := writeStream(t, s, acct, data)
	id2 := writeStream(t, s, acct, data)
	if id1 != id2 {
		t.Fatalf("identical content produced different ids: %q vs %q", id1, id2)
	}
	// Only the winner's four pieces remain across the whole account.
	start, end := acctPieceRange(acct)
	if got := countRange(t, be, start, end); got != 4 {
		t.Fatalf("account holds %d pieces, want 4 (dedup left a second copy)", got)
	}
	ms, me := markerRange()
	if got := countRange(t, be, ms, me); got != 0 {
		t.Fatalf("markers left after dedup: %d", got)
	}
	if got := readAll(t, s, acct, id1); !bytes.Equal(got, data) {
		t.Fatal("deduplicated blob did not read back")
	}
}

// TestAbortLeavesNothing: aborting a partially written blob removes its
// pieces and marker, and the writer refuses further use.
func TestAbortLeavesNothing(t *testing.T) {
	smallPieces(t, 64)
	s, be := newStore(t)
	acct := jmap.Id("Aone")
	data := fill(200)

	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	// Nothing addressable remains.
	if _, _, err := s.Open(context.Background(), acct, blob.IdFor(data)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("aborted blob opens: %v", err)
	}
	start, end := acctPieceRange(acct)
	if got := countRange(t, be, start, end); got != 0 {
		t.Fatalf("aborted blob left %d pieces", got)
	}
	ms, me := markerRange()
	if got := countRange(t, be, ms, me); got != 0 {
		t.Fatalf("aborted blob left %d markers", got)
	}
	// A finalized writer refuses further use.
	if _, err := w.Write(data); err == nil {
		t.Fatal("write after abort must error")
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("commit after abort must error")
	}
}

// TestCommitFinalizes: a second Commit is refused and a deferred Abort after
// a successful Commit is a harmless no-op error, matching the BlobWriter
// contract.
func TestCommitFinalizes(t *testing.T) {
	s, _ := newStore(t)
	acct := jmap.Id("Aone")
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("second commit must error")
	}
	if err := w.Abort(); err == nil {
		t.Fatal("abort after commit must error")
	}
}

// TestDeleteAndNamespace: delete removes a blob idempotently, and the same
// content in another account is independent (per-account namespace).
func TestDeleteAndNamespace(t *testing.T) {
	smallPieces(t, 64)
	s, be := newStore(t)
	acct, other := jmap.Id("Aone"), jmap.Id("Atwo")
	data := fill(200)

	id := writeStream(t, s, acct, data)
	otherID := writeStream(t, s, other, data)
	if id != otherID {
		t.Fatal("content address should not depend on the account")
	}
	// The two accounts hold independent copies.
	if _, _, err := s.Open(context.Background(), other, id); err != nil {
		t.Fatalf("other account cannot open its own blob: %v", err)
	}

	if err := s.Delete(context.Background(), acct, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(context.Background(), acct, id); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("deleted blob opens: %v", err)
	}
	// The other account is untouched.
	if got := readAll(t, s, other, id); !bytes.Equal(got, data) {
		t.Fatal("deleting one account's blob affected another")
	}
	start, end := acctPieceRange(acct)
	if got := countRange(t, be, start, end); got != 0 {
		t.Fatalf("deleted blob left %d pieces", got)
	}
	// Double delete succeeds.
	if err := s.Delete(context.Background(), acct, id); err != nil {
		t.Fatalf("double delete: %v", err)
	}
}

// TestOpenMissing: opening an unknown blob reports ErrNotFound.
func TestOpenMissing(t *testing.T) {
	s, _ := newStore(t)
	if _, _, err := s.Open(context.Background(), "Aone", blob.IdFor([]byte("nope"))); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("missing blob: %v", err)
	}
}

// TestPutRoundTripIdempotent: Put stores in-hand content under a known id,
// reads back exactly, and is an idempotent no-op the second time.
func TestPutRoundTripIdempotent(t *testing.T) {
	smallPieces(t, 64)
	s, be := newStore(t)
	acct := jmap.Id("Aone")
	data := fill(200)
	id := blob.IdFor(data)

	if err := s.Put(context.Background(), acct, id, data); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, s, acct, id); !bytes.Equal(got, data) {
		t.Fatal("Put round trip mismatch")
	}
	if err := s.Put(context.Background(), acct, id, data); err != nil {
		t.Fatal(err)
	}
	start, end := acctPieceRange(acct)
	if got := countRange(t, be, start, end); got != 4 {
		t.Fatalf("idempotent Put left %d pieces, want 4", got)
	}
	// Empty content through Put.
	empty := blob.IdFor(nil)
	if err := s.Put(context.Background(), acct, empty, nil); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, s, acct, empty); len(got) != 0 {
		t.Fatalf("empty Put read back %d bytes", len(got))
	}
}

// TestWriteFullBackendTempFails: when the backend is at capacity a piece
// write fails; aborting then leaves nothing behind (chaos: a store that
// fills mid-upload must not orphan bytes).
func TestWriteFullBackendTempFails(t *testing.T) {
	smallPieces(t, 8)
	// Room for the marker and a couple of pieces, not the whole blob.
	s, be := newStore(t, memory.WithCapacity(40))
	acct := jmap.Id("Aone")

	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	_, werr := w.Write(fill(200))
	if werr == nil {
		t.Fatal("expected a write to fail once the backend is full")
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("Abort after a full backend: %v", err)
	}
	start, end := acctPieceRange(acct)
	if got := countRange(t, be, start, end); got != 0 {
		t.Fatalf("full-backend abort left %d pieces", got)
	}
	ms, me := markerRange()
	if got := countRange(t, be, ms, me); got != 0 {
		t.Fatalf("full-backend abort left %d markers", got)
	}
}

// TestCorruptManifestOpen: a manifest value that this version did not write
// must make Open fail cleanly rather than panic or misread (chaos: a damaged
// or foreign value at a manifest key).
func TestCorruptManifestOpen(t *testing.T) {
	s, be := newStore(t)
	acct, id := jmap.Id("Aone"), blob.IdFor([]byte("x"))
	b := &backend.Batch{}
	b.Set(manifestKey(acct, id), []byte("not a manifest"))
	if err := be.WriteBatch(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(context.Background(), acct, id); err == nil {
		t.Fatal("Open accepted a corrupt manifest")
	} else if errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("corrupt manifest reported as not found: %v", err)
	}
}

// TestMissingPieceOpen: if a piece named by the manifest is gone, reading
// stops with a clean error at that point instead of silently truncating
// (chaos: partial data loss under a valid manifest).
func TestMissingPieceOpen(t *testing.T) {
	smallPieces(t, 64)
	s, be := newStore(t)
	acct := jmap.Id("Aone")
	data := fill(200)
	id := writeStream(t, s, acct, data)
	m, err := s.loadManifest(context.Background(), acct, id)
	if err != nil {
		t.Fatal(err)
	}
	// Remove the second piece.
	b := &backend.Batch{}
	b.Delete(pieceKey(acct, m.run, 1))
	if err := be.WriteBatch(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	rc, _, err := s.Open(context.Background(), acct, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("reading a blob with a missing piece must error")
	}
}

// TestSweepReclaimsCrashOrphan simulates a crash: pieces and a marker are
// written but never committed, then a fresh Store (an empty live set, as
// after a restart) reclaims them at construction, leaving a separately
// committed blob intact.
func TestSweepReclaimsCrashOrphan(t *testing.T) {
	smallPieces(t, 64)
	be := memory.New()
	clk := time.Unix(1_720_000_000, 0)
	s := storeAt(be, &clk)
	acct := jmap.Id("Aone")
	survivor := writeStream(t, s, acct, fill(200))

	// An upload that streams pieces but never commits (the process dies).
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(fill(200)); err != nil {
		t.Fatal(err)
	}
	ms, me := markerRange()
	if got := countRange(t, be, ms, me); got != 1 {
		t.Fatalf("expected one in-flight marker, got %d", got)
	}

	// Before the window elapses the run is spared: it could still be a live
	// upload (in this or another process).
	clk = clk.Add(tuning.UploadReclaimWindow - time.Second)
	if n, err := s.Sweep(context.Background()); err != nil || n != 0 {
		t.Fatalf("premature sweep reclaimed a fresh run: n=%d err=%v", n, err)
	}
	// Past the window the un-refreshed marker is a crash orphan and its pieces
	// are reclaimed; the committed survivor (which carries no marker) is
	// untouched.
	clk = clk.Add(2 * time.Second)
	if n, err := s.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep of stale orphan: n=%d err=%v, want 1", n, err)
	}
	if got := countRange(t, be, ms, me); got != 0 {
		t.Fatalf("sweep left %d markers", got)
	}
	if got := readAll(t, s, acct, survivor); len(got) != 200 {
		t.Fatal("sweep destroyed a committed blob")
	}
	// Idempotent: a second sweep finds nothing.
	if n, err := s.Sweep(context.Background()); err != nil || n != 0 {
		t.Fatalf("second sweep: n=%d err=%v", n, err)
	}
}

// TestSweepSparesLiveWriter: a sweep within the window must not touch a run
// whose marker is still fresh; the writer must be able to commit afterwards.
func TestSweepSparesLiveWriter(t *testing.T) {
	smallPieces(t, 64)
	be := memory.New()
	clk := time.Unix(1_720_000_000, 0)
	s := storeAt(be, &clk)
	acct := jmap.Id("Aone")
	data := fill(200)

	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	clk = clk.Add(tuning.UploadReclaimWindow - time.Second)
	if n, err := s.Sweep(context.Background()); err != nil || n != 0 {
		t.Fatalf("sweep reclaimed a fresh upload: n=%d err=%v", n, err)
	}
	id, err := w.Commit()
	if err != nil {
		t.Fatalf("commit after sweep: %v", err)
	}
	if got := readAll(t, s, acct, id); !bytes.Equal(got, data) {
		t.Fatal("blob damaged by a concurrent sweep")
	}
}

// TestSweepStaleUploadCommitGuard: if a run goes stale and a sweep reclaims
// its pieces mid-upload, the writer's Commit must fail (errUploadReclaimed)
// rather than publish a manifest over deleted pieces - the finish-time guard.
// The client then retries into a clean state, and nothing is left behind.
func TestSweepStaleUploadCommitGuard(t *testing.T) {
	smallPieces(t, 64)
	be := memory.New()
	clk := time.Unix(1_720_000_000, 0)
	s := storeAt(be, &clk)
	acct := jmap.Id("Aone")

	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(fill(200)); err != nil {
		t.Fatal(err)
	}
	// The upload stalls past the window; a sweep reclaims its pieces.
	clk = clk.Add(tuning.UploadReclaimWindow + time.Second)
	if n, err := s.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep of stalled upload: n=%d err=%v, want 1", n, err)
	}
	if _, err := w.Commit(); !errors.Is(err, errUploadReclaimed) {
		t.Fatalf("commit after reclaim: err=%v, want errUploadReclaimed", err)
	}
	// Nothing published and no leftover pieces.
	ps, pe := acctPieceRange(acct)
	if got := countRange(t, be, ps, pe); got != 0 {
		t.Fatalf("reclaimed upload left %d pieces", got)
	}
}

// TestSweepMultiProcessSparesForeignLiveUpload is the case #3c exists for: a
// second store sharing the backend (a second process) must not reclaim
// another store's in-progress upload. The two share no in-memory state, so
// safety rests entirely on the marker stamp the writing store keeps fresh -
// which is exactly what the old in-memory live-set could not provide across
// processes.
func TestSweepMultiProcessSparesForeignLiveUpload(t *testing.T) {
	smallPieces(t, 64)
	be := memory.New()
	clk := time.Unix(1_720_000_000, 0)
	writerStore := storeAt(be, &clk)
	sweeperStore := storeAt(be, &clk) // a different "process" over the same backend
	acct := jmap.Id("Aone")
	ctx := context.Background()

	content := fill(500)
	w, err := writerStore.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content[:200]); err != nil {
		t.Fatal(err)
	}
	// Time passes but the writer keeps working: the next write, past the
	// refresh interval, re-stamps the marker.
	clk = clk.Add(tuning.UploadRefreshInterval + time.Second)
	if _, err := w.Write(content[200:]); err != nil {
		t.Fatal(err)
	}
	// More time passes - enough that the run is older than the window measured
	// from Create, but the last refresh is recent. The foreign sweeper reads
	// the fresh stamp and must spare the run.
	clk = clk.Add(tuning.UploadReclaimWindow - time.Second)
	if n, err := sweeperStore.Sweep(ctx); err != nil || n != 0 {
		t.Fatalf("foreign sweep reclaimed a live upload: n=%d err=%v", n, err)
	}
	// The writer commits its blob intact, proving the sweep did no damage.
	id, err := w.Commit()
	if err != nil {
		t.Fatalf("commit after foreign sweep: %v", err)
	}
	if got := readAll(t, sweeperStore, acct, id); !bytes.Equal(got, content) {
		t.Fatal("blob damaged by a foreign sweep")
	}
}

// TestStreamingCopyBetweenAccounts mirrors how Blob/copy moves a blob: the
// source is opened as a stream and written into another account through a
// fresh writer, holding only one piece at a time. The copy must be
// byte-identical, share the content-addressed id, and be independent of the
// source (deleting one leaves the other).
func TestStreamingCopyBetweenAccounts(t *testing.T) {
	smallPieces(t, 64)
	s, be := newStore(t)
	src, dst := jmap.Id("Asrc"), jmap.Id("Adst")
	data := fill(200) // four pieces
	id := writeStream(t, s, src, data)

	rc, _, err := s.Open(context.Background(), src, id)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Create(context.Background(), dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, rc); err != nil {
		t.Fatal(err)
	}
	rc.Close()
	newID, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if newID != id {
		t.Fatalf("copied id %q, want the source id %q", newID, id)
	}
	if got := readAll(t, s, dst, id); !bytes.Equal(got, data) {
		t.Fatal("copy is not byte-identical")
	}
	// The destination holds its own pieces, independent of the source.
	start, end := acctPieceRange(dst)
	if got := countRange(t, be, start, end); got != 4 {
		t.Fatalf("destination holds %d pieces, want 4", got)
	}
	if err := s.Delete(context.Background(), src, id); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, s, dst, id); !bytes.Equal(got, data) {
		t.Fatal("deleting the source destroyed the copy")
	}
}

// TestConcurrentIdenticalUploads: many writers storing identical content at
// once must converge on one id and one stored copy, with no leftover
// markers. Run under -race, this exercises the manifest assert that breaks
// the tie.
func TestConcurrentIdenticalUploads(t *testing.T) {
	smallPieces(t, 64)
	s, be := newStore(t)
	acct := jmap.Id("Aone")
	data := fill(200) // four pieces

	const n = 12
	ids := make([]jmap.Id, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, err := s.Create(context.Background(), acct)
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			if _, err := w.Write(data); err != nil {
				t.Errorf("Write: %v", err)
				return
			}
			id, err := w.Commit()
			if err != nil {
				t.Errorf("Commit: %v", err)
				return
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()

	want := blob.IdFor(data)
	for i, id := range ids {
		if id != want {
			t.Fatalf("upload %d got id %q, want %q", i, id, want)
		}
	}
	// Exactly one winner's pieces remain; the losers discarded theirs.
	start, end := acctPieceRange(acct)
	if got := countRange(t, be, start, end); got != 4 {
		t.Fatalf("account holds %d pieces after the race, want 4", got)
	}
	ms, me := markerRange()
	if got := countRange(t, be, ms, me); got != 0 {
		t.Fatalf("race left %d markers", got)
	}
	if got := readAll(t, s, acct, want); !bytes.Equal(got, data) {
		t.Fatal("blob unreadable after the race")
	}
}

// readSoleMarker returns the value of the single run marker in the backend,
// failing the test if there is not exactly one.
func readSoleMarker(t *testing.T, be backend.Backend) []byte {
	t.Helper()
	var vals [][]byte
	ms, me := markerRange()
	if err := be.Scan(context.Background(), ms, me, false, func(_, value []byte) bool {
		v := make([]byte, len(value))
		copy(v, value)
		vals = append(vals, v)
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(vals) != 1 {
		t.Fatalf("expected exactly one marker, got %d", len(vals))
	}
	return vals[0]
}

// TestFlushRestampsMarker: every stored piece re-stamps the run marker in the
// same batch, so a run that is making progress always carries a marker value a
// Sweep's scan has not seen. This is what lets the reclaim batch's assert
// (reclaimRun) detect progress made after the scan.
func TestFlushRestampsMarker(t *testing.T) {
	smallPieces(t, 64)
	be := memory.New()
	clk := time.Unix(1_720_000_000, 0)
	s := storeAt(be, &clk)
	acct := jmap.Id("Aone")

	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(fill(64)); err != nil { // exactly one piece: one flush
		t.Fatal(err)
	}
	before := readSoleMarker(t, be)
	clk = clk.Add(time.Second)
	if _, err := w.Write(fill(64)); err != nil { // second piece, second flush
		t.Fatal(err)
	}
	after := readSoleMarker(t, be)
	if bytes.Equal(before, after) {
		t.Fatal("storing a piece did not change the run marker value")
	}
	if _, _, stamp, ok := decodeMarker(after); !ok || !stamp.Equal(clk) {
		t.Fatalf("marker stamp = %v ok=%v, want the flush-time clock %v", stamp, ok, clk)
	}
}

// TestSweepSparesRunRevivedAfterScan pins the scan-to-delete race closed by
// reclaimRun's marker assert: a Sweep judges a run stale, but before its
// delete lands the writer stores another piece (re-stamping the marker). The
// reclaim must then fail its assert and spare the run - deleting it would
// also delete the marker while missing the new piece, leaving that piece
// unreachable by any later Sweep. The writer then commits normally.
func TestSweepSparesRunRevivedAfterScan(t *testing.T) {
	smallPieces(t, 64)
	be := memory.New()
	clk := time.Unix(1_720_000_000, 0)
	s := storeAt(be, &clk)
	acct := jmap.Id("Aone")
	data := fill(200)

	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data[:128]); err != nil { // two pieces stored
		t.Fatal(err)
	}
	// The run goes stale; a Sweep's scan reads the marker as it is now.
	clk = clk.Add(tuning.UploadReclaimWindow + time.Second)
	scanned := readSoleMarker(t, be)
	// Between that scan and the reclaim, the writer stores another piece,
	// which re-stamps the marker in the same batch.
	if _, err := w.Write(data[128:]); err != nil {
		t.Fatal(err)
	}
	// The reclaim built from the scan must fail its assert and delete nothing.
	acctID, run, _, ok := decodeMarker(scanned)
	if !ok {
		t.Fatal("scanned marker did not decode")
	}
	if err := s.reclaimRun(context.Background(), acctID, run, scanned); !errors.Is(err, backend.ErrAssertFailed) {
		t.Fatalf("reclaim of a revived run: err=%v, want ErrAssertFailed", err)
	}
	ps, pe := acctPieceRange(acct)
	if got := countRange(t, be, ps, pe); got != 3 {
		t.Fatalf("revived run has %d pieces, want 3 intact", got)
	}
	id, err := w.Commit()
	if err != nil {
		t.Fatalf("commit after spared reclaim: %v", err)
	}
	if got := readAll(t, s, acct, id); !bytes.Equal(got, data) {
		t.Fatal("blob damaged by the spared reclaim")
	}
}
