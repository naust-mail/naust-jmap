package objectdb

// Blob metadata (RFC 8620 section 6). The blob.Store socket holds only
// bytes; everything the protocol needs to know ABOUT a blob lives here,
// in the same backend and consistency model as the objects:
//
//   - upload records ({acct} u {blobId}): existence, upload time, and
//     uploaders. A blob exists in an account iff its record exists.
//   - the reference index ({acct} r {blobId} {type} {id}): maintained
//     inside the same commit as the referencing object (buildBatch), so
//     "is this blob referenced?" can never disagree with the data.
//
// There are no reference counts: garbage collection is a sweep that
// deletes unreferenced blobs past a grace window (section 6 gives the
// rules; see SweepBlobs). Upload-before-reference is a normal transient
// state, and a missed sweep just runs again - self-healing beats
// precise.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
)

// MinUnreferencedAge is the floor on how long an unreferenced blob is
// kept: RFC 8620 section 6 says an unreferenced blob MUST NOT be
// deleted for at least 1 hour from the time of upload (except under
// quota pressure, which this runtime does not implement).
const MinUnreferencedAge = time.Hour

// BlobUpload is a blob's upload record.
type BlobUpload struct {
	// UploadedAt is the most recent upload time; a reupload of the same
	// content resets it (section 6: "if reuploaded, the same blobId MAY
	// be returned, but this SHOULD reset the expiry time").
	UploadedAt time.Time `json:"uploadedAt"`
	// Uploaders are the usernames that uploaded this content. Section
	// 6.1: unreferenced blobs MUST only be accessible to the uploader.
	Uploaders []string `json:"uploaders"`
}

// RecordBlobUpload registers an upload under the account's lease:
// creates the blob's record or, for a reupload of existing content,
// adds the uploader and resets the upload time.
func (db *DB) RecordBlobUpload(ctx context.Context, acct, blobID jmap.Id, uploader string, now time.Time) error {
	l, err := db.leases.Acquire(ctx, acct)
	if err != nil {
		return err
	}
	defer l.Release()

	rec, err := db.BlobUpload(ctx, acct, blobID)
	if errors.Is(err, ErrNotFound) {
		rec = &BlobUpload{}
	} else if err != nil {
		return err
	}
	rec.UploadedAt = now.UTC().Truncate(time.Second)
	if !slices.Contains(rec.Uploaders, uploader) {
		rec.Uploaders = append(rec.Uploaders, uploader)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	batch := &backend.Batch{}
	batch.Set(uploadKey(acct, blobID), raw)
	l.Fence(batch)
	return db.be.WriteBatch(ctx, batch)
}

// BlobUpload returns a blob's upload record, or ErrNotFound. Record
// presence is the existence test for a blob in an account.
func (db *DB) BlobUpload(ctx context.Context, acct, blobID jmap.Id) (*BlobUpload, error) {
	raw, err := db.be.Get(ctx, uploadKey(acct, blobID))
	if errors.Is(err, backend.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var rec BlobUpload
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// BlobReferenced reports whether any committed object references the
// blob through a BlobRef property.
func (db *DB) BlobReferenced(ctx context.Context, acct, blobID jmap.Id) (bool, error) {
	start, end := prefixRange(seg(string(acct)), seg("r"), seg(string(blobID)))
	referenced := false
	err := db.be.Scan(ctx, start, end, false, func(_, _ []byte) bool {
		referenced = true
		return false
	})
	return referenced, err
}

// BlobExists reports whether the blob exists in the account, as this
// Update sees it. /set uses it to reject dangling blobId references
// (invalidProperties, RFC 8620 section 5.3: "There is a reference to
// another record (foreign key), and the given id does not correspond to
// a valid record"). Running inside the Update means the check holds
// through commit: the sweep needs the same lease this Update holds.
func (u *Update) BlobExists(blobID jmap.Id) (bool, error) {
	_, err := u.db.BlobUpload(u.ctx, u.acct, blobID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// SweepBlobs garbage-collects the account's unreferenced blobs whose
// last upload is older than the grace window (never less than
// MinUnreferencedAge) and returns the blobIds it deleted. It runs under
// the account lease, so it can never race a method call that is
// referencing or de-referencing blobs - which is also how the section 6
// rule "a blob MUST NOT be deleted during the method call that removed
// the last reference" holds.
//
// Content is deleted from the store before the upload record: a crash
// in between leaves a record whose content is gone (the next sweep
// finishes the job) rather than invisible, unsweepable content.
func (db *DB) SweepBlobs(ctx context.Context, acct jmap.Id, store blob.Store, now time.Time, grace time.Duration) ([]jmap.Id, error) {
	if grace < MinUnreferencedAge {
		grace = MinUnreferencedAge
	}
	l, err := db.leases.Acquire(ctx, acct)
	if err != nil {
		return nil, err
	}
	defer l.Release()

	// Collect first: the scan callback must not do store I/O.
	type candidate struct {
		blobID jmap.Id
		rec    BlobUpload
	}
	var candidates []candidate
	var scanErr error
	start, end := prefixRange(seg(string(acct)), seg("u"))
	err = db.be.Scan(ctx, start, end, false, func(k, v []byte) bool {
		var rec BlobUpload
		if scanErr = json.Unmarshal(v, &rec); scanErr != nil {
			return false
		}
		candidates = append(candidates, candidate{blobID: idFromObjKey(k), rec: rec})
		return true
	})
	if err == nil {
		err = scanErr
	}
	if err != nil {
		return nil, err
	}

	var deleted []jmap.Id
	batch := &backend.Batch{}
	for _, c := range candidates {
		if now.Sub(c.rec.UploadedAt) < grace {
			continue
		}
		referenced, err := db.BlobReferenced(ctx, acct, c.blobID)
		if err != nil {
			return deleted, err
		}
		if referenced {
			continue
		}
		if err := store.Delete(ctx, acct, c.blobID); err != nil {
			return deleted, err
		}
		batch.Delete(uploadKey(acct, c.blobID))
		deleted = append(deleted, c.blobID)
	}
	if len(deleted) == 0 {
		return nil, nil
	}
	l.Fence(batch)
	if err := db.be.WriteBatch(ctx, batch); err != nil {
		return nil, err
	}
	return deleted, nil
}

// blobRefsOf collects the blobIds referenced by an object's BlobRef
// properties. Values are assumed kind-checked already.
func blobRefsOf(t *descriptor.Type, obj Object) map[jmap.Id]bool {
	var refs map[jmap.Id]bool
	for name, p := range t.Properties {
		if !p.BlobRef {
			continue
		}
		raw, has := obj[name]
		if !has {
			continue
		}
		var id jmap.Id
		if unmarshal(raw, &id) != nil {
			continue
		}
		if refs == nil {
			refs = make(map[jmap.Id]bool)
		}
		refs[id] = true
	}
	return refs
}

// refOps maintains the blob reference index inside the object's commit
// batch, exactly like indexOps does for property indexes.
func refOps(batch *backend.Batch, acct jmap.Id, t *descriptor.Type, id jmap.Id, old, new Object) {
	oldRefs := blobRefsOf(t, old)
	newRefs := blobRefsOf(t, new)
	for blobID := range oldRefs {
		if !newRefs[blobID] {
			batch.Delete(refKey(acct, blobID, t.Name, id))
		}
	}
	for blobID := range newRefs {
		if !oldRefs[blobID] {
			batch.Set(refKey(acct, blobID, t.Name, id), nil)
		}
	}
}
