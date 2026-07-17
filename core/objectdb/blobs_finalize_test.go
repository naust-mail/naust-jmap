package objectdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// stubWriter is a blob.BlobWriter whose Commit can be made to fail, so a
// test can drive the crash-between-record-and-publish window directly.
type stubWriter struct {
	id        jmap.Id
	commitErr error
	committed bool
}

func (w *stubWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *stubWriter) ID() jmap.Id                 { return w.id }
func (w *stubWriter) Commit() (jmap.Id, error) {
	w.committed = true
	return w.id, w.commitErr
}
func (w *stubWriter) Abort() error { return nil }

// TestFinalizeBlobUploadRecordFirst: the upload record is written before the
// content is published, so a failure (or crash) at publish leaves a record
// over reclaimable content rather than published bytes no record covers.
func TestFinalizeBlobUploadRecordFirst(t *testing.T) {
	db, _ := newBlobDB(t)
	ctx := context.Background()
	boom := errors.New("publish failed")
	bw := &stubWriter{id: "Grecordfirst", commitErr: boom}

	_, err := db.FinalizeBlobUpload(ctx, acct, bw, "alice", time.Now())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the publish failure", err)
	}
	if !bw.committed {
		t.Fatal("publish was never attempted")
	}
	// The record exists despite the failed publish (record-first ordering).
	rec, err := db.BlobUpload(ctx, acct, "Grecordfirst")
	if err != nil {
		t.Fatalf("record not written before publish: %v", err)
	}
	if !slices.Equal(rec.Uploaders, []string{"alice"}) {
		t.Fatalf("record = %+v", rec)
	}
}

// recordFaultBackend fails the WriteBatch that stores a specific upload
// record, leaving every other write (the lease bump, blob content) to
// succeed, so a test can drive a record-write failure in isolation.
type recordFaultBackend struct {
	backend.Backend
	failKey []byte
	err     error
}

func (f *recordFaultBackend) WriteBatch(ctx context.Context, b *backend.Batch) error {
	for _, op := range b.Ops {
		if op.Kind == backend.OpSet && bytes.Equal(op.Key, f.failKey) {
			return f.err
		}
	}
	return f.Backend.WriteBatch(ctx, b)
}

// TestFinalizeBlobUploadRecordFails: if the record write fails, the content
// is never published. Record-first means the failure aborts before Commit,
// so there is neither a record nor addressable content - the caller retries
// or reports the error, and nothing is stranded.
func TestFinalizeBlobUploadRecordFails(t *testing.T) {
	ctx := context.Background()
	body := "content that must not be published if its record fails"
	id := blob.IdFor([]byte(body))

	boom := errors.New("record write failed")
	be := &recordFaultBackend{Backend: memory.New(), failKey: uploadKey(acct, id), err: boom}
	db := New(be, lease.NewInProcess(be))
	store := kvstore.New(be)

	bw, err := store.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(bw, body)
	if _, err := db.FinalizeBlobUpload(ctx, acct, bw, "alice", time.Now()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the record failure", err)
	}
	// No record, and crucially no published content: the writer's Commit
	// was never reached.
	if _, err := db.BlobUpload(ctx, acct, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("record exists after a failed record write: %v", err)
	}
	if _, _, err := store.Open(ctx, acct, id); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("content published despite the record failing: %v", err)
	}
}

// TestFinalizeBlobUploadEmpty: a zero-byte upload finalizes to the
// empty-content address, is recorded, and reads back empty. Nothing in the
// path assumes at least one byte or one flushed piece.
func TestFinalizeBlobUploadEmpty(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()

	bw, err := store.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.FinalizeBlobUpload(ctx, acct, bw, "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if want := blob.IdFor(nil); id != want {
		t.Fatalf("empty id = %q, want %q", id, want)
	}
	rc, size, err := store.Open(ctx, acct, id)
	if err != nil {
		t.Fatalf("empty blob not published: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if size != 0 || len(got) != 0 {
		t.Fatalf("empty blob read back size=%d bytes=%q", size, got)
	}
	if _, err := db.BlobUpload(ctx, acct, id); err != nil {
		t.Fatalf("empty blob has no record: %v", err)
	}
}

// TestFinalizeBlobUploadConcurrentIdentical: two uploads of identical
// content finalized at once must converge - the account lease serializes the
// record read-modify-write, so both uploaders are recorded (never one lost)
// and one shared content copy is readable (dedup). Run under -race.
func TestFinalizeBlobUploadConcurrentIdentical(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()
	now := time.Now()
	body := "the very same bytes from two uploaders at once"
	want := blob.IdFor([]byte(body))

	finalize := func(uploader string) (jmap.Id, error) {
		bw, err := store.Create(ctx, acct)
		if err != nil {
			return "", err
		}
		io.WriteString(bw, body)
		return db.FinalizeBlobUpload(ctx, acct, bw, uploader, now)
	}

	var wg sync.WaitGroup
	ids := make([]jmap.Id, 2)
	errs := make([]error, 2)
	for i, who := range []string{"alice", "bob"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = finalize(who)
		}()
	}
	wg.Wait()

	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("finalize %d: %v", i, errs[i])
		}
		if ids[i] != want {
			t.Fatalf("finalize %d id = %q, want %q", i, ids[i], want)
		}
	}
	// Both uploaders recorded (neither read-modify-write clobbered the
	// other), and the shared content is present exactly once.
	rec, err := db.BlobUpload(ctx, acct, want)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rec.Uploaders, "alice") || !slices.Contains(rec.Uploaders, "bob") || len(rec.Uploaders) != 2 {
		t.Fatalf("uploaders = %v, want alice and bob", rec.Uploaders)
	}
	if _, _, err := store.Open(ctx, acct, want); err != nil {
		t.Fatalf("shared content missing: %v", err)
	}
}

// TestFinalizeBlobUpload: a streamed upload is recorded and published
// together; re-finalizing identical content deduplicates to the same id and
// resets the expiry with the new uploader added (RFC 8620 section 6.1).
func TestFinalizeBlobUpload(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()
	now := time.Now()
	body := "streamed body across the finalize"
	want := blob.IdFor([]byte(body))

	bw, err := store.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(bw, body)
	id, err := db.FinalizeBlobUpload(ctx, acct, bw, "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if id != want {
		t.Fatalf("id = %q, want content address %q", id, want)
	}
	// Content published and record present.
	rc, _, err := store.Open(ctx, acct, id)
	if err != nil {
		t.Fatalf("content not published: %v", err)
	}
	rc.Close()
	if _, err := db.BlobUpload(ctx, acct, id); err != nil {
		t.Fatalf("record missing: %v", err)
	}

	// Re-finalize identical content by another uploader: same id, refreshed.
	bw2, err := store.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(bw2, body)
	id2, err := db.FinalizeBlobUpload(ctx, acct, bw2, "bob", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id {
		t.Fatalf("dedup id = %q, want %q", id2, id)
	}
	rec, err := db.BlobUpload(ctx, acct, id)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.UploadedAt.Equal(now.Add(time.Minute).UTC().Truncate(time.Second)) ||
		!slices.Equal(rec.Uploaders, []string{"alice", "bob"}) {
		t.Fatalf("record after re-finalize = %+v", rec)
	}
}

// countingLeases wraps a Manager and counts Acquire calls, so a test can
// assert how many times a path queues for the account lease.
type countingLeases struct {
	lease.Manager
	n int
}

func (c *countingLeases) Acquire(ctx context.Context, account jmap.Id) (lease.Lease, error) {
	c.n++
	return c.Manager.Acquire(ctx, account)
}

// TestFinalizeBlobUploadThenUpdate: the merged path records the upload,
// publishes the content, and commits the update - all under ONE acquisition
// of the account lease, with the same end state the two separate calls leave.
func TestFinalizeBlobUploadThenUpdate(t *testing.T) {
	be := memory.New()
	lm := &countingLeases{Manager: lease.NewInProcess(be)}
	db := New(be, lm)
	if err := db.RegisterType(docType()); err != nil {
		t.Fatal(err)
	}
	store := kvstore.New(be)
	ctx := context.Background()
	body := "finalized and committed under one hold"
	want := blob.IdFor([]byte(body))

	bw, err := store.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(bw, body)

	var docID jmap.Id
	blobID, states, err := db.FinalizeBlobUploadThenUpdate(ctx, acct, bw, "alice", time.Now(), func(u *Update) error {
		raw, _ := json.Marshal(want)
		docID, err = u.Create("TestDoc", Object{"blobId": raw})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if blobID != want {
		t.Fatalf("blobID = %q, want %q", blobID, want)
	}
	if lm.n != 1 {
		t.Fatalf("lease acquired %d times, want 1", lm.n)
	}
	if states["TestDoc"] == "" {
		t.Fatalf("states = %v, want a TestDoc state", states)
	}
	if _, err := db.BlobUpload(ctx, acct, blobID); err != nil {
		t.Fatalf("record missing: %v", err)
	}
	if _, _, err := store.Open(ctx, acct, blobID); err != nil {
		t.Fatalf("content not published: %v", err)
	}
	if _, err := db.Get(ctx, acct, "TestDoc", docID); err != nil {
		t.Fatalf("update not committed: %v", err)
	}
}

// TestFinalizeBlobUploadThenUpdateFnFails: the finalize half completes before
// fn runs, so a failing update leaves the blob recorded and published (the
// non-empty blobId alongside the error says so) and commits nothing - exactly
// the state separate FinalizeBlobUpload-then-Update calls would leave.
func TestFinalizeBlobUploadThenUpdateFnFails(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()
	body := "published even though the update fails"
	want := blob.IdFor([]byte(body))

	bw, err := store.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(bw, body)

	boom := errors.New("update failed")
	var docID jmap.Id
	blobID, states, err := db.FinalizeBlobUploadThenUpdate(ctx, acct, bw, "alice", time.Now(), func(u *Update) error {
		raw, _ := json.Marshal(want)
		if docID, err = u.Create("TestDoc", Object{"blobId": raw}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the update failure", err)
	}
	if blobID != want {
		t.Fatalf("blobID = %q, want %q (finalized despite the failed update)", blobID, want)
	}
	if states != nil {
		t.Fatalf("states = %v, want none", states)
	}
	if _, err := db.BlobUpload(ctx, acct, blobID); err != nil {
		t.Fatalf("record missing: %v", err)
	}
	if _, _, err := store.Open(ctx, acct, blobID); err != nil {
		t.Fatalf("content not published: %v", err)
	}
	if _, err := db.Get(ctx, acct, "TestDoc", docID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("staged record committed despite fn failing: %v", err)
	}
}

// TestFinalizeBlobUploadThenUpdateRecordFails: a failed record write aborts
// before the content publishes and before fn ever runs - empty blobId, no
// record, no content, no commit.
func TestFinalizeBlobUploadThenUpdateRecordFails(t *testing.T) {
	ctx := context.Background()
	body := "nothing may happen if the record fails"
	id := blob.IdFor([]byte(body))

	boom := errors.New("record write failed")
	be := &recordFaultBackend{Backend: memory.New(), failKey: uploadKey(acct, id), err: boom}
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(docType()); err != nil {
		t.Fatal(err)
	}
	store := kvstore.New(be)

	bw, err := store.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(bw, body)

	ran := false
	blobID, _, err := db.FinalizeBlobUploadThenUpdate(ctx, acct, bw, "alice", time.Now(), func(u *Update) error {
		ran = true
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the record failure", err)
	}
	if blobID != "" {
		t.Fatalf("blobID = %q, want empty (nothing was finalized)", blobID)
	}
	if ran {
		t.Fatal("fn ran despite the finalize failing")
	}
	if _, _, err := store.Open(ctx, acct, id); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("content published despite the record failing: %v", err)
	}
}

// TestFinalizeBlobUploadVersusSweep: a finalize of content identical to an
// aged, unreferenced blob runs concurrently with SweepBlobs. Both take the
// account lease, so they serialize and the end state is always consistent:
// the record exists if and only if the content does, never a record over
// deleted content (the dedup-versus-sweep race the lease closes). Run under
// -race to exercise the interleavings.
func TestFinalizeBlobUploadVersusSweep(t *testing.T) {
	body := "aged unreferenced content"
	id := blob.IdFor([]byte(body))

	for round := 0; round < 50; round++ {
		db, store := newBlobDB(t)
		ctx := context.Background()
		now := time.Now()
		// Seed an aged, unreferenced copy: eligible for the sweep now.
		uploadBlob(t, db, store, body, "alice", now.Add(-2*time.Hour))

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = db.SweepBlobs(ctx, acct, store, now, tuning.BlobMinUnreferencedAge)
		}()
		go func() {
			defer wg.Done()
			bw, err := store.Create(ctx, acct)
			if err != nil {
				return
			}
			io.WriteString(bw, body)
			_, _ = db.FinalizeBlobUpload(ctx, acct, bw, "bob", now)
		}()
		wg.Wait()

		_, recErr := db.BlobUpload(ctx, acct, id)
		_, _, contentErr := store.Open(ctx, acct, id)
		hasRecord := recErr == nil
		hasContent := contentErr == nil
		if hasRecord != hasContent {
			t.Fatalf("round %d: record=%v content=%v, must agree", round, hasRecord, hasContent)
		}
	}
}
