// Package blob defines the BlobStore socket: persistence for the
// immutable binary data that blobIds represent (RFC 8620 section 6).
//
// The runtime owns blobId semantics: ids are content addresses computed
// by IdFor, and the namespace is per account so that one account is
// always one self-contained unit (copy/migrate/drop an account = its
// key range plus its blobs). A store may deduplicate identical content
// across accounts below this interface (e.g. an object-store CAS
// layer); the runtime never sees or depends on that.
//
// Reference tracking, upload metadata, and garbage collection are not
// the store's concern - they live in objectdb, in-commit with the
// objects doing the referencing. The store only holds bytes.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"

	"github.com/naust-mail/naust-jmap/core/jmap"
)

// ErrNotFound reports a blob that does not exist in the account.
var ErrNotFound = errors.New("blob: not found")

// Store is the socket. All methods must be safe for concurrent use.
type Store interface {
	// Create begins a streaming write into acct. The caller feeds the blob's
	// bytes to the returned writer and calls Commit to store them under their
	// content address; the whole blob need never be held above this interface
	// (a store over object storage can flush chunks as they arrive). This is
	// the path for content of unknown, caller-supplied bytes (upload,
	// delivery); Put is the path for copying content whose id is already known.
	Create(ctx context.Context, acct jmap.Id) (BlobWriter, error)
	// Put stores content under (acct, blobID). Content is immutable and
	// blobID is its content address, so overwriting is idempotent: a
	// store may treat a Put of an existing blob as a no-op.
	Put(ctx context.Context, acct, blobID jmap.Id, data []byte) error
	// Open returns the blob's content and size, or ErrNotFound.
	Open(ctx context.Context, acct, blobID jmap.Id) (io.ReadCloser, int64, error)
	// Delete removes a blob. Deleting a missing blob is not an error
	// (garbage collection must be idempotent).
	Delete(ctx context.Context, acct, blobID jmap.Id) error
}

// BlobWriter streams one blob's content into a Store, computing its content
// address (IdFor) from the bytes written. A BlobWriter is used by a single
// goroutine and is not safe for concurrent use. The content is not stored
// until Commit returns nil; Abort discards an uncommitted write. Exactly one
// of Commit or Abort finalizes the writer, and calling either after the writer
// is finalized is a no-op error - so a deferred Abort after a successful Commit
// is safe.
type BlobWriter interface {
	// Write appends bytes to the blob (io.Writer semantics).
	io.Writer
	// ID returns the content-addressed id of the bytes written so far (the
	// value Commit would return), computed without publishing the content.
	// It lets a caller record a blob's existence before the content is made
	// durable, so a crash between the two leaves a record over reclaimable
	// partial content rather than published content that no record covers
	// and no sweep can find. It is normally called once, after the final
	// Write and before Commit.
	ID() jmap.Id
	// Commit stores the blob and returns its content-addressed id (the value
	// IdFor would return for all bytes written). After Commit the content is
	// durable and immutable.
	Commit() (jmap.Id, error)
	// Abort discards a blob that was never committed.
	Abort() error
}

// IdFor computes the content-addressed blobId for data: "G" followed by
// the unpadded URL-safe base64 of its SHA-256 digest. The result is a
// valid RFC 8620 section 1.2 Id; the leading letter keeps server ids
// out of the section's risky forms (digit-only, leading dash). Content
// addressing gives the section 6.1 dedup behavior ("If identical binary
// content to an existing blob in the account is uploaded, the existing
// blobId MAY be returned") for free.
func IdFor(data []byte) jmap.Id {
	return IdFromDigest(sha256.Sum256(data))
}

// IdFromDigest builds the same content-addressed blobId as IdFor from an
// already-computed SHA-256 digest. A store that hashes its input as a
// stream (never holding the whole blob) finalizes its running digest and
// calls this, so the "G" + unpadded base64url formatting has a single
// definition shared with IdFor and the two can never drift.
func IdFromDigest(sum [sha256.Size]byte) jmap.Id {
	return jmap.Id("G" + base64.RawURLEncoding.EncodeToString(sum[:]))
}
