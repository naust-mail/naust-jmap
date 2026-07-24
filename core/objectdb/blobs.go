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
//   - pending hints ({acct} p {blobId}): written by every path that can
//     leave a blob unreferenced (upload, reference removal), cleared on
//     reference add, so SweepBlobs works a candidate set proportional
//     to churn instead of scanning every upload record.
//
// There are no reference counts: garbage collection is a sweep that
// deletes unreferenced blobs past a grace window (section 6 gives the
// rules; see SweepBlobs). Upload-before-reference is a normal transient
// state, and a missed sweep just runs again - self-healing beats
// precise. The pending hints keep that shape: a hint is a candidate,
// never a verdict, so a stale one is dropped on inspection and deletion
// safety never rests on it.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/internal/jsonscan"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// maxSweepPerCall bounds how many pending hints one SweepBlobs call
// inspects, so a large backlog (a mass destroy) never builds one
// unbounded lease hold; the untouched hints wait for the next call.
const maxSweepPerCall = 1024

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

// FinalizeBlobUpload records a streamed upload and publishes its content
// as one lease-held step, so it cannot interleave with SweepBlobs (which
// holds the same account lease). Holding the lease across both closes two
// races the naive Commit-then-record order leaves open:
//
//   - A sweep that deletes an aged, unreferenced copy of the same content
//     between the writer's dedup check and the record write, leaving a
//     fresh record over content that was just deleted.
//   - A crash after the content is published but before the record exists,
//     which strands published bytes that no upload record and no reference
//     cover, so neither SweepBlobs nor the store's own sweep can ever
//     reclaim them.
//
// The record is written before bw.Commit publishes the content
// (record-first). A crash in between then leaves a record for content that
// is not yet addressable: SweepBlobs drops the unreferenced record after
// its grace window and the store reclaims the still-parked pieces, rather
// than the unreclaimable published-but-unrecorded bytes the reverse order
// would leave. The recorded id is bw.ID, which content addressing keeps
// equal to the id Commit returns.
func (db *DB) FinalizeBlobUpload(ctx context.Context, acct jmap.Id, bw blob.BlobWriter, uploader string, now time.Time) (jmap.Id, error) {
	l, err := db.leases.Acquire(ctx, acct)
	if err != nil {
		return "", err
	}
	defer l.Release()
	return db.finalizeBlobUpload(ctx, acct, bw, uploader, now, l)
}

// finalizeBlobUpload is FinalizeBlobUpload's body under an already-held lease.
func (db *DB) finalizeBlobUpload(ctx context.Context, acct jmap.Id, bw blob.BlobWriter, uploader string, now time.Time, l lease.Lease) (jmap.Id, error) {
	blobID := bw.ID()
	if err := db.recordUpload(ctx, acct, blobID, uploader, now, l); err != nil {
		return "", err
	}
	if _, err := bw.Commit(); err != nil {
		return "", err
	}
	return blobID, nil
}

// FinalizeBlobUploadThenUpdate is FinalizeBlobUpload followed by Update under
// ONE hold of the account lease. Delivery both publishes a message's blob and
// creates its Email; acquiring the lease once for the pair removes a second
// queue wait behind the account's other writers, without changing what
// commits: the semantics are exactly the sequential composition of the two
// calls, crash ordering included.
//
// The finalize half completes before fn runs, so when fn or its commit fails
// the blob is already recorded and published - the returned blobId is then
// non-empty alongside the error, and the caller must treat that state as
// "blob finalized, update failed", just as if the two calls had been made
// separately. A blob left that way is unreferenced and SweepBlobs reclaims it.
// An empty blobId with an error means the finalize itself failed and nothing
// was published.
func (db *DB) FinalizeBlobUploadThenUpdate(ctx context.Context, acct jmap.Id, bw blob.BlobWriter, uploader string, now time.Time, fn func(u *Update) error) (jmap.Id, map[string]string, error) {
	l, err := db.leases.Acquire(ctx, acct)
	if err != nil {
		return "", nil, err
	}
	defer l.Release()

	blobID, err := db.finalizeBlobUpload(ctx, acct, bw, uploader, now, l)
	if err != nil {
		return "", nil, err
	}
	states, err := db.update(ctx, acct, l, fn)
	return blobID, states, err
}

// recordUpload writes a blob's upload record under an already-held lease:
// it reads the current record (if any), sets the upload time and adds the
// uploader, and writes it fenced by the lease. A reupload of existing
// content adds the uploader and resets the upload time (RFC 8620 section 6).
func (db *DB) recordUpload(ctx context.Context, acct, blobID jmap.Id, uploader string, now time.Time, l lease.Lease) error {
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
	// A fresh upload is unreferenced until something commits a BlobRef
	// to it, so it enters the sweep's candidate set now; the commit that
	// references it clears the hint again (refOps).
	batch.Set(pendingKey(acct, blobID), nil)
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
// tuning.BlobMinUnreferencedAge) and returns the blobIds it deleted. It runs under
// the account lease, so it can never race a method call that is
// referencing or de-referencing blobs - which is also how the section 6
// rule "a blob MUST NOT be deleted during the method call that removed
// the last reference" holds.
//
// Candidates come from the pending hints, not a scan of every upload
// record: every path that can leave a blob unreferenced writes a hint
// in the same batch as the fact (recordUpload, refOps), so a sweep's
// work - and the time it holds the account lease against the account's
// writers - is proportional to churn since the last sweep, never to how
// many blobs the account holds. The hint only nominates: deletion is
// still decided by the reference index and the grace window, so a
// stale hint is simply dropped and can never delete a live blob.
//
// When the store implements blob.BatchDeleter over this DB's own
// backend, a blob's content, upload record, and hint are deleted in ONE
// fenced batch: all-or-nothing, and a lease lost mid-sweep fails the
// whole deletion cleanly. Otherwise content is deleted from the store
// before the upload record - a crash in between leaves a record whose
// content is gone (the next sweep finishes the job) rather than
// invisible, unsweepable content - and a lease lost between the
// reference check and the content deletion is not caught for the
// content itself, only for the records.
//
// One call inspects at most maxSweepPerCall candidates, bounding how
// long the lease is held against the account's writers when a mass
// destroy leaves a large backlog of hints. more reports that the call
// hit that bound while still making progress; a caller draining a
// backlog calls again until more is false.
func (db *DB) SweepBlobs(ctx context.Context, acct jmap.Id, store blob.Store, now time.Time, grace time.Duration) (deleted []jmap.Id, more bool, err error) {
	if grace < tuning.BlobMinUnreferencedAge {
		grace = tuning.BlobMinUnreferencedAge
	}

	// Lease-free emptiness probe: most sweeps of most accounts find no
	// hints at all, and taking the account lease for that answer would
	// cost a write round trip per account per pass and make the
	// account's writers queue behind it. Probing without the lease is
	// safe: hints are only written inside commits, so an empty range
	// means there was nothing to collect at probe time, and a hint that
	// lands concurrently simply waits for the next pass - the same
	// outcome as if this sweep had run a moment earlier. The scan below,
	// under the lease, is the authoritative one.
	start, end := prefixRange(seg(string(acct)), seg("p"))
	empty := true
	if err := db.be.Scan(ctx, start, end, false, func(_, _ []byte) bool {
		empty = false
		return false
	}); err != nil {
		return nil, false, err
	}
	if empty {
		return nil, false, nil
	}

	// Fenced deletion when the store persists in this DB's own backend:
	// content, upload record, and hint then leave in one fenced batch, so
	// a lease lost mid-sweep aborts the whole deletion instead of
	// destroying content a new lease holder may have re-referenced (the
	// content address is deterministic, so an identical re-upload gets
	// the same blobId back).
	bd, fenced := store.(blob.BatchDeleter)
	if fenced && bd.DeleteBackend() != db.be {
		fenced = false
	}

	l, err := db.leases.Acquire(ctx, acct)
	if err != nil {
		return nil, false, err
	}
	defer l.Release()

	// Collect first: the scan callback must not do store I/O.
	var candidates []jmap.Id
	err = db.be.Scan(ctx, start, end, false, func(k, _ []byte) bool {
		candidates = append(candidates, idFromObjKey(k))
		return len(candidates) < maxSweepPerCall
	})
	if err != nil {
		return nil, false, err
	}

	batch := &backend.Batch{}
	for _, blobID := range candidates {
		rec, err := db.BlobUpload(ctx, acct, blobID)
		if errors.Is(err, ErrNotFound) {
			// Record already gone (a crashed sweep got that far): the
			// hint is all that is left to clean.
			batch.Delete(pendingKey(acct, blobID))
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if now.Sub(rec.UploadedAt) < grace {
			continue // keep the hint; a later sweep decides
		}
		referenced, err := db.BlobReferenced(ctx, acct, blobID)
		if err != nil {
			return nil, false, err
		}
		if referenced {
			batch.Delete(pendingKey(acct, blobID)) // stale hint
			continue
		}
		if fenced {
			if err := bd.AppendDelete(ctx, batch, acct, blobID); err != nil {
				return nil, false, err
			}
		} else if err := store.Delete(ctx, acct, blobID); err != nil {
			return nil, false, err
		}
		batch.Delete(uploadKey(acct, blobID))
		batch.Delete(pendingKey(acct, blobID))
		deleted = append(deleted, blobID)
	}
	if len(batch.Ops) == 0 {
		// Every candidate stayed inside its grace window: nothing to
		// write, and calling again now would inspect the same hints, so
		// there is no progress for more to promise.
		return nil, false, nil
	}
	l.Fence(batch)
	if err := db.be.WriteBatch(ctx, batch); err != nil {
		return nil, false, err
	}
	// The bound was hit and this call removed hints, so the next call
	// sees a fresh window of candidates: a backlog drains one bounded,
	// separately-leased call at a time.
	more = len(candidates) == maxSweepPerCall
	return deleted, more, nil
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
		s, ok := jsonscan.String(raw)
		if !ok {
			continue // includes a stored null: no blob referenced
		}
		if refs == nil {
			refs = make(map[jmap.Id]bool)
		}
		refs[jmap.Id(s)] = true
	}
	return refs
}

// refOps maintains the blob reference index inside the object's commit
// batch, exactly like indexOps does for property indexes. It also keeps
// the pending hints: a removed reference may have been the blob's last,
// so the hint is set unconditionally (the sweep verifies), and an added
// reference clears it. When one commit moves a reference between two
// objects the batch order decides which hint op lands last - both
// outcomes are safe, because a leftover hint on a referenced blob is
// dropped by the sweep, and a commit that leaves a blob genuinely
// unreferenced has no add for it, so its hint always survives.
func refOps(batch *backend.Batch, acct jmap.Id, t *descriptor.Type, id jmap.Id, old, new Object) {
	oldRefs := blobRefsOf(t, old)
	newRefs := blobRefsOf(t, new)
	for blobID := range oldRefs {
		if !newRefs[blobID] {
			batch.Delete(refKey(acct, blobID, t.Name, id))
			batch.Set(pendingKey(acct, blobID), nil)
		}
	}
	for blobID := range newRefs {
		if !oldRefs[blobID] {
			batch.Set(refKey(acct, blobID, t.Name, id), nil)
			batch.Delete(pendingKey(acct, blobID))
		}
	}
}
