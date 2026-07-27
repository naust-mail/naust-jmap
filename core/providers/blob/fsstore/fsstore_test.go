package fsstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/fsstore"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

const acct = jmap.Id("Ademo")

func newStore(t *testing.T) (*fsstore.Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := fsstore.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

// age moves every temporary file's mtime back by d, standing in for time
// passing. The mtime IS the liveness signal the store reads, so aging it is the
// honest way to simulate an upload that stopped making progress - there is no
// clock to fake.
func age(t *testing.T, root string, d time.Duration) {
	t.Helper()
	dir := filepath.Join(root, "tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-d)
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(dir, e.Name()), when, when); err != nil {
			t.Fatal(err)
		}
	}
}

// write streams data through a BlobWriter and commits it.
func write(t *testing.T, s *fsstore.Store, data []byte) jmap.Id {
	t.Helper()
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	id, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func read(t *testing.T, s *fsstore.Store, id jmap.Id) []byte {
	t.Helper()
	rc, size, err := s.Open(context.Background(), acct, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(got)) {
		t.Fatalf("Open reported size %d, content is %d bytes", size, len(got))
	}
	return got
}

func TestRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	data := []byte("From: a@example.com\r\n\r\nhello")

	id := write(t, s, data)
	if want := blob.IdFor(data); id != want {
		t.Fatalf("Commit returned id %q, want the content address %q", id, want)
	}
	if got := read(t, s, id); !bytes.Equal(got, data) {
		t.Fatalf("read back %q, want %q", got, data)
	}
}

// The blob must survive being written in many small pieces: the content address
// covers the bytes, not the calls.
func TestStreamingMatchesWholeWrite(t *testing.T) {
	s, _ := newStore(t)
	data := make([]byte, 300*1024)
	rand.Read(data)

	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(data); off += 977 { // deliberately not a round size
		end := off + 977
		if end > len(data) {
			end = len(data)
		}
		if _, err := w.Write(data[off:end]); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.ID(); got != blob.IdFor(data) {
		t.Fatalf("ID() before Commit is %q, want %q", got, blob.IdFor(data))
	}
	id, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if id != blob.IdFor(data) {
		t.Fatalf("Commit id %q, want %q", id, blob.IdFor(data))
	}
	if got := read(t, s, id); !bytes.Equal(got, data) {
		t.Fatal("streamed content differs from the bytes written")
	}
}

// Identical content stores once and yields one id (RFC 8620 section 6.1).
func TestDedup(t *testing.T) {
	s, root := newStore(t)
	data := []byte("the same message twice")

	first := write(t, s, data)
	second := write(t, s, data)
	if first != second {
		t.Fatalf("identical content got two ids: %q and %q", first, second)
	}
	if got := read(t, s, first); !bytes.Equal(got, data) {
		t.Fatal("content corrupted by the second write")
	}
	if n := countFiles(t, filepath.Join(root, string(acct))); n != 1 {
		t.Fatalf("identical content stored %d times, want 1", n)
	}
}

func TestOpenMissing(t *testing.T) {
	s, _ := newStore(t)
	_, _, err := s.Open(context.Background(), acct, blob.IdFor([]byte("never stored")))
	if !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Open of a missing blob: %v, want blob.ErrNotFound", err)
	}
}

// Deletion is idempotent, because garbage collection must be able to retry.
func TestDeleteIsIdempotent(t *testing.T) {
	s, _ := newStore(t)
	id := write(t, s, []byte("transient"))

	for i := 0; i < 2; i++ {
		if err := s.Delete(context.Background(), acct, id); err != nil {
			t.Fatalf("Delete #%d: %v", i+1, err)
		}
	}
	if _, _, err := s.Open(context.Background(), acct, id); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("after Delete, Open: %v, want blob.ErrNotFound", err)
	}
}

// An aborted upload publishes nothing and leaves nothing behind.
func TestAbortLeavesNothing(t *testing.T) {
	s, root := newStore(t)
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("half an upload")
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(context.Background(), acct, blob.IdFor(data)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("aborted content is readable: %v", err)
	}
	if n := countFiles(t, filepath.Join(root, "tmp")); n != 0 {
		t.Fatalf("Abort left %d temporary file(s)", n)
	}
	// A deferred Abort after Commit is documented as a harmless no-op error, so
	// a finalized writer must refuse rather than delete anything.
	if err := w.Abort(); err == nil {
		t.Fatal("second Abort succeeded, want an already-finalized error")
	}
}

func TestPut(t *testing.T) {
	s, _ := newStore(t)
	data := []byte("content whose id the caller already knows")
	id := blob.IdFor(data)

	if err := s.Put(context.Background(), acct, id, data); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, id); !bytes.Equal(got, data) {
		t.Fatalf("Put stored %q, want %q", got, data)
	}
	// Content is immutable, so re-Putting it is a no-op, not an error.
	if err := s.Put(context.Background(), acct, id, data); err != nil {
		t.Fatalf("re-Put of existing content: %v", err)
	}
}

// One account's blobs are invisible to another: the store's namespace is per
// account, which is what lets an account be dropped or migrated as a unit.
func TestAccountsAreSeparate(t *testing.T) {
	s, _ := newStore(t)
	data := []byte("mine")
	id := write(t, s, data)

	_, _, err := s.Open(context.Background(), jmap.Id("Aother"), id)
	if !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("another account can read the blob: %v", err)
	}
}

// Sweep reclaims the temporary file a crashed upload leaves, but only once it
// has gone untouched for the reclaim window - a slow upload still writing must
// not be swept out from under itself.
func TestSweepReclaimsOnlyStaleUploads(t *testing.T) {
	s, root := newStore(t)

	// A writer that never commits, as a killed process leaves behind.
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("interrupted")); err != nil {
		t.Fatal(err)
	}

	// Still fresh: the upload could be live, so it is spared.
	n, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("swept %d fresh upload(s), want 0", n)
	}
	if got := countFiles(t, filepath.Join(root, "tmp")); got != 1 {
		t.Fatalf("fresh upload's file is gone: %d files in tmp", got)
	}

	// Untouched for longer than the window: abandoned.
	age(t, root, tuning.UploadReclaimWindow+time.Minute)
	n, err = s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept %d stale upload(s), want 1", n)
	}
	if got := countFiles(t, filepath.Join(root, "tmp")); got != 0 {
		t.Fatalf("stale upload left %d file(s) in tmp", got)
	}
}

// The mtime is the liveness signal, and flushing refreshes it. Writes batch
// toward the buffer size, so a trickling upload proves it is alive not on
// every write but at the tuning.UploadRefreshInterval cadence: age the file
// past the reclaim window and the writer's flush throttle past the interval,
// as a long trickle would, and one more small write must restore freshness so
// the sweep that follows spares it.
func TestWriteRefreshesLiveness(t *testing.T) {
	s, root := newStore(t)

	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("slow but alive")); err != nil {
		t.Fatal(err)
	}

	// Age it past the window, as a stalled upload would be - then write. With
	// the flush throttle also past its interval, the write alone must restore
	// freshness, so the sweep that follows spares it.
	age(t, root, tuning.UploadReclaimWindow+time.Minute)
	fsstore.AgeThrottle(w, tuning.UploadRefreshInterval+time.Second)
	if _, err := w.Write([]byte(" - still going")); err != nil {
		t.Fatal(err)
	}
	n, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("swept %d live upload(s), want 0", n)
	}
	if _, err := w.Commit(); err != nil {
		t.Fatalf("live upload could not commit: %v", err)
	}
}

// A committed blob is published by rename, so it is complete by construction.
// Sweep only ever looks in tmp/, so age cannot make it a candidate.
func TestSweepSparesCommittedBlobs(t *testing.T) {
	s, root := newStore(t)
	id := write(t, s, []byte("long-lived mail"))

	age(t, root, 100*tuning.UploadReclaimWindow)
	if _, err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(context.Background(), acct, id); err != nil {
		t.Fatalf("Sweep took a committed blob: %v", err)
	}
}

// An id is never trusted as a path element. A caller cannot walk out of the
// store, whatever it passes.
func TestRejectsPathEscapes(t *testing.T) {
	s, _ := newStore(t)
	// The last three are not path elements whole, but blobPath slices the id
	// for the shard (id[1:3]), so "A.." yields a ".." shard: an id must be
	// rejected for any dangerous substring, not only when it is one itself.
	for _, bad := range []jmap.Id{"..", "../../etc", "a/b", "", "A..", "G..", "A/."} {
		if _, _, err := s.Open(context.Background(), acct, bad); err == nil ||
			errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Open with blobId %q: %v, want a rejection", bad, err)
		}
		if _, _, err := s.Open(context.Background(), bad, "Gx"); err == nil ||
			errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Open with account %q: %v, want a rejection", bad, err)
		}
	}
}

// The root-escape backstop in blobPath compares a joined path against a
// prefix derived from root. If New did not canonicalize a root given with a
// trailing separator, that prefix would carry a doubled separator no clean
// joined path ever matches, and every legitimate call would be rejected as an
// escape. New must normalize root so this never happens.
func TestNonCanonicalRootStillWorks(t *testing.T) {
	base := t.TempDir()
	s, err := fsstore.New(base + string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("From: a@example.com\r\n\r\nhello")
	id := write(t, s, data)
	if got := read(t, s, id); !bytes.Equal(got, data) {
		t.Fatalf("read back %q, want %q", got, data)
	}
}

// New's own doc says it "runs Sweep to reclaim any temporary file left
// behind by an upload that crashed long enough ago" - startup debris from a
// previous, killed process. That path was untested: every other test
// starts from a fresh root via newStore before any tmp file exists.
func TestNewSweepsStaleDebrisAtStartup(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(tmp, "up-stale")
	if err := os.WriteFile(stale, []byte("orphaned by a killed process"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-(tuning.UploadReclaimWindow + time.Minute))
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(tmp, "up-fresh")
	if err := os.WriteFile(fresh, []byte("still within the window"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := fsstore.New(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale debris survived New: stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("New swept a file still within the reclaim window: %v", err)
	}
}

// Sweep must tolerate a missing tmp directory rather than erroring: nothing
// else in the store recreates it once removed (a mistaken cleanup, a
// separate maintenance script), and a permanent Sweep failure would be
// worse than treating "no debris directory" as "no debris".
func TestSweepToleratesMissingTmpDir(t *testing.T) {
	s, root := newStore(t)
	if err := os.RemoveAll(filepath.Join(root, "tmp")); err != nil {
		t.Fatal(err)
	}
	n, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep with no tmp dir: %v", err)
	}
	if n != 0 {
		t.Fatalf("Sweep with no tmp dir reclaimed %d, want 0", n)
	}
}

// Sweep must stop promptly on a cancelled context rather than walking a
// large backlog to completion regardless of the caller.
func TestSweepRespectsCancelledContext(t *testing.T) {
	s, root := newStore(t)
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("stale")); err != nil {
		t.Fatal(err)
	}
	age(t, root, tuning.UploadReclaimWindow+time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Sweep(ctx); err == nil {
		t.Fatal("Sweep with a cancelled context: want an error, got nil")
	}
}

// A finalized writer, whether by Commit or by Abort, must refuse every
// further operation: a caller holding a stale reference (a bug, or a retry
// after a caller-side timeout that actually succeeded) must not be able to
// mutate or republish content past finalization.
func TestWriterRefusesUseAfterFinalize(t *testing.T) {
	commitThenRefuse := func(t *testing.T, finalize func(w blob.BlobWriter) error) {
		s, _ := newStore(t)
		w, err := s.Create(context.Background(), acct)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("data")); err != nil {
			t.Fatal(err)
		}
		if err := finalize(w); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("more")); err == nil {
			t.Error("Write after finalize succeeded, want an error")
		}
		if _, err := w.Commit(); err == nil {
			t.Error("Commit after finalize succeeded, want an error")
		}
	}
	t.Run("after commit", func(t *testing.T) {
		commitThenRefuse(t, func(w blob.BlobWriter) error { _, err := w.Commit(); return err })
	})
	t.Run("after abort", func(t *testing.T) {
		commitThenRefuse(t, func(w blob.BlobWriter) error { return w.Abort() })
	})
}

// Put and Delete share blobPath with Open, but Open's path-escape coverage
// (TestRejectsPathEscapes) does not exercise them: Put creates a file and
// Delete removes one, so a bypass here is a write/unlink primitive rather
// than just an unexpected read.
func TestPutAndDeleteRejectPathEscapes(t *testing.T) {
	s, _ := newStore(t)
	for _, bad := range []jmap.Id{"..", "../../etc", "a/b", "", "A..", "G..", "A/."} {
		if err := s.Put(context.Background(), acct, bad, []byte("x")); err == nil {
			t.Errorf("Put with blobId %q: want a rejection", bad)
		}
		if err := s.Put(context.Background(), bad, "Gx", []byte("x")); err == nil {
			t.Errorf("Put with account %q: want a rejection", bad)
		}
		if err := s.Delete(context.Background(), acct, bad); err == nil {
			t.Errorf("Delete with blobId %q: want a rejection", bad)
		}
		if err := s.Delete(context.Background(), bad, "Gx"); err == nil {
			t.Errorf("Delete with account %q: want a rejection", bad)
		}
	}
}

// blobPath falls back to the whole id as the shard when the id is under 3
// characters (the general case slices id[1:3]). A blobId that short never
// comes from IdFromDigest, but Put accepts a caller-supplied id, so the
// fallback is reachable and must still round-trip correctly.
func TestPutWithShortBlobID(t *testing.T) {
	s, _ := newStore(t)
	const id = jmap.Id("Gx")
	data := []byte("short id")
	if err := s.Put(context.Background(), acct, id, data); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, id); !bytes.Equal(got, data) {
		t.Fatalf("read back %q, want %q", got, data)
	}
	if err := s.Delete(context.Background(), acct, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(context.Background(), acct, id); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("after Delete, Open: %v, want blob.ErrNotFound", err)
	}
}

// Create validates the account id up front, like blobPath does for Open,
// Put, and Delete - but Create's check was untested, and a bypass here
// would leave a temporary file allocated under an attacker-chosen path
// component before anything else in the store gets a chance to reject it.
func TestCreateRejectsBadAccount(t *testing.T) {
	s, root := newStore(t)
	for _, bad := range []jmap.Id{"..", "../../etc", "a/b", ""} {
		if _, err := s.Create(context.Background(), bad); err == nil {
			t.Errorf("Create with account %q: want a rejection", bad)
		}
	}
	if n := countFiles(t, filepath.Join(root, "tmp")); n != 0 {
		t.Fatalf("rejected Create calls left %d file(s) in tmp", n)
	}
}

// A zero-byte blob is a legitimate value (an empty attachment, a stub
// upload) and its content address is well-defined (sha256 of no bytes);
// nothing in Write/Commit special-cases "never wrote anything", so it must
// still round-trip through both entry points.
func TestEmptyBlob(t *testing.T) {
	s, _ := newStore(t)

	id := write(t, s, nil)
	if want := blob.IdFor(nil); id != want {
		t.Fatalf("empty blob via Create/Commit got id %q, want %q", id, want)
	}
	if got := read(t, s, id); len(got) != 0 {
		t.Fatalf("empty blob read back %d bytes, want 0", len(got))
	}

	if err := s.Put(context.Background(), acct, id, nil); err != nil {
		t.Fatalf("Put of empty content: %v", err)
	}
	if got := read(t, s, id); len(got) != 0 {
		t.Fatalf("empty blob via Put read back %d bytes, want 0", len(got))
	}
}

// Sweep must not choke on a subdirectory under tmp/: nothing in this store
// creates one, but Sweep is a maintenance pass over a directory it does not
// fully control (an operator poking around, a future writer that shards
// tmp/), and e.IsDir() is the guard that is supposed to make that safe.
func TestSweepSkipsSubdirectories(t *testing.T) {
	s, root := newStore(t)
	sub := filepath.Join(root, "tmp", "unexpected-dir")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-(tuning.UploadReclaimWindow + time.Minute))
	if err := os.Chtimes(sub, old, old); err != nil {
		t.Fatal(err)
	}

	n, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep with a subdirectory present: %v", err)
	}
	if n != 0 {
		t.Fatalf("Sweep reported %d reclaimed, want 0", n)
	}
	if _, err := os.Stat(sub); err != nil {
		t.Fatalf("Sweep removed the subdirectory: %v", err)
	}
}

// chmodCleanup restores path's mode at test end, before t.TempDir()'s own
// cleanup runs (t.Cleanup is LIFO, and this is registered after newStore's
// call to t.TempDir), so a permission fault injected mid-test never leaves
// TempDir unable to remove its own tree.
func chmodCleanup(t *testing.T, path string, restore os.FileMode) {
	t.Helper()
	t.Cleanup(func() { os.Chmod(path, restore) })
}

// skipIfRoot skips a test that induces its failure by clamping permissions.
// Root is exempt from the permission checks, so the operation under test
// succeeds, no error is produced, and the assertion is vacuous. Reporting
// that as a pass would claim coverage the run did not have, so it is skipped
// instead: the error propagation these tests guard is real, it just cannot be
// provoked this way by a user the kernel never refuses.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("running as root: permission bits are not enforced, so the failure this test induces cannot occur")
	}
}

// New propagates a failure to create its tmp directory: root is a plain
// file, so MkdirAll cannot descend into it.
func TestNewFailsWhenRootIsAFile(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fsstore.New(root); err == nil {
		t.Fatal("New with a file as root: want an error")
	}
}

// New propagates a failure from its startup Sweep: an unreadable tmp
// directory (already present from a prior run, permissions clamped down by
// an operator or a broken deploy) must fail New rather than silently
// starting with debris New could not inspect.
func TestNewPropagatesSweepFailure(t *testing.T) {
	skipIfRoot(t)
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o000); err != nil {
		t.Fatal(err)
	}
	chmodCleanup(t, tmp, 0o700)

	if _, err := fsstore.New(root); err == nil {
		t.Fatal("New with an unreadable tmp dir: want an error")
	}
}

// Sweep itself, not just New's startup call, must propagate a permission
// failure reading tmp/ rather than reporting a false "nothing to reclaim".
func TestSweepPropagatesReadDirFailure(t *testing.T) {
	skipIfRoot(t)
	s, root := newStore(t)
	tmp := filepath.Join(root, "tmp")
	if err := os.Chmod(tmp, 0o000); err != nil {
		t.Fatal(err)
	}
	chmodCleanup(t, tmp, 0o700)

	if _, err := s.Sweep(context.Background()); err == nil {
		t.Fatal("Sweep with an unreadable tmp dir: want an error")
	}
}

// Sweep must propagate a failure removing a stale file (a read-only tmp
// directory - an operator lockdown, a misconfigured mount) rather than
// silently reporting the file reclaimed.
func TestSweepPropagatesRemoveFailure(t *testing.T) {
	skipIfRoot(t)
	s, root := newStore(t)
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("stale")); err != nil {
		t.Fatal(err)
	}
	age(t, root, tuning.UploadReclaimWindow+time.Minute)

	tmp := filepath.Join(root, "tmp")
	if err := os.Chmod(tmp, 0o500); err != nil { // read+execute, no write
		t.Fatal(err)
	}
	chmodCleanup(t, tmp, 0o700)

	if _, err := s.Sweep(context.Background()); err == nil {
		t.Fatal("Sweep unable to remove a stale file: want an error")
	}
}

// Create propagates a failure allocating its temporary file: tmp/ is gone
// (removed out from under a running store, which Sweep and Delete never
// do, but an operator or a stray cleanup script might).
func TestCreateFailsWhenTmpDirMissing(t *testing.T) {
	s, root := newStore(t)
	if err := os.RemoveAll(filepath.Join(root, "tmp")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), acct); err == nil {
		t.Fatal("Create with tmp/ missing: want an error")
	}
}

// A write that must flush (the buffer throttle has aged past
// UploadRefreshInterval) propagates the underlying file error rather than
// reporting success over lost bytes; the failure is sticky, so a later call
// that flushes unconditionally (ID, or a Write past the buffer threshold)
// gets the same error without a second attempt to touch the file.
func TestWriteFailsWhenUnderlyingFileClosed(t *testing.T) {
	s, _ := newStore(t)
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsstore.CloseUnderlyingFile(w); err != nil {
		t.Fatal(err)
	}
	fsstore.AgeThrottle(w, tuning.UploadRefreshInterval+time.Second)

	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("Write against a closed file: want an error")
	}
	// The failed flush already reset the throttle, so a second small write
	// would not attempt another flush on its own; ID() flushes
	// unconditionally and is what must surface the sticky failure.
	w.ID()
	if _, err := w.Commit(); err == nil {
		t.Fatal("Commit after a sticky flush failure: want an error")
	}
}

// Commit propagates a flush failure for buffered-but-unflushed bytes: the
// underlying file is closed while data still sits in the write buffer.
func TestCommitFailsWhenFlushFails(t *testing.T) {
	s, _ := newStore(t)
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	// Small write, well under the buffer threshold and the refresh
	// interval: it stays buffered, unflushed, until Commit forces it.
	if _, err := w.Write([]byte("buffered")); err != nil {
		t.Fatal(err)
	}
	if err := fsstore.CloseUnderlyingFile(w); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("Commit whose flush fails: want an error")
	}
}

// Commit propagates an fsync failure distinctly from a flush failure: all
// bytes are already flushed (a write past the buffer threshold empties the
// buffer as it goes), so Commit's own flush is a no-op and Sync is the
// first call to touch the now-closed file.
func TestCommitFailsWhenSyncFails(t *testing.T) {
	s, _ := newStore(t)
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 128<<10) // well past writerBufSize
	if _, err := w.Write(big); err != nil {
		t.Fatal(err)
	}
	if err := fsstore.CloseUnderlyingFile(w); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("Commit whose fsync fails: want an error")
	}
}

// Commit propagates a failure creating the destination shard directory: the
// account's top-level directory already exists as a plain file, so
// MkdirAll cannot descend through it. The temporary file must still be
// cleaned up rather than left in tmp/.
func TestCommitFailsWhenAccountDirIsAFile(t *testing.T) {
	s, root := newStore(t)
	if err := os.WriteFile(filepath.Join(root, string(acct)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("Commit under a file-shadowed account dir: want an error")
	}
	if n := countFiles(t, filepath.Join(root, "tmp")); n != 0 {
		t.Fatalf("failed Commit left %d file(s) in tmp", n)
	}
}

// Commit propagates a rename failure when the destination path is already
// occupied by a directory: a directory cannot be replaced by a rename of a
// regular file onto it.
func TestCommitFailsWhenDestinationIsADirectory(t *testing.T) {
	s, root := newStore(t)
	data := []byte("collides with a pre-existing directory")
	id := blob.IdFor(data)
	dst := filepath.Join(root, string(acct), string(id[1:3]), string(id))
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}

	w, err := s.Create(context.Background(), acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("Commit onto an existing directory: want an error")
	}
}

// Put shares Commit's directory-creation and rename steps; both failure
// modes above apply to it independently and were unexercised.
func TestPutFailsWhenAccountDirIsAFile(t *testing.T) {
	s, root := newStore(t)
	if err := os.WriteFile(filepath.Join(root, string(acct)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := []byte("put target")
	if err := s.Put(context.Background(), acct, blob.IdFor(data), data); err == nil {
		t.Fatal("Put under a file-shadowed account dir: want an error")
	}
	if n := countFiles(t, filepath.Join(root, "tmp")); n != 0 {
		t.Fatalf("failed Put left %d file(s) in tmp", n)
	}
}

// Put allocates its own temporary file independently of Create; that
// allocation failing (tmp/ missing) is a distinct branch from the
// account-directory MkdirAll failure above.
func TestPutFailsWhenTmpDirMissing(t *testing.T) {
	s, root := newStore(t)
	if err := os.RemoveAll(filepath.Join(root, "tmp")); err != nil {
		t.Fatal(err)
	}
	data := []byte("no tmp dir to stage through")
	if err := s.Put(context.Background(), acct, blob.IdFor(data), data); err == nil {
		t.Fatal("Put with tmp/ missing: want an error")
	}
}

func TestPutFailsWhenDestinationIsADirectory(t *testing.T) {
	s, root := newStore(t)
	data := []byte("put collides with a directory")
	id := blob.IdFor(data)
	dst := filepath.Join(root, string(acct), string(id[1:3]), string(id))
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), acct, id, data); err == nil {
		t.Fatal("Put onto an existing directory: want an error")
	}
}

// Delete propagates a failure unlinking a blob: the containing shard
// directory is read-only, so the file exists and is readable but cannot be
// removed - distinct from the already-covered "missing, so a no-op" path.
func TestDeletePropagatesRemoveFailure(t *testing.T) {
	skipIfRoot(t)
	s, root := newStore(t)
	id := write(t, s, []byte("undeletable"))
	shard := filepath.Join(root, string(acct), string(id[1:3]))
	if err := os.Chmod(shard, 0o500); err != nil { // read+execute, no write
		t.Fatal(err)
	}
	chmodCleanup(t, shard, 0o700)

	if err := s.Delete(context.Background(), acct, id); err == nil {
		t.Fatal("Delete from a read-only directory: want an error")
	}
}

// Open propagates a permission failure distinctly from ErrNotFound: the
// blob exists but this process cannot read it.
func TestOpenPropagatesPermissionFailure(t *testing.T) {
	skipIfRoot(t)
	s, root := newStore(t)
	id := write(t, s, []byte("unreadable"))
	path := filepath.Join(root, string(acct), string(id[1:3]), string(id))
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	chmodCleanup(t, path, 0o600)

	_, _, err := s.Open(context.Background(), acct, id)
	if err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Open of an unreadable blob = %v, want a non-ErrNotFound error", err)
	}
}

// syncDir itself, isolated from Commit/Put: a missing directory cannot be
// opened for the fsync. Driving this through Commit's own call site would
// need the destination directory to vanish in the narrow window between
// Rename and syncDir, which is exactly the kind of race this direct,
// deterministic call avoids.
func TestSyncDirMissingDirectory(t *testing.T) {
	if err := fsstore.SyncDir(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("SyncDir of a missing directory: want an error")
	}
}

// countFiles counts the regular files under dir, recursively.
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return n
}
