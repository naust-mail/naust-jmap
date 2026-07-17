package kvstore

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
)

func TestPutOpenDelete(t *testing.T) {
	s := New(memory.New())
	ctx := context.Background()
	acct, other := jmap.Id("Aone"), jmap.Id("Atwo")
	content := []byte("some binary\x00content")
	blobID := blob.IdFor(content)

	if _, _, err := s.Open(ctx, acct, blobID); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("missing blob -> %v, want ErrNotFound", err)
	}
	if err := s.Put(ctx, acct, blobID, content); err != nil {
		t.Fatal(err)
	}
	rc, size, err := s.Open(ctx, acct, blobID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(content) || size != int64(len(content)) {
		t.Fatalf("round trip: %q size %d", got, size)
	}

	// The namespace is per account: the same blobId does not exist in
	// another account until put there.
	if _, _, err := s.Open(ctx, other, blobID); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("other account sees the blob: %v", err)
	}

	// Idempotent overwrite and delete.
	if err := s.Put(ctx, acct, blobID, content); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, acct, blobID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(ctx, acct, blobID); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("deleted blob still opens: %v", err)
	}
	if err := s.Delete(ctx, acct, blobID); err != nil {
		t.Fatalf("double delete must succeed: %v", err)
	}
}

// TestCreateStreamsContentAddress: bytes written in several chunks commit to
// the content address IdFor would compute for the whole, and read back intact.
func TestCreateStreamsContentAddress(t *testing.T) {
	s := New(memory.New())
	ctx := context.Background()
	acct := jmap.Id("Aone")
	content := []byte("streamed\x00binary content across chunks")

	w, err := s.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	// Write in three chunks to prove the id is over the whole, not a chunk.
	for _, chunk := range [][]byte{content[:8], content[8:20], content[20:]} {
		if n, err := w.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("write chunk: n=%d err=%v", n, err)
		}
	}
	id, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if id != blob.IdFor(content) {
		t.Fatalf("committed id = %q, want the content address %q", id, blob.IdFor(content))
	}
	rc, size, err := s.Open(ctx, acct, id)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != string(content) || size != int64(len(content)) {
		t.Fatalf("round trip: %q size %d", got, size)
	}
}

// TestWriterIDBeforeCommit: ID reports the content address of the bytes
// written without publishing them, and matches the id Commit returns.
func TestWriterIDBeforeCommit(t *testing.T) {
	s := New(memory.New())
	ctx := context.Background()
	acct := jmap.Id("Aone")
	content := []byte("record me before publishing")

	w, err := s.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	id := w.ID()
	if id != blob.IdFor(content) {
		t.Fatalf("ID() = %q, want content address %q", id, blob.IdFor(content))
	}
	// ID must not publish: the content is not readable until Commit.
	if _, _, err := s.Open(ctx, acct, id); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("ID() published content early: Open err = %v", err)
	}
	committed, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if committed != id {
		t.Fatalf("Commit id = %q, want ID() %q", committed, id)
	}
}

// TestAbortStoresNothing: an aborted writer leaves no blob, and a finalized
// writer refuses further use.
func TestAbortStoresNothing(t *testing.T) {
	s := New(memory.New())
	ctx := context.Background()
	acct := jmap.Id("Aone")
	content := []byte("discarded")

	w, err := s.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Open(ctx, acct, blob.IdFor(content)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("aborted blob exists: %v", err)
	}
	// A finalized writer refuses further Write/Commit/Abort.
	if _, err := w.Write(content); err == nil {
		t.Fatal("write after abort must error")
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("commit after abort must error")
	}
}

// TestCommitFinalizes: a second Commit is refused, so a deferred Abort after a
// successful Commit is a harmless no-op error rather than a double store.
func TestCommitFinalizes(t *testing.T) {
	s := New(memory.New())
	ctx := context.Background()
	acct := jmap.Id("Aone")

	w, err := s.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(); err == nil {
		t.Fatal("second commit must error")
	}
	if err := w.Abort(); err == nil {
		t.Fatal("abort after commit must error (no-op)")
	}
}
