package sqlite

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/chunkstore"
)

// The chunked blob store is backend-agnostic: it streams a blob as a run of
// pieces through any backend.Backend. These tests exercise it over the real
// SQLite backend to prove blobs larger than a single comfortable value can
// be stored, read back, survive a reopen, and have their crash-orphaned
// pieces reclaimed - none of which needs any SQLite-specific blob code.

func fill(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*131 + 17)
	}
	return b
}

func rows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.r.QueryRow(`SELECT count(*) FROM kv`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestChunkstoreOverSQLite streams a multi-piece blob (larger than one
// piece) through SQLite and reads it back byte-for-byte at its content
// address, then reopens the database and confirms the blob is still intact -
// the pieces and manifest are durable, not held in memory.
func TestChunkstoreOverSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob.sqlite")
	be, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := chunkstore.New(be)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	acct := jmap.Id("Aone")
	data := fill(9 << 20) // ~9 MiB: three pieces at the default 4 MiB

	w, err := cs.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	// Feed the writer in uneven bursts so piece boundaries fall mid-burst.
	for off := 0; off < len(data); off += 100_000 {
		end := off + 100_000
		if end > len(data) {
			end = len(data)
		}
		if _, err := w.Write(data[off:end]); err != nil {
			t.Fatal(err)
		}
	}
	id, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if want := blob.IdFor(data); id != want {
		t.Fatalf("id %q, want content address %q", id, want)
	}

	readBack := func(cs *chunkstore.Store) {
		rc, size, err := cs.Open(ctx, acct, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(got)) != size || !bytes.Equal(got, data) {
			t.Fatalf("round trip mismatch: size=%d read=%d", size, len(got))
		}
	}
	readBack(cs)

	// Reopen the database from disk: the blob must still be there.
	if err := be.Close(); err != nil {
		t.Fatal(err)
	}
	be2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer be2.Close()
	cs2, err := chunkstore.New(be2)
	if err != nil {
		t.Fatal(err)
	}
	readBack(cs2)
}

// TestChunkstoreSweepOverSQLite simulates a crash mid-upload: pieces and a
// marker are written but never committed. Reopening the database and
// constructing a fresh store past the reclaim window reclaims the orphaned
// pieces at startup, returning the table to its committed-blob baseline while
// leaving the committed blob intact. The clock is injected so the window is
// crossed without waiting.
func TestChunkstoreSweepOverSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob.sqlite")
	be, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	clk := time.Unix(1_720_000_000, 0)
	now := func() time.Time { return clk }
	cs, err := chunkstore.New(be, chunkstore.WithNow(now))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	acct := jmap.Id("Aone")

	// A committed survivor sets the baseline row count.
	survivor := fill(5 << 20) // two pieces
	sw, err := cs.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Write(survivor); err != nil {
		t.Fatal(err)
	}
	survivorID, err := sw.Commit()
	if err != nil {
		t.Fatal(err)
	}
	baseline := rows(t, be)

	// An upload that streams pieces but never commits (the process dies).
	ow, err := cs.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ow.Write(fill(9 << 20)); err != nil {
		t.Fatal(err)
	}
	if rows(t, be) <= baseline {
		t.Fatal("orphan upload wrote nothing to reclaim")
	}

	// Restart well past the reclaim window: reopen the database and build a
	// fresh store, whose construction sweep sees the stale marker and reclaims.
	if err := be.Close(); err != nil {
		t.Fatal(err)
	}
	clk = clk.Add(16 * time.Minute)
	be2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer be2.Close()
	cs2, err := chunkstore.New(be2, chunkstore.WithNow(now))
	if err != nil {
		t.Fatal(err)
	}
	if got := rows(t, be2); got != baseline {
		t.Fatalf("after sweep rows = %d, want baseline %d", got, baseline)
	}
	// The committed blob is untouched.
	rc, _, err := cs2.Open(ctx, acct, survivorID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, survivor) {
		t.Fatal("sweep damaged the committed blob")
	}
}
