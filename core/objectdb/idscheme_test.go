package objectdb

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// End-to-end tests of the id schemes through construction options: the
// scheme is chosen per store with WithIdScheme (defaulting to
// tuning.DefaultIdScheme) and every Create in that store draws from it.

func newSchemeDB(t *testing.T, opts ...Option) *DB {
	t.Helper()
	be := memory.New()
	db := New(be, lease.NewInProcess(be), opts...)
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestIdSchemeDefaultAndOverride(t *testing.T) {
	if got := newSchemeDB(t).idScheme; got != tuning.DefaultIdScheme {
		t.Fatalf("default scheme = %v, want %v", got, tuning.DefaultIdScheme)
	}
	for _, s := range []tuning.IdScheme{tuning.SchemeULID, tuning.SchemeSequence, tuning.SchemeRandom} {
		if got := newSchemeDB(t, WithIdScheme(s)).idScheme; got != s {
			t.Fatalf("WithIdScheme(%v) stored %v", s, got)
		}
	}
}

// TestIdSchemesEndToEnd: under every scheme, created records get valid,
// distinct, N-prefixed ids and are readable back under them.
func TestIdSchemesEndToEnd(t *testing.T) {
	for _, s := range []tuning.IdScheme{tuning.SchemeULID, tuning.SchemeSequence, tuning.SchemeRandom} {
		t.Run(s.String(), func(t *testing.T) {
			db := newSchemeDB(t, WithIdScheme(s))
			seen := make(map[jmap.Id]bool)
			for i := 0; i < 10; i++ {
				id, _ := create(t, db, note("s", "b"))
				if !id.Valid() || id[0] != 'N' {
					t.Fatalf("scheme %v produced id %q", s, id)
				}
				if seen[id] {
					t.Fatalf("scheme %v repeated id %q", s, id)
				}
				seen[id] = true
				if _, err := db.Get(context.Background(), acct, "TestNote", id); err != nil {
					t.Fatalf("scheme %v: created record unreadable: %v", s, err)
				}
			}
		})
	}
}

// TestULIDSchemeSortsByCreation: with the store clock advancing between
// commits, ULID ids sort in creation order (the scheme's whole point:
// index locality and orderable ids).
func TestULIDSchemeSortsByCreation(t *testing.T) {
	clk := time.UnixMilli(1_720_000_000_000)
	db := newSchemeDB(t, WithIdScheme(tuning.SchemeULID), WithNow(func() time.Time { return clk }))
	var ids []jmap.Id
	for i := 0; i < 20; i++ {
		id, _ := create(t, db, note("s", "b"))
		ids = append(ids, id)
		clk = clk.Add(time.Millisecond)
	}
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] < ids[j] }) {
		t.Fatal("ULID ids are not in creation order")
	}
}

// TestSequenceSchemeSortsByCreation: sequence ids sort by in-account
// creation order across commits AND within one commit (the per-commit
// index), with no wall-clock involved - the store clock never advances here.
func TestSequenceSchemeSortsByCreation(t *testing.T) {
	fixed := time.UnixMilli(1_720_000_000_000)
	db := newSchemeDB(t, WithIdScheme(tuning.SchemeSequence), WithNow(func() time.Time { return fixed }))

	// Ten commits of three creates each: order must hold across both the
	// commit boundary (the per-account sequence) and the within-commit
	// boundary (the per-commit index).
	var ids []jmap.Id
	for c := 0; c < 10; c++ {
		if _, err := db.Update(context.Background(), acct, func(u *Update) error {
			for i := 0; i < 3; i++ {
				id, err := u.Create("TestNote", note("s", "b"))
				if err != nil {
					return err
				}
				ids = append(ids, id)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < len(ids); i++ {
		if !(ids[i-1] < ids[i]) {
			t.Fatalf("sequence ids out of creation order at %d: %q >= %q", i, ids[i-1], ids[i])
		}
	}
}

// TestIdSchemesConcurrentNoDuplicates: concurrent commits across several
// accounts must never mint the same id twice, under every scheme. For
// Sequence this is what the random tail is for - different accounts share
// the same sequence numbering space.
func TestIdSchemesConcurrentNoDuplicates(t *testing.T) {
	for _, s := range []tuning.IdScheme{tuning.SchemeULID, tuning.SchemeSequence, tuning.SchemeRandom} {
		t.Run(s.String(), func(t *testing.T) {
			db := newSchemeDB(t, WithIdScheme(s))
			accounts := []jmap.Id{"Aone", "Atwo", "Athree", "Afour"}
			var mu sync.Mutex
			seen := make(map[jmap.Id]jmap.Id) // id -> account that minted it
			var wg sync.WaitGroup
			for _, a := range accounts {
				wg.Add(1)
				go func(a jmap.Id) {
					defer wg.Done()
					for i := 0; i < 25; i++ {
						_, err := db.Update(context.Background(), a, func(u *Update) error {
							id, err := u.Create("TestNote", note("s", "b"))
							if err != nil {
								return err
							}
							mu.Lock()
							defer mu.Unlock()
							if prev, dup := seen[id]; dup {
								t.Errorf("scheme %v minted %q for both %q and %q", s, id, prev, a)
							}
							seen[id] = a
							return nil
						})
						if err != nil {
							t.Errorf("update: %v", err)
							return
						}
					}
				}(a)
			}
			wg.Wait()
			if len(seen) != len(accounts)*25 {
				t.Fatalf("minted %d distinct ids, want %d", len(seen), len(accounts)*25)
			}
		})
	}
}
