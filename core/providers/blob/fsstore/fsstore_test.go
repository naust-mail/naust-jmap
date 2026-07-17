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
