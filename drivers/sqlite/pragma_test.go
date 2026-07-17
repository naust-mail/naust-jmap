package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// TestOpenPragmas pins the effective per-connection settings Open promises,
// on a freshly created database. Two of them are easy to lose silently:
// page_size only holds because Open bootstraps the file before WAL pins it
// (the driver applies DSN pragmas in lexicographic order, so a DSN-borne
// page_size runs after journal_mode and does nothing), and
// wal_autocheckpoint(0) is what keeps checkpoints off the writer's commit
// path (checkpointLoop does them instead).
func TestOpenPragmas(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "kv.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	want := map[string]struct{ w, r string }{
		"page_size":          {"16384", "16384"},
		"journal_mode":       {"wal", "wal"},
		"synchronous":        {"1", "1"}, // NORMAL
		"wal_autocheckpoint": {"0", "1000"},
	}
	for pragma, exp := range want {
		var wv, rv string
		if err := s.w.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&wv); err != nil {
			t.Fatalf("writer %s: %v", pragma, err)
		}
		if err := s.r.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&rv); err != nil {
			t.Fatalf("reader %s: %v", pragma, err)
		}
		if wv != exp.w || rv != exp.r {
			t.Errorf("%s: writer=%s reader=%s, want writer=%s reader=%s", pragma, wv, rv, exp.w, exp.r)
		}
	}
}

// TestReopenKeepsPageSize: page_size is a creation-time property, so a
// reopen of an existing file must find it already 16 KiB rather than try
// (and silently fail) to change it.
func TestReopenKeepsPageSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kv.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var got string
	if err := s.w.QueryRowContext(context.Background(), "PRAGMA page_size").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "16384" {
		t.Fatalf("page_size after reopen = %s, want 16384", got)
	}
}
