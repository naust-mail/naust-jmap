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

// IdFor computes the content-addressed blobId for data: "G" followed by
// the unpadded URL-safe base64 of its SHA-256 digest. The result is a
// valid RFC 8620 section 1.2 Id; the leading letter keeps server ids
// out of the section's risky forms (digit-only, leading dash). Content
// addressing gives the section 6.1 dedup behavior ("If identical binary
// content to an existing blob in the account is uploaded, the existing
// blobId MAY be returned") for free.
func IdFor(data []byte) jmap.Id {
	sum := sha256.Sum256(data)
	return jmap.Id("G" + base64.RawURLEncoding.EncodeToString(sum[:]))
}
