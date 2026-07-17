// Package fsstore is a blob.Store on a plain filesystem: each blob's content
// (RFC 8620 section 6) is one file, named by its content address, written with
// the tmp-then-rename discipline every maildir-style mail store uses.
//
// It exists because a key-value backend is the wrong shape for message bodies.
// A transactional store has to journal what it writes - SQLite through its WAL,
// any SQL engine through its own - so a megabyte body is copied several times
// on the way in, and it is copied through the engine's page cache rather than
// straight to disk. Measured on one 1.4 MB message: about 9 ms into the best
// case SQLite can offer, against about 4 ms for the same bytes written to a
// file and fsynced. Nothing above blob.Store can tell the difference, so the
// choice is purely an operational one - which is exactly why it belongs behind
// this interface and not in the runtime.
//
// The tradeoff it takes, and it is a real one: blobs stop being covered by the
// backend's transaction. With kvstore or chunkstore, content and the objects
// referencing it commit together, so they cannot disagree. Here they cannot,
// and the ordering is chosen so the surviving inconsistency is the harmless
// one: content is made durable BEFORE the object referencing it is committed
// (delivery calls Commit, then writes the Email), so a crash in between leaves
// an unreferenced blob - garbage, reclaimed by objectdb.SweepBlobs, which is
// the same state an abandoned upload leaves. The reverse order would leave an
// Email pointing at content that does not exist, which no sweep can repair.
//
// Layout under root:
//
//	tmp/<random>                in-progress writes, renamed away on Commit
//	<acct>/<xx>/<blobId>        committed content
//
// One directory per account keeps an account a self-contained unit, as the
// blob.Store contract requires: an account is its key range plus its blob
// directory. The <xx> shard is two characters of the blobId, which spreads a
// large account over 4096 directories rather than piling every message into
// one; the leading "G" that every blobId carries is skipped, since a constant
// makes a poor prefix.
//
// Crash recovery needs no bookkeeping at all. A blob is published by rename(2),
// which is atomic: a file under its content address is complete, by
// construction, and a crash mid-upload can only leave a file in tmp/. Those are
// reclaimed by Sweep on the same "not touched within the window is abandoned"
// rule chunkstore applies to its run markers - except that here the filesystem
// keeps the freshness stamp for free, because writing to a file updates its
// mtime. There is no marker to refresh, and no liveness record to maintain.
//
// This store requires a case-sensitive filesystem. A blobId is base64url and a
// jmap.Id is case-sensitive, so on a case-folding filesystem two distinct ids
// could name one file. That rules out a stock macOS or Windows volume, and no
// POSIX server filesystem.
package fsstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// tmpDir holds in-progress writes. It is a sibling of the account directories
// and shares their filesystem, because rename(2) is only atomic within one.
const tmpDir = "tmp"

// Store implements blob.Store on a directory.
//
// There is deliberately no injectable clock, unlike chunkstore's. The freshness
// stamp here is the temporary file's mtime, which the kernel sets from the real
// clock, so a Sweep judging staleness against a fake one could not agree with
// it. A test ages an upload by moving its mtime (os.Chtimes), which exercises
// the mechanism as it actually runs.
type Store struct {
	root string
}

// New prepares root as a blob store, creating it if needed, and runs Sweep to
// reclaim any temporary file left behind by an upload that crashed long enough
// ago to exceed tuning.UploadReclaimWindow. A file touched recently - another
// process may still be writing it - is spared, and reclaimed by a later sweep
// once its window elapses.
func New(root string) (*Store, error) {
	s := &Store{root: root}
	if err := os.MkdirAll(filepath.Join(root, tmpDir), 0o700); err != nil {
		return nil, err
	}
	if _, err := s.Sweep(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// safeID rejects an id that could escape the store's directory. A section 1.2
// Id is drawn from [A-Za-z0-9_-] only, so it holds no separator and no path
// element that filepath.Join would resolve (".", ".."), whole or in any
// substring the path is built from (the shard is id[1:3]). Requiring validity
// here is what lets blobPath slice the id without re-checking the piece.
func safeID(id jmap.Id) error {
	if !id.Valid() {
		return fmt.Errorf("fsstore: unusable id %q", string(id))
	}
	return nil
}

// blobPath is where an account's blob lives. The shard is taken after the
// leading "G" that blob.IdFromDigest gives every id, so it varies.
func (s *Store) blobPath(acct, blobID jmap.Id) (string, error) {
	if err := safeID(acct); err != nil {
		return "", err
	}
	if err := safeID(blobID); err != nil {
		return "", err
	}
	id := string(blobID)
	shard := id
	if len(id) >= 3 {
		shard = id[1:3]
	}
	return filepath.Join(s.root, string(acct), shard, id), nil
}

// Create implements blob.Store. Bytes go to a temporary file as they arrive and
// the content address is computed from them in one pass, so no part of the blob
// is held in memory.
func (s *Store) Create(ctx context.Context, acct jmap.Id) (blob.BlobWriter, error) {
	if err := safeID(acct); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Join(s.root, tmpDir), "up-")
	if err != nil {
		return nil, err
	}
	return &writer{s: s, ctx: ctx, acct: acct, f: f, hash: sha256.New(), lastFlush: time.Now()}, nil
}

// writerBufSize is how many bytes a writer holds before pushing them to the
// file and the hash. Delivery hands the writer line-sized fragments (the
// parser reads line by line and a tee mirrors each read here), so without
// batching a megabyte message costs tens of thousands of write(2) calls and
// as many tiny hash updates - measured at roughly half the CPU of a delivery.
const writerBufSize = 64 << 10

// writer streams one blob to a temporary file, hashing as it goes, and
// publishes it with a rename at Commit. Bytes are batched in buf; every path
// that reads the hash or the file (ID, Commit) flushes first, so the batching
// is invisible outside this type.
type writer struct {
	s    *Store
	ctx  context.Context
	acct jmap.Id
	f    *os.File
	hash hash.Hash
	buf  []byte
	// lastFlush throttles the freshness guarantee below; werr makes the
	// first flush failure sticky so no later call can publish past it.
	lastFlush time.Time
	werr      error
	done      bool
}

var errWriterDone = errors.New("fsstore: blob writer already finalized")

// Write batches bytes toward the temporary file and the running content hash,
// so Commit needs no second pass over the data. A flush also refreshes the
// file's mtime, which is what proves to Sweep that this upload is still live.
// Batching must not starve that proof: a slow trickle that stays under the
// buffer size still flushes every tuning.UploadRefreshInterval, the same
// liveness cadence chunkstore's marker re-stamp gives, and the interval's
// contract keeps it well inside the reclaim window.
func (w *writer) Write(p []byte) (int, error) {
	if w.done {
		return 0, errWriterDone
	}
	w.buf = append(w.buf, p...)
	if len(w.buf) >= writerBufSize || time.Since(w.lastFlush) >= tuning.UploadRefreshInterval {
		if err := w.flush(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// flush moves the batched bytes to the file and the hash. The hash sees
// exactly the bytes the file took, so the content address can never cover
// bytes that never reached the file; a failure is sticky, failing every
// later flush (and so Commit) rather than letting a gap go unnoticed.
func (w *writer) flush() error {
	if w.werr != nil {
		return w.werr
	}
	if len(w.buf) == 0 {
		return nil
	}
	n, err := w.f.Write(w.buf)
	w.hash.Write(w.buf[:n])
	w.buf = w.buf[:0]
	w.lastFlush = time.Now()
	if err != nil {
		w.werr = err
	}
	return err
}

// ID returns the content address of the bytes written so far, without
// publishing them, so an upload can be recorded before Commit makes it
// durable. A flush failure here leaves the id computed over the bytes that
// did reach the file; the failure is sticky, so Commit can never publish
// content that disagrees with it - the record ends up covering reclaimable
// partial content, the exact state ID exists to make survivable.
func (w *writer) ID() jmap.Id {
	w.flush()
	var sum [sha256.Size]byte
	w.hash.Sum(sum[:0])
	return blob.IdFromDigest(sum)
}

// Commit makes the content durable and publishes it under its content address.
// The file is fsynced before the rename, so the name can never appear over
// bytes that did not reach the disk, and the containing directory is fsynced
// after, so the name itself survives a crash. Identical content already present
// is deduplicated for free: the rename simply replaces one complete copy with
// an identical one (RFC 8620 section 6.1).
func (w *writer) Commit() (jmap.Id, error) {
	if w.done {
		return "", errWriterDone
	}
	w.done = true
	tmp := w.f.Name()
	// Any exit from here that is not a successful rename must not leave the
	// temporary file behind: Sweep would eventually take it, but a failed
	// Commit is not a crash and should not need reclaiming.
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()

	if err := w.flush(); err != nil {
		w.f.Close()
		return "", err
	}
	if err := w.f.Sync(); err != nil {
		w.f.Close()
		return "", err
	}
	if err := w.f.Close(); err != nil {
		return "", err
	}

	var sum [sha256.Size]byte
	w.hash.Sum(sum[:0])
	id := blob.IdFromDigest(sum)
	dst, err := w.s.blobPath(w.acct, id)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	tmp = "" // published: the deferred cleanup must not remove it
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return id, nil
}

// Abort discards a blob that was never committed. A finalized writer refuses
// further use, so a deferred Abort after a successful Commit is a harmless
// no-op error.
func (w *writer) Abort() error {
	if w.done {
		return errWriterDone
	}
	w.done = true
	name := w.f.Name()
	w.f.Close()
	return os.Remove(name)
}

// Put implements blob.Store: content whose id the caller already knows. It goes
// through the same tmp-then-rename path, so a Put is as crash-safe as a Create
// and an interrupted one leaves a reclaimable temporary file rather than a
// truncated blob under a valid content address.
func (s *Store) Put(ctx context.Context, acct, blobID jmap.Id, data []byte) error {
	dst, err := s.blobPath(acct, blobID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Join(s.root, tmpDir), "put-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	tmp = ""
	return syncDir(dir)
}

// Open implements blob.Store. The file is returned unread, so the caller
// streams it and no blob is materialized in memory.
func (s *Store) Open(ctx context.Context, acct, blobID jmap.Id) (io.ReadCloser, int64, error) {
	path, err := s.blobPath(acct, blobID)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, blob.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// Delete implements blob.Store. Deleting a missing blob succeeds, because
// garbage collection must be idempotent.
func (s *Store) Delete(ctx context.Context, acct, blobID jmap.Id) error {
	path, err := s.blobPath(acct, blobID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Sweep removes the debris of uploads that died mid-write: temporary files not
// touched within tuning.UploadReclaimWindow. It returns how many it reclaimed.
//
// The rule is chunkstore's - a run not touched within the window is abandoned -
// but the stamp needs no maintaining here, because every Write updates the
// file's mtime. A live upload, however slow, keeps proving itself simply by
// making progress; one whose process is gone stops. Nothing else in the store
// can be orphaned: a committed blob arrives by rename, atomically, so there is
// no such thing as a half-published one to find.
//
// Erring long is safe (dead bytes linger a little); erring short is not (a live
// upload's file would be unlinked out from under it), which is why the window
// must exceed the embedder's upload idle timeout.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	dir := filepath.Join(s.root, tmpDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-tuning.UploadReclaimWindow)
	n := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue // another sweep, or the writer itself, got there first
		}
		if err != nil {
			return n, err
		}
		if fi.ModTime().After(cutoff) {
			continue // touched recently: an upload may still be writing it
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return n, err
		}
		n++
	}
	return n, nil
}

// syncDir fsyncs a directory so a rename into it survives a crash. Without it
// the file's bytes are durable but the name pointing at them need not be.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
