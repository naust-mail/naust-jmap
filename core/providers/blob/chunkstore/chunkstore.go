// Package chunkstore is a streaming blob.Store: it holds each blob's
// content (RFC 8620 section 6, the immutable binary data a blobId names)
// as a run of fixed-size pieces in a backend.Backend, so no whole blob is
// ever held in memory on the way in or out. This lets a single-node
// key-value backend (for example SQLite) serve blobs far larger than a
// comfortable in-memory value without any backend-specific code.
//
// Layout in the shared backend (one account is one contiguous key range,
// so an account stays a self-contained unit):
//
//	{acct} B {blobId}          manifest: where the pieces are + total size
//	{acct} C {run} {index}     one piece of content (index is 4 bytes, big-endian)
//	!C {run}                   run marker: an upload in progress or crash-orphaned
//
// The "B" tag is the same one the whole-value KV store uses; a backend
// holds one blob store implementation, never both, so there is no clash.
// The "!C" marker range is top level, like the push range: "!" is outside
// the section 1.2 Id alphabet, so it can never collide with an account.
//
// Why a manifest rather than keying pieces by the blobId directly: a
// blobId is the content address (blob.IdFor) and is only known once the
// last byte has been hashed, but pieces must be written as they stream in.
// So pieces are parked under a random run id, and Commit writes one small
// manifest at the blobId pointing at that run. Identical content still
// deduplicates: the second upload computes the same blobId, finds the
// existing manifest, and discards its own pieces (section 6.1, "If
// identical binary content to an existing blob in the account is uploaded,
// the existing blobId MAY be returned").
//
// A run marker records an in-progress upload. On a clean Commit or Abort
// the marker is removed; a crash leaves it behind, pointing at orphaned
// pieces that no manifest references. Sweep reclaims those. Reference
// tracking and quota-driven deletion of unreferenced blobs are not this
// store's concern (they live in objectdb, in-commit with the referencing
// objects); the store only holds bytes and cleans up its own partial
// writes.
//
// Against the alternatives: kvstore is cheaper while blobs stay small
// (one value, no manifest, no pieces) but holds each arriving blob whole
// in memory, so it does not stay cheap as they grow; fsstore is faster
// still on large blobs but gives up committing blobs in the same
// transaction as the objects referencing them. This store is the one that
// costs the same at any blob size, which is why it is the default for
// mail, where an attachment's size is the client's choice and not the
// server's.
package chunkstore

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/naust-mail/naust-jmap/core/internal/keyenc"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// runIDLen is the length of a run id: 16 random bytes, enough that two
// uploads never collide on a run without any coordination.
const runIDLen = 16

// runID identifies the parked piece-run for one upload.
type runID [runIDLen]byte

// Store implements blob.Store over a backend.Backend.
type Store struct {
	be backend.Backend
	// now supplies the wall-clock reading used to stamp run markers and to
	// judge staleness in Sweep. It is a field rather than a direct time.Now
	// call only so a test can drive the reclaim window without real waiting.
	now func() time.Time
}

// Option configures a Store at construction.
type Option func(*Store)

// WithNow overrides the clock used to stamp run markers and to judge
// staleness in Sweep. It exists for deterministic testing (and embedders that
// inject their own clock); production leaves the default, time.Now.
func WithNow(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New wraps a backend. Construction runs Sweep, which reclaims the pieces of
// any run whose marker has gone stale (see Sweep) - the debris of an upload
// that crashed long enough ago to exceed UploadReclaimWindow. A run stamped
// recently (this or another process may still be writing it) is spared and
// reclaimed by a later sweep once its window elapses.
func New(be backend.Backend, opts ...Option) (*Store, error) {
	s := &Store{be: be, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	if _, err := s.Sweep(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// --- keys ---

func manifestKey(acct, blobID jmap.Id) []byte {
	return keyenc.Key([]byte(acct), []byte("B"), []byte(blobID))
}

// pieceKey addresses one piece. The index is fixed 4-byte big-endian so
// that a prefix scan over a run visits its pieces in order.
func pieceKey(acct jmap.Id, run runID, index uint32) []byte {
	var ix [4]byte
	binary.BigEndian.PutUint32(ix[:], index)
	return keyenc.Key([]byte(acct), []byte("C"), run[:], ix[:])
}

// pieceRange covers every piece of one run, for scanning or bulk delete.
func pieceRange(acct jmap.Id, run runID) (start, end []byte) {
	return keyenc.PrefixRange([]byte(acct), []byte("C"), run[:])
}

func markerKey(run runID) []byte {
	return keyenc.Key([]byte("!C"), run[:])
}

// markerRange covers every run marker across all accounts, so Sweep can
// enumerate in-progress and orphaned runs without scanning blob content.
func markerRange() (start, end []byte) {
	return keyenc.PrefixRange([]byte("!C"))
}

// markerValue carries the run, a last-progress wall-clock stamp (Unix
// nanoseconds), and the account inside the marker. The stamp lets Sweep judge
// from the marker alone whether a run is still being written (stamped
// recently) or was abandoned (stale), with no per-process state; the run
// locates the pieces and the account scopes them.
func markerValue(acct jmap.Id, run runID, stamp time.Time) []byte {
	v := make([]byte, runIDLen+8, runIDLen+8+len(acct))
	copy(v, run[:])
	binary.BigEndian.PutUint64(v[runIDLen:], uint64(stamp.UnixNano()))
	return append(v, []byte(acct)...)
}

func decodeMarker(value []byte) (acct jmap.Id, run runID, stamp time.Time, ok bool) {
	if len(value) < runIDLen+8 {
		return "", run, time.Time{}, false
	}
	copy(run[:], value[:runIDLen])
	nanos := int64(binary.BigEndian.Uint64(value[runIDLen:]))
	return jmap.Id(value[runIDLen+8:]), run, time.Unix(0, nanos), true
}

// --- manifest ---

// manifest is the small record at {acct} B {blobId}. It names the run
// holding the blob's pieces and records the blob's exact octet length, so
// a read reports the size (RFC 8620 section 6) and streams the pieces back
// without having to scan them first.
type manifest struct {
	run   runID
	count uint32 // number of pieces
	size  int64  // total content length in octets
}

const (
	manifestVersion = 1
	// A leading version byte, the run, a 4-byte piece count, and an
	// 8-byte size.
	manifestLen = 1 + runIDLen + 4 + 8
)

func encodeManifest(m manifest) []byte {
	b := make([]byte, manifestLen)
	b[0] = manifestVersion
	copy(b[1:1+runIDLen], m.run[:])
	binary.BigEndian.PutUint32(b[1+runIDLen:], m.count)
	binary.BigEndian.PutUint64(b[1+runIDLen+4:], uint64(m.size))
	return b
}

// decodeManifest rejects any length or version it does not recognise so a
// truncated or foreign value fails cleanly instead of being misread.
func decodeManifest(b []byte) (manifest, error) {
	if len(b) != manifestLen {
		return manifest{}, fmt.Errorf("chunkstore: manifest is %d bytes, want %d", len(b), manifestLen)
	}
	if b[0] != manifestVersion {
		return manifest{}, fmt.Errorf("chunkstore: unknown manifest version %d", b[0])
	}
	var m manifest
	copy(m.run[:], b[1:1+runIDLen])
	m.count = binary.BigEndian.Uint32(b[1+runIDLen:])
	m.size = int64(binary.BigEndian.Uint64(b[1+runIDLen+4:]))
	return m, nil
}
