package objectdb

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// tagType exercises both member kinds: labels is a KindObject
// (String[Boolean], members = keys, like mailboxIds/keywords) and refs a
// KindArray of strings (members = elements, like Email's msgid lists).
func tagType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "Tagged",
		Capability: "https://naust.email/test/tagged",
		Properties: map[string]descriptor.Property{
			"labels": {Kind: descriptor.KindObject, SetIndexed: true},
			"refs":   {Kind: descriptor.KindArray, Nullable: true, SetIndexed: true},
			"note":   {Kind: descriptor.KindString},
		},
	}
}

func newTagDB(t *testing.T) *DB {
	t.Helper()
	be := memory.New()
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(tagType()); err != nil {
		t.Fatal(err)
	}
	return db
}

func raw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func createTagged(t *testing.T, db *DB, obj Object) jmap.Id {
	t.Helper()
	var id jmap.Id
	_, err := db.Update(context.Background(), acct, func(u *Update) error {
		var err error
		id, err = u.Create("Tagged", obj)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func members(t *testing.T, db *DB, prop, member string) []jmap.Id {
	t.Helper()
	ids, err := db.IdsWhereMember(context.Background(), acct, "Tagged", prop, member)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func hasId(ids []jmap.Id, want jmap.Id) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestSetIndexMembership(t *testing.T) {
	db := newTagDB(t)
	ctx := context.Background()

	a := createTagged(t, db, Object{"labels": raw(map[string]bool{"red": true, "blue": true}), "refs": raw([]string{"m1", "m2"})})
	b := createTagged(t, db, Object{"labels": raw(map[string]bool{"blue": true}), "refs": raw([]string{"m2"})})

	// Object-key members: "blue" is on both, "red" only on a.
	if got := members(t, db, "labels", "blue"); len(got) != 2 || !hasId(got, a) || !hasId(got, b) {
		t.Errorf("labels=blue: %v, want both", got)
	}
	if got := members(t, db, "labels", "red"); len(got) != 1 || got[0] != a {
		t.Errorf("labels=red: %v, want [%s]", got, a)
	}
	// Array-element members.
	if got := members(t, db, "refs", "m2"); len(got) != 2 {
		t.Errorf("refs=m2: %v, want both", got)
	}
	if got := members(t, db, "refs", "m1"); len(got) != 1 || got[0] != a {
		t.Errorf("refs=m1: %v", got)
	}
	// A member no record has.
	if got := members(t, db, "labels", "green"); len(got) != 0 {
		t.Errorf("labels=green: %v, want none", got)
	}

	// Update a: drop "red", add "green"; drop ref m1.
	idRaw, _ := json.Marshal(a)
	_, err := db.Update(ctx, acct, func(u *Update) error {
		return u.Put("Tagged", a, Object{"id": idRaw, "labels": raw(map[string]bool{"blue": true, "green": true}), "refs": raw([]string{"m2"})})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := members(t, db, "labels", "red"); len(got) != 0 {
		t.Errorf("labels=red after update: %v, want none", got)
	}
	if got := members(t, db, "labels", "green"); len(got) != 1 || got[0] != a {
		t.Errorf("labels=green after update: %v", got)
	}
	if got := members(t, db, "refs", "m1"); len(got) != 0 {
		t.Errorf("refs=m1 after update: %v, want none", got)
	}

	// Destroy b; "blue" now only a.
	if _, err := db.Update(ctx, acct, func(u *Update) error { return u.Destroy("Tagged", b) }); err != nil {
		t.Fatal(err)
	}
	if got := members(t, db, "labels", "blue"); len(got) != 1 || got[0] != a {
		t.Errorf("labels=blue after destroy: %v", got)
	}
}

func TestSetIndexNullAndExact(t *testing.T) {
	db := newTagDB(t)

	// Nullable refs = null contributes no members; empty label object too.
	a := createTagged(t, db, Object{"labels": raw(map[string]bool{}), "refs": json.RawMessage("null")})
	if got := members(t, db, "refs", "m1"); len(got) != 0 {
		t.Errorf("null refs indexed: %v", got)
	}

	// Matching is case-sensitive (no i;ascii-casemap fold): "M1" != "m1".
	b := createTagged(t, db, Object{"labels": raw(map[string]bool{"Red": true}), "refs": raw([]string{"M1"})})
	if got := members(t, db, "labels", "red"); len(got) != 0 {
		t.Errorf("case-folded object key matched: %v", got)
	}
	if got := members(t, db, "labels", "Red"); len(got) != 1 || got[0] != b {
		t.Errorf("labels=Red: %v", got)
	}
	if got := members(t, db, "refs", "m1"); len(got) != 0 {
		t.Errorf("case-folded array member matched: %v", got)
	}
	_ = a
}

func TestSetIndexStagedAware(t *testing.T) {
	db := newTagDB(t)
	ctx := context.Background()

	existing := createTagged(t, db, Object{"labels": raw(map[string]bool{"x": true}), "refs": raw([]string{"shared"})})

	// Within one commit, a second record must see the first via the
	// staged-aware Update.IdsWhereMember even before the batch lands.
	_, err := db.Update(ctx, acct, func(u *Update) error {
		id, err := u.Create("Tagged", Object{"labels": raw(map[string]bool{"y": true}), "refs": raw([]string{"shared"})})
		if err != nil {
			return err
		}
		ids, err := u.IdsWhereMember("Tagged", "refs", "shared")
		if err != nil {
			return err
		}
		if len(ids) != 2 || !hasId(ids, existing) || !hasId(ids, id) {
			t.Errorf("staged IdsWhereMember: %v, want existing+new", ids)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSetIndexNotSetIndexed(t *testing.T) {
	db := newTagDB(t)
	if _, err := db.IdsWhereMember(context.Background(), acct, "Tagged", "note", "x"); err == nil {
		t.Error("IdsWhereMember on a non-SetIndexed property should error")
	}
}

// TestLowestMemberId: the bounded lookup returns the smallest matching id and
// stays in lockstep with the full staged-aware IdsWhereMember - it just reads
// one row instead of the whole match set. Ids are random, so the assertions
// compare against IdsWhereMember's own ordering rather than hard-coded ids.
func TestLowestMemberId(t *testing.T) {
	db := newTagDB(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		createTagged(t, db, Object{"refs": raw([]string{"shared"})})
	}
	committed := members(t, db, "refs", "shared") // IdsWhereMember returns ascending
	if len(committed) != 5 {
		t.Fatalf("setup: %d members, want 5", len(committed))
	}

	_, err := db.Update(ctx, acct, func(u *Update) error {
		got, ok, err := u.LowestMemberId("Tagged", "refs", "shared")
		if err != nil || !ok {
			t.Fatalf("LowestMemberId: got=%q ok=%v err=%v", got, ok, err)
		}
		if got != committed[0] {
			t.Errorf("lowest = %q, want %q (smallest committed id)", got, committed[0])
		}
		// A member no record has: ok is false, no error.
		if id, ok, err := u.LowestMemberId("Tagged", "refs", "absent"); err != nil || ok {
			t.Errorf("absent member: id=%q ok=%v err=%v, want ok=false", id, ok, err)
		}
		// Not a set-indexed property: an error, like IdsWhereMember.
		if _, _, err := u.LowestMemberId("Tagged", "note", "x"); err == nil {
			t.Error("LowestMemberId on a non-SetIndexed property should error")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// With the committed lowest staged-destroyed and a fresh member staged in,
	// LowestMemberId must still equal the minimum of the staged-aware
	// IdsWhereMember: the destroyed id is excluded, the created one considered.
	_, err = db.Update(ctx, acct, func(u *Update) error {
		if err := u.Destroy("Tagged", committed[0]); err != nil {
			return err
		}
		if _, err := u.Create("Tagged", Object{"refs": raw([]string{"shared"})}); err != nil {
			return err
		}
		full, err := u.IdsWhereMember("Tagged", "refs", "shared") // ascending
		if err != nil {
			return err
		}
		got, ok, err := u.LowestMemberId("Tagged", "refs", "shared")
		if err != nil {
			return err
		}
		if len(full) == 0 || !ok || got != full[0] {
			t.Errorf("staged lowest = %q ok=%v, want %q (min of %v)", got, ok, first(full), full)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func first(ids []jmap.Id) jmap.Id {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}
