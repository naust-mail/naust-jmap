package objectdb

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
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
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(docType()); err != nil {
		t.Fatal(err)
	}
	return db, kvstore.New(be)
}

// uploadBlob stores content and records it, like the upload endpoint.
func uploadBlob(t *testing.T, db *DB, store blob.Store, content, uploader string, at time.Time) jmap.Id {
	t.Helper()
	ctx := context.Background()
	blobID := blob.IdFor([]byte(content))
	if err := store.Put(ctx, acct, blobID, []byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordBlobUpload(ctx, acct, blobID, uploader, at); err != nil {
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
		raw, _ := json.Marshal(b2)
		obj["blobId"] = raw
		return u.Put("TestDoc", id, obj)
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
	deleted, err := db.SweepBlobs(ctx, acct, store, now, 0)
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
	deleted, err = db.SweepBlobs(ctx, acct, store, now, 3*time.Hour)
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
