package postgres

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/backendtest"
)

// PG_TEST_DSN points at a Postgres server to test against (e.g.
// postgres://user:pass@host:port/anydb?sslmode=disable); the dbname in it is
// only used to open the admin connection that creates/drops each test's own
// database, never written to directly. Tests skip, not fail, when unset -
// this suite exercises real network I/O against a real server and has no
// in-process fallback.
const dsnEnv = "PG_TEST_DSN"

var notIdentSafe = regexp.MustCompile(`[^a-z0-9_]`)

// dbNameFor derives a valid, unique-enough Postgres database name from a
// subtest name (e.g. "TestContract/ScanOrdering").
func dbNameFor(testName string) string {
	name := "naust_test_" + notIdentSafe.ReplaceAllString(strings.ToLower(testName), "_")
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// withDSN swaps the dbname in base for name.
func withDSN(base, name string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// openTestDB creates a fresh database for the current subtest (dropped via
// t.Cleanup) and returns a Store connected to it. Persistence tests reopen
// the same database rather than dropping it between Open and Reopen.
func openTestDB(t *testing.T) *Store {
	t.Helper()
	base := os.Getenv(dsnEnv)
	if base == "" {
		t.Skipf("%s not set; skipping Postgres backend tests", dsnEnv)
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close()

	name := dbNameFor(t.Name())
	if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, name)); err != nil {
		t.Fatalf("drop stale test db: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, name)); err != nil {
		t.Fatalf("create test db: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		admin, err := pgxpool.New(ctx, base)
		if err != nil {
			t.Logf("cleanup admin connect: %v", err)
			return
		}
		defer admin.Close()
		if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, name)); err != nil {
			t.Logf("cleanup drop %s: %v", name, err)
		}
	})

	dsn, err := withDSN(base, name)
	if err != nil {
		t.Fatalf("build dsn: %v", err)
	}
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

// TestContract runs the identical suite every Backend must pass, including
// the persistence tests via the Reopen hook.
func TestContract(t *testing.T) {
	dsns := map[backend.Backend]string{}
	backendtest.Run(t, backendtest.Config{
		Open: func(t *testing.T) backend.Backend {
			s := openTestDB(t)
			dsns[s] = s.pool.Config().ConnString()
			return s
		},
		Reopen: func(t *testing.T, b backend.Backend) backend.Backend {
			dsn := dsns[b]
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(context.Background(), dsn)
			if err != nil {
				t.Fatal(err)
			}
			dsns[s] = dsn
			return s
		},
	})
}
