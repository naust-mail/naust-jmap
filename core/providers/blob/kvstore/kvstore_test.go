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
