package objectdb

// Tests for OrderBy index ordering: an Indexed property that declares
// OrderBy stores its index entries sorted by the named sibling
// properties, then id, so IdsWhereEqual returns records in that order
// with no record loads (the shape behind Thread's emailIds ordering,
// RFC 8621 section 3).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// orderedType groups records by an indexed id, ordered per group by an
// immutable date.
func orderedType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "TestOrdered",
		Capability: "urn:example:ordered",
		Properties: map[string]descriptor.Property{
			"group": {Kind: descriptor.KindId, Indexed: true, OrderBy: []string{"at"}},
			"at":    {Kind: descriptor.KindDate, Immutable: true},
		},
	}
}

func orderedDB(t *testing.T) *DB {
	t.Helper()
	be := memory.New()
	db := New(be, lease.NewInProcess(be), WithVerifyPreImages())
	if err := db.RegisterType(orderedType()); err != nil {
		t.Fatal(err)
	}
	return db
}

func createOrdered(t *testing.T, db *DB, group string, at json.RawMessage) jmap.Id {
	t.Helper()
	var id jmap.Id
	_, err := db.Update(context.Background(), acct, func(u *Update) error {
		obj := Object{"group": rawStr(group)}
		if at != nil {
			obj["at"] = at
		}
		var err error
		id, err = u.Create("TestOrdered", obj)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestOrderByScanOrder proves records created in arbitrary order come
// back ordered by the OrderBy property: a record with an earlier date
// created later still files into its chronological position, because
// the position is the key, written once at create.
func TestOrderByScanOrder(t *testing.T) {
	db := orderedDB(t)
	ctx := context.Background()
	late := createOrdered(t, db, "g1", rawStr("2026-07-17T12:00:00Z"))
	early := createOrdered(t, db, "g1", rawStr("2026-07-17T08:00:00Z"))
	mid := createOrdered(t, db, "g1", rawStr("2026-07-17T10:00:00Z"))
	other := createOrdered(t, db, "g2", rawStr("2026-07-17T00:00:00Z"))

	got, err := db.IdsWhereEqual(ctx, acct, "TestOrdered", "group", rawStr("g1"))
	if err != nil {
		t.Fatal(err)
	}
	want := []jmap.Id{early, mid, late}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("scan order = %v, want %v", got, want)
	}
	// The other group is untouched by g1's entries.
	got, err = db.IdsWhereEqual(ctx, acct, "TestOrdered", "group", rawStr("g2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != other {
		t.Fatalf("g2 = %v, want [%v]", got, other)
	}
}

// TestOrderByEqualDates proves records with an identical ordering value
// fall back to id order - the stable tiebreak (RFC 8621 section 3
// recommends id). The store uses the sequence id scheme, whose ids
// ascend in creation order by construction, so the expected order is
// simply the creation order - no computed expectations.
func TestOrderByEqualDates(t *testing.T) {
	be := memory.New()
	db := New(be, lease.NewInProcess(be), WithVerifyPreImages(), WithIdScheme(tuning.SchemeSequence))
	if err := db.RegisterType(orderedType()); err != nil {
		t.Fatal(err)
	}
	at := rawStr("2026-07-17T10:00:00Z")
	want := []jmap.Id{
		createOrdered(t, db, "g1", at),
		createOrdered(t, db, "g1", at),
		createOrdered(t, db, "g1", at),
	}
	got, err := db.IdsWhereEqual(context.Background(), acct, "TestOrdered", "group", rawStr("g1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("tie order = %v, want %v", got, want)
	}
}

// TestOrderByAbsentValue proves a record missing its ordering property
// files before every present value, deterministically, and the write
// succeeds: missing data is recomputable from the record alone and
// never fails a commit.
func TestOrderByAbsentValue(t *testing.T) {
	db := orderedDB(t)
	dated := createOrdered(t, db, "g1", rawStr("0000-01-01T00:00:00Z"))
	blank := createOrdered(t, db, "g1", nil)
	got, err := db.IdsWhereEqual(context.Background(), acct, "TestOrdered", "group", rawStr("g1"))
	if err != nil {
		t.Fatal(err)
	}
	want := []jmap.Id{blank, dated}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("absent-value order = %v, want %v", got, want)
	}
}

// TestOrderByDeleteAndMove proves the delete path rebuilds the exact
// key the create wrote (destroy removes the entry) and that a change to
// the indexed value itself moves the entry to the new value's range at
// the same ordered position.
func TestOrderByDeleteAndMove(t *testing.T) {
	db := orderedDB(t)
	ctx := context.Background()
	a := createOrdered(t, db, "g1", rawStr("2026-07-17T08:00:00Z"))
	b := createOrdered(t, db, "g1", rawStr("2026-07-17T10:00:00Z"))

	// Destroy a: its entry (including ordering segments) must go.
	_, err := db.Update(ctx, acct, func(u *Update) error {
		return u.Destroy("TestOrdered", a)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.IdsWhereEqual(ctx, acct, "TestOrdered", "group", rawStr("g1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != b {
		t.Fatalf("after destroy = %v, want [%v]", got, b)
	}

	// Move b to g2: the entry leaves g1's range and appears in g2's,
	// ordered by its unchanged date.
	early := createOrdered(t, db, "g2", rawStr("2026-07-17T09:00:00Z"))
	_, err = db.Update(ctx, acct, func(u *Update) error {
		obj, err := u.Get("TestOrdered", b)
		if err != nil {
			return err
		}
		// Get results are shared views; clone before mutating.
		moved := make(Object, len(obj))
		for k, v := range obj {
			moved[k] = v
		}
		moved["group"] = rawStr("g2")
		return u.Put("TestOrdered", b, moved)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = db.IdsWhereEqual(ctx, acct, "TestOrdered", "group", rawStr("g1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("g1 after move = %v, want empty", got)
	}
	got, err = db.IdsWhereEqual(ctx, acct, "TestOrdered", "group", rawStr("g2"))
	if err != nil {
		t.Fatal(err)
	}
	want := []jmap.Id{early, b}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("g2 after move = %v, want %v", got, want)
	}
}
