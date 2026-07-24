package objectdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// docType references blobs: the target for the reference index tests.
func docType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "TestDoc",
		Capability: "https://naust.email/test/docs",
		Properties: map[string]descriptor.Property{
			"title":  {Kind: descriptor.KindString},
			"blobId": {Kind: descriptor.KindId, BlobRef: true},
		},
	}
}

func newBlobDB(t *testing.T) (*DB, blob.Store) {
	t.Helper()
	be := memory.New()
	db := New(be, lease.NewInProcess(be), WithVerifyPreImages())
	if err := db.RegisterType(docType()); err != nil {
		t.Fatal(err)
	}
	return db, kvstore.New(be)
}

// uploadBlob stores content and records it, like the upload endpoint: it
// streams the bytes into a writer and finalizes them (record then publish).
func uploadBlob(t *testing.T, db *DB, store blob.Store, content, uploader string, at time.Time) jmap.Id {
	t.Helper()
	ctx := context.Background()
	bw, err := store.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(bw, content); err != nil {
		t.Fatal(err)
	}
	blobID, err := db.FinalizeBlobUpload(ctx, acct, bw, uploader, at)
	if err != nil {
		t.Fatal(err)
	}
	return blobID
}

func docFor(blobID jmap.Id) Object {
	raw, _ := json.Marshal(blobID)
	return Object{"title": json.RawMessage(`"doc"`), "blobId": raw}
}

func TestBlobUploadRecord(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()
	t0 := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)

	blobID := uploadBlob(t, db, store, "hello", "alice", t0)
	rec, err := db.BlobUpload(ctx, acct, blobID)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.UploadedAt.Equal(t0) || !slices.Equal(rec.Uploaders, []string{"alice"}) {
		t.Fatalf("record = %+v", rec)
	}

	// Reupload by another user: uploader added, expiry clock reset
	// (RFC 8620 section 6: reupload SHOULD reset the expiry time).
	uploadBlob(t, db, store, "hello", "bob", t0.Add(time.Minute))
	rec, err = db.BlobUpload(ctx, acct, blobID)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.UploadedAt.Equal(t0.Add(time.Minute)) || !slices.Equal(rec.Uploaders, []string{"alice", "bob"}) {
		t.Fatalf("after reupload: %+v", rec)
	}

	if _, err := db.BlobUpload(ctx, acct, "Gnope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown blob -> %v, want ErrNotFound", err)
	}

	// Update.BlobExists sees the record.
	_, err = db.Update(ctx, acct, func(u *Update) error {
		if ok, err := u.BlobExists(blobID); err != nil || !ok {
			t.Errorf("BlobExists(%s) = %v, %v", blobID, ok, err)
		}
		if ok, err := u.BlobExists("Gnope"); err != nil || ok {
			t.Errorf("BlobExists(Gnope) = %v, %v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The reference index follows create, update, and destroy in-commit.
func TestBlobReferenceIndex(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()
	now := time.Now()
	b1 := uploadBlob(t, db, store, "one", "alice", now)
	b2 := uploadBlob(t, db, store, "two", "alice", now)

	referenced := func(b jmap.Id) bool {
		t.Helper()
		ok, err := db.BlobReferenced(ctx, acct, b)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if referenced(b1) || referenced(b2) {
		t.Fatal("fresh uploads must be unreferenced")
	}

	var id jmap.Id
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		var err error
		id, err = u.Create("TestDoc", docFor(b1))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !referenced(b1) || referenced(b2) {
		t.Fatal("create must reference b1 only")
	}

	// Update the doc to point at b2: b1 dereferenced, b2 referenced,
	// atomically with the object write.
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		obj, err := u.Get("TestDoc", id)
		if err != nil {
			return err
		}
		next := make(Object, len(obj))
		for k, v := range obj {
			next[k] = v
		}
		raw, _ := json.Marshal(b2)
		next["blobId"] = raw
		return u.Put("TestDoc", id, next)
	}); err != nil {
		t.Fatal(err)
	}
	if referenced(b1) || !referenced(b2) {
		t.Fatal("update must move the reference to b2")
	}

	if _, err := db.Update(ctx, acct, func(u *Update) error {
		return u.Destroy("TestDoc", id)
	}); err != nil {
		t.Fatal(err)
	}
	if referenced(b2) {
		t.Fatal("destroy must drop the reference")
	}
}

// SweepBlobs: unreferenced past the grace window goes, referenced or
// fresh stays, and the 1-hour floor of section 6 always applies.
func TestSweepBlobs(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()
	now := time.Now()

	old := uploadBlob(t, db, store, "old and unreferenced", "alice", now.Add(-2*time.Hour))
	kept := uploadBlob(t, db, store, "old but referenced", "alice", now.Add(-2*time.Hour))
	fresh := uploadBlob(t, db, store, "fresh and unreferenced", "alice", now.Add(-10*time.Minute))
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		_, err := u.Create("TestDoc", docFor(kept))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// grace 0 still means the 1-hour MUST NOT floor: fresh survives.
	deleted, _, err := db.SweepBlobs(ctx, acct, store, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []jmap.Id{old}) {
		t.Fatalf("deleted = %v, want [%s]", deleted, old)
	}
	if _, err := db.BlobUpload(ctx, acct, old); !errors.Is(err, ErrNotFound) {
		t.Error("swept blob still has a record")
	}
	if _, _, err := store.Open(ctx, acct, old); !errors.Is(err, blob.ErrNotFound) {
		t.Error("swept blob still has content")
	}
	for _, b := range []jmap.Id{kept, fresh} {
		if _, err := db.BlobUpload(ctx, acct, b); err != nil {
			t.Errorf("blob %s should have survived: %v", b, err)
		}
	}

	// A wider grace keeps even old unreferenced blobs.
	old2 := uploadBlob(t, db, store, "another old one", "alice", now.Add(-2*time.Hour))
	deleted, _, err = db.SweepBlobs(ctx, acct, store, now, 3*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted = %v, want none under 3h grace", deleted)
	}
	if _, err := db.BlobUpload(ctx, acct, old2); err != nil {
		t.Errorf("old2 should survive 3h grace: %v", err)
	}
}

// pendingHints lists the sweep candidate range: the exact set of blobs
// the next SweepBlobs call will inspect.
func pendingHints(t *testing.T, db *DB) []jmap.Id {
	t.Helper()
	var out []jmap.Id
	start, end := prefixRange(seg(string(acct)), seg("p"))
	if err := db.be.Scan(context.Background(), start, end, false, func(k, _ []byte) bool {
		out = append(out, idFromObjKey(k))
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// The pending hints follow a blob through its life: set by the upload,
// cleared by the commit that references it, restored by the commit that
// removes the reference. A referenced blob therefore costs the sweep
// nothing at all - the candidate set holds only blobs whose collection
// is genuinely in question.
func TestBlobPendingHintLifecycle(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()
	now := time.Now()

	b := uploadBlob(t, db, store, "payload", "alice", now.Add(-2*time.Hour))
	if hints := pendingHints(t, db); !slices.Equal(hints, []jmap.Id{b}) {
		t.Fatalf("hints after upload = %v, want [%s]", hints, b)
	}

	var id jmap.Id
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		var err error
		id, err = u.Create("TestDoc", docFor(b))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if hints := pendingHints(t, db); len(hints) != 0 {
		t.Fatalf("hints after referencing = %v, want none", hints)
	}

	// No hint: the sweep inspects nothing, however old the blob is.
	deleted, _, err := db.SweepBlobs(ctx, acct, store, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("sweep of a fully referenced account deleted %v", deleted)
	}

	if _, err := db.Update(ctx, acct, func(u *Update) error {
		return u.Destroy("TestDoc", id)
	}); err != nil {
		t.Fatal(err)
	}
	if hints := pendingHints(t, db); !slices.Equal(hints, []jmap.Id{b}) {
		t.Fatalf("hints after destroy = %v, want [%s]", hints, b)
	}

	// The upload is old, the last reference just left: collectable now
	// (the section 6 MUST NOT covers only the removing method call).
	deleted, _, err = db.SweepBlobs(ctx, acct, store, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []jmap.Id{b}) {
		t.Fatalf("deleted = %v, want [%s]", deleted, b)
	}
	if _, err := db.BlobUpload(ctx, acct, b); !errors.Is(err, ErrNotFound) {
		t.Error("swept blob still has a record")
	}
	if _, _, err := store.Open(ctx, acct, b); !errors.Is(err, blob.ErrNotFound) {
		t.Error("swept blob still has content")
	}
	if hints := pendingHints(t, db); len(hints) != 0 {
		t.Errorf("hints after sweep = %v, want none", hints)
	}
}

// A hint is a nomination, never a verdict: destroying one of two docs
// sharing a blob leaves a hint on a still-referenced blob, and the
// sweep must drop the hint and spare the blob.
func TestSweepDropsStaleHint(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()
	now := time.Now()

	b := uploadBlob(t, db, store, "shared", "alice", now.Add(-2*time.Hour))
	var d1 jmap.Id
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		var err error
		if d1, err = u.Create("TestDoc", docFor(b)); err != nil {
			return err
		}
		_, err = u.Create("TestDoc", docFor(b))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		return u.Destroy("TestDoc", d1)
	}); err != nil {
		t.Fatal(err)
	}
	if hints := pendingHints(t, db); !slices.Equal(hints, []jmap.Id{b}) {
		t.Fatalf("hints after partial destroy = %v, want [%s]", hints, b)
	}

	deleted, _, err := db.SweepBlobs(ctx, acct, store, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("sweep deleted %v; the blob is still referenced", deleted)
	}
	if _, err := db.BlobUpload(ctx, acct, b); err != nil {
		t.Fatalf("referenced blob lost its record: %v", err)
	}
	if hints := pendingHints(t, db); len(hints) != 0 {
		t.Errorf("stale hint not dropped: %v", hints)
	}
}

// A hint with no upload record behind it (torn state) is cleaned up
// without an error and without touching anything else.
func TestSweepCleansOrphanHint(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()

	batch := &backend.Batch{}
	batch.Set(pendingKey(acct, "Bghost"), nil)
	if err := db.be.WriteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}

	deleted, _, err := db.SweepBlobs(ctx, acct, store, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("orphan hint produced deletions: %v", deleted)
	}
	if hints := pendingHints(t, db); len(hints) != 0 {
		t.Errorf("orphan hint survived: %v", hints)
	}
}

// A backlog larger than one call's bound is processed in bounded slices
// and fully drained by repeated calls - the shape a mass destroy leaves.
func TestSweepBacklogBoundedAndDrained(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()
	now := time.Now()

	const n = maxSweepPerCall + 176
	for i := 0; i < n; i++ {
		uploadBlob(t, db, store, fmt.Sprintf("bulk-%d", i), "alice", now.Add(-2*time.Hour))
	}

	deleted, more, err := db.SweepBlobs(ctx, acct, store, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != maxSweepPerCall {
		t.Fatalf("first call deleted %d, want the %d bound", len(deleted), maxSweepPerCall)
	}
	if !more {
		t.Fatal("first call hit the bound with a backlog left but reported no more")
	}
	if hints := pendingHints(t, db); len(hints) != n-maxSweepPerCall {
		t.Fatalf("hints after first call = %d, want %d", len(hints), n-maxSweepPerCall)
	}

	total := len(deleted)
	for more {
		var batch []jmap.Id
		batch, more, err = db.SweepBlobs(ctx, acct, store, now, 0)
		if err != nil {
			t.Fatal(err)
		}
		total += len(batch)
	}
	if total != n {
		t.Fatalf("drained %d blobs, want %d", total, n)
	}
	if hints := pendingHints(t, db); len(hints) != 0 {
		t.Errorf("hints remain after drain: %d", len(hints))
	}
}

// failDeleteStore fails Delete for one blob a limited number of times,
// simulating a store outage mid-sweep.
type failDeleteStore struct {
	blob.Store
	failOn jmap.Id
	fails  int
}

var errStoreDown = errors.New("injected store failure")

func (s *failDeleteStore) Delete(ctx context.Context, a, b jmap.Id) error {
	if s.fails > 0 && b == s.failOn {
		s.fails--
		return errStoreDown
	}
	return s.Store.Delete(ctx, a, b)
}

// A sweep that dies mid-pass (store failure after some content is gone)
// leaves records and hints intact - the batch never commits - and the
// next sweep finishes the job, re-deleting already-gone content without
// complaint (the Store contract makes Delete of a missing blob a no-op).
func TestSweepRecoversFromStoreFailure(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()
	now := time.Now()

	b1 := uploadBlob(t, db, store, "first", "alice", now.Add(-2*time.Hour))
	b2 := uploadBlob(t, db, store, "second", "alice", now.Add(-2*time.Hour))
	// Candidates are visited in key order; fail on the later one so the
	// earlier one's content is already gone when the pass dies.
	first, second := b1, b2
	if second < first {
		first, second = second, first
	}
	failing := &failDeleteStore{Store: store, failOn: second, fails: 1}

	if _, _, err := db.SweepBlobs(ctx, acct, failing, now, 0); !errors.Is(err, errStoreDown) {
		t.Fatalf("sweep error = %v, want the injected failure", err)
	}
	// Nothing committed: both records and both hints still present, and
	// only the first blob's content is gone.
	for _, b := range []jmap.Id{first, second} {
		if _, err := db.BlobUpload(ctx, acct, b); err != nil {
			t.Fatalf("record for %s lost by a failed sweep: %v", b, err)
		}
	}
	if hints := pendingHints(t, db); len(hints) != 2 {
		t.Fatalf("hints after failed sweep = %v, want both", hints)
	}
	if _, _, err := store.Open(ctx, acct, first); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("first blob's content should be gone: %v", err)
	}

	deleted, _, err := db.SweepBlobs(ctx, acct, failing, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 {
		t.Fatalf("recovery sweep deleted %v, want both", deleted)
	}
	for _, b := range []jmap.Id{first, second} {
		if _, err := db.BlobUpload(ctx, acct, b); !errors.Is(err, ErrNotFound) {
			t.Errorf("record for %s survived recovery: %v", b, err)
		}
	}
	if hints := pendingHints(t, db); len(hints) != 0 {
		t.Errorf("hints after recovery = %v, want none", hints)
	}
}

// TestSweepHintCoverageChaos drives a scripted pseudo-random history of
// uploads, references, re-references, destroys, and interleaved sweeps,
// then checks the hint mechanism against first principles: after a
// final drain, every still-referenced blob has its record and content
// (a hint never deleted a live blob) and every unreferenced blob is
// fully reclaimed with no hint left behind (a transition never escaped
// the hint set, so nothing leaks).
func TestSweepHintCoverageChaos(t *testing.T) {
	db, store := newBlobDB(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(1))
	clock := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	var blobs []jmap.Id              // every blob ever uploaded
	docBlob := map[jmap.Id]jmap.Id{} // live doc -> blob it references
	var docs []jmap.Id

	newDoc := func(b jmap.Id) {
		var id jmap.Id
		if _, err := db.Update(ctx, acct, func(u *Update) error {
			var err error
			id, err = u.Create("TestDoc", docFor(b))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		docBlob[id] = b
		docs = append(docs, id)
	}
	repoint := func(id, b jmap.Id) {
		if _, err := db.Update(ctx, acct, func(u *Update) error {
			obj, err := u.Get("TestDoc", id)
			if err != nil {
				return err
			}
			next := make(Object, len(obj))
			for k, v := range obj {
				next[k] = v
			}
			raw, _ := json.Marshal(b)
			next["blobId"] = raw
			return u.Put("TestDoc", id, next)
		}); err != nil {
			t.Fatal(err)
		}
		docBlob[id] = b
	}
	// liveBlob picks a random blob that still exists (unreferenced ones
	// may have been swept mid-history).
	liveBlob := func() (jmap.Id, bool) {
		for range 8 {
			b := blobs[rng.Intn(len(blobs))]
			if _, err := db.BlobUpload(ctx, acct, b); err == nil {
				return b, true
			}
		}
		return "", false
	}

	for i := 0; i < 400; i++ {
		op := rng.Intn(10)
		switch {
		case op < 3 || len(blobs) == 0:
			b := uploadBlob(t, db, store, fmt.Sprintf("content-%d", i), "alice", clock)
			blobs = append(blobs, b)
		case op < 6:
			if b, ok := liveBlob(); ok {
				newDoc(b)
			}
		case op < 8 && len(docs) > 0:
			if b, ok := liveBlob(); ok {
				repoint(docs[rng.Intn(len(docs))], b)
			}
		case op < 9 && len(docs) > 0:
			i := rng.Intn(len(docs))
			id := docs[i]
			if _, err := db.Update(ctx, acct, func(u *Update) error {
				return u.Destroy("TestDoc", id)
			}); err != nil {
				t.Fatal(err)
			}
			delete(docBlob, id)
			docs[i] = docs[len(docs)-1]
			docs = docs[:len(docs)-1]
		default:
			if _, _, err := db.SweepBlobs(ctx, acct, store, clock, 0); err != nil {
				t.Fatal(err)
			}
		}
		clock = clock.Add(7 * time.Minute)
	}

	// Age everything past the grace floor and drain.
	clock = clock.Add(2 * time.Hour)
	for {
		deleted, more, err := db.SweepBlobs(ctx, acct, store, clock, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(deleted) == 0 && !more {
			break
		}
	}

	refCount := func(b jmap.Id) int {
		n := 0
		for _, rb := range docBlob {
			if rb == b {
				n++
			}
		}
		return n
	}
	for _, b := range blobs {
		_, recErr := db.BlobUpload(ctx, acct, b)
		_, _, openErr := store.Open(ctx, acct, b)
		if refCount(b) > 0 {
			if recErr != nil || openErr != nil {
				t.Errorf("referenced blob %s harmed: record=%v open=%v", b, recErr, openErr)
			}
		} else {
			if !errors.Is(recErr, ErrNotFound) {
				t.Errorf("unreferenced blob %s leaked a record: %v", b, recErr)
			}
			if !errors.Is(openErr, blob.ErrNotFound) {
				t.Errorf("unreferenced blob %s leaked content: %v", b, openErr)
			}
		}
	}
	if hints := pendingHints(t, db); len(hints) != 0 {
		t.Errorf("hints remain after drain: %v", hints)
	}
}

// takeoverBackend simulates a lease takeover landing between a sweep's
// reference checks and its commit: when armed, the next WriteBatch that
// both asserts and deletes (the sweep's fenced deletion batch; a lease
// acquire asserts but never deletes) first overwrites the asserted key,
// as a new holder's claim swap would, so the assertion fails.
type takeoverBackend struct {
	backend.Backend
	armed bool
}

func (b *takeoverBackend) WriteBatch(ctx context.Context, batch *backend.Batch) error {
	if b.armed {
		deletes := false
		for _, op := range batch.Ops {
			if op.Kind == backend.OpDelete {
				deletes = true
				break
			}
		}
		if deletes {
			for _, op := range batch.Ops {
				if op.Kind == backend.OpAssert {
					b.armed = false
					steal := &backend.Batch{}
					steal.Set(op.Key, []byte("taken over"))
					if err := b.Backend.WriteBatch(ctx, steal); err != nil {
						return err
					}
					break
				}
			}
		}
	}
	return b.Backend.WriteBatch(ctx, batch)
}

// On a store sharing the DB's backend, content deletion rides the
// sweep's fenced batch: a lease takeover landing before the commit
// aborts the WHOLE deletion - content included - instead of destroying
// content the new lease holder may already have re-referenced. The
// next sweep, under a fresh lease, finishes the job.
func TestSweepFencedDeletionAborts(t *testing.T) {
	ctx := context.Background()
	be := &takeoverBackend{Backend: memory.New()}
	db := New(be, lease.NewInProcess(be), WithVerifyPreImages())
	if err := db.RegisterType(docType()); err != nil {
		t.Fatal(err)
	}
	store := kvstore.New(be)
	if bd, ok := blob.Store(store).(blob.BatchDeleter); !ok || bd.DeleteBackend() != backend.Backend(be) {
		t.Fatal("test store must batch-delete over the DB's own backend")
	}

	now := time.Now()
	doomed := uploadBlob(t, db, store, "doomed content", "alice", now.Add(-2*time.Hour))

	be.armed = true
	if _, _, err := db.SweepBlobs(ctx, acct, store, now, 0); err == nil {
		t.Fatal("sweep must fail when its fence is broken")
	}
	// Nothing was deleted: content, upload record, and hint all intact.
	if rc, _, err := store.Open(ctx, acct, doomed); err != nil {
		t.Fatalf("content deleted despite the aborted fence: %v", err)
	} else {
		rc.Close()
	}
	if _, err := db.BlobUpload(ctx, acct, doomed); err != nil {
		t.Fatalf("upload record lost by an aborted sweep: %v", err)
	}
	if hints := pendingHints(t, db); len(hints) != 1 {
		t.Fatalf("hints after aborted sweep = %v, want the one candidate", hints)
	}

	deleted, _, err := db.SweepBlobs(ctx, acct, store, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != doomed {
		t.Fatalf("recovery sweep deleted %v, want [%s]", deleted, doomed)
	}
	if _, _, err := store.Open(ctx, acct, doomed); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("content should be gone after the recovery sweep: %v", err)
	}
	if hints := pendingHints(t, db); len(hints) != 0 {
		t.Fatalf("hints remain after recovery: %v", hints)
	}
}
