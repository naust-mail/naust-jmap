package postgres

// BenchmarkSequentialGet measures what loadAndMatch and the generic Foo/get
// handler do today: one Get per key, sequential, against real Postgres
// (PG_TEST_DSN) - to put a real round-trip-cost number on the N+1 pattern
// before touching the Backend interface.

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func benchOpen(b *testing.B) *Store {
	b.Helper()
	base := os.Getenv(dsnEnv)
	if base == "" {
		b.Skipf("%s not set; skipping Postgres round-trip benchmark", dsnEnv)
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		b.Fatalf("admin connect: %v", err)
	}
	defer admin.Close()

	name := "bench_multiget"
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name); err != nil {
		b.Fatalf("drop stale bench db: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		b.Fatalf("create bench db: %v", err)
	}
	b.Cleanup(func() {
		ctx := context.Background()
		admin, err := pgxpool.New(ctx, base)
		if err != nil {
			return
		}
		defer admin.Close()
		_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name)
	})

	dsn, err := withDSN(base, name)
	if err != nil {
		b.Fatal(err)
	}
	s, err := Open(ctx, dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

func BenchmarkSequentialGet(b *testing.B) {
	s := benchOpen(b)
	ctx := context.Background()

	for _, n := range []int{1, 10, 50, 200} {
		keys := make([][]byte, n)
		for i := range keys {
			keys[i] = []byte(fmt.Sprintf("bench-multiget-key-%d", i))
			if _, err := s.pool.Exec(ctx, sqlSet, keys[i], []byte("some representative record value, roughly email-sized")); err != nil {
				b.Fatal(err)
			}
		}
		b.Run(fmt.Sprintf("%d_keys", n), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, k := range keys {
					if _, err := s.Get(ctx, k); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

func BenchmarkBatchedMultiGet(b *testing.B) {
	s := benchOpen(b)
	ctx := context.Background()

	for _, n := range []int{1, 10, 50, 200, 500, 1000, 2000, 5000} {
		keys := make([][]byte, n)
		for i := range keys {
			keys[i] = []byte(fmt.Sprintf("bench-multiget-key-%d", i))
			if _, err := s.pool.Exec(ctx, sqlSet, keys[i], []byte("some representative record value, roughly email-sized")); err != nil {
				b.Fatal(err)
			}
		}
		b.Run(fmt.Sprintf("%d_keys", n), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				vals, err := s.MultiGet(ctx, keys)
				if err != nil {
					b.Fatal(err)
				}
				if len(vals) != n {
					b.Fatalf("got %d results, want %d", len(vals), n)
				}
			}
		})
	}
}

func TestMultiGetMatchesGet(t *testing.T) {
	s := openTestDB(t)
	ctx := context.Background()

	present := [][]byte{[]byte("mg-a"), []byte("mg-b"), []byte("mg-c")}
	for i, k := range present {
		if _, err := s.pool.Exec(ctx, sqlSet, k, []byte(fmt.Sprintf("value-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	// Mix present, absent, and a duplicate key.
	keys := [][]byte{present[0], []byte("mg-missing"), present[1], present[0], present[2]}

	got, err := s.MultiGet(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(keys) {
		t.Fatalf("got %d results, want %d", len(got), len(keys))
	}
	for i, k := range keys {
		want, err := s.Get(ctx, k)
		if err != nil {
			want = nil
		}
		if string(got[i]) != string(want) {
			t.Errorf("key %d (%q): MultiGet = %q, Get = %q", i, k, got[i], want)
		}
	}
	if got[1] != nil {
		t.Errorf("missing key: got %q, want nil", got[1])
	}
}
