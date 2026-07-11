package objectdb

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

const acct = jmap.Id("Atest")

func noteType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "TestNote",
		Capability: "https://naust.email/test/notes",
		Properties: map[string]descriptor.Property{
			"subject": {Kind: descriptor.KindString, Indexed: true},
			"body":    {Kind: descriptor.KindString},
			"pinned":  {Kind: descriptor.KindBool},
		},
	}
}

func newDB(t *testing.T) *DB {
	t.Helper()
	be := memory.New()
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	return db
}

func note(subject, body string) Object {
	s, _ := json.Marshal(subject)
	b, _ := json.Marshal(body)
	return Object{"subject": s, "body": b}
}

func create(t *testing.T, db *DB, obj Object) (jmap.Id, string) {
	t.Helper()
	var id jmap.Id
	states, err := db.Update(context.Background(), acct, func(u *Update) error {
		var err error
		id, err = u.Create("TestNote", obj)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, states["TestNote"]
}

func TestCreateGetPutDestroy(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	if _, err := db.Get(ctx, acct, "TestNote", "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: %v", err)
	}
	if _, err := db.Get(ctx, acct, "NoSuch", "x"); !errors.Is(err, ErrUnknownType) {
		t.Errorf("unknown type: %v", err)
	}
	if state, _ := db.TypeState(ctx, acct, "TestNote"); state != "0" {
		t.Errorf("pristine state = %q, want 0", state)
	}

	id, state1 := create(t, db, note("Hello", "World"))
	if !id.Valid() {
		t.Fatalf("invalid id %q", id)
	}
	if state1 == "0" || state1 == "" {
		t.Fatalf("state after create = %q", state1)
	}
	got, err := db.Get(ctx, acct, "TestNote", id)
	if err != nil {
		t.Fatal(err)
	}
	var subj string
	if json.Unmarshal(got["subject"], &subj); subj != "Hello" {
		t.Errorf("subject = %q", subj)
	}
	var storedId jmap.Id
	if json.Unmarshal(got["id"], &storedId); storedId != id {
		t.Errorf("stored id = %q, want %q", storedId, id)
	}

	// Update via Put; state advances.
	updated := note("Hello2", "World")
	idRaw, _ := json.Marshal(id)
	updated["id"] = idRaw
	states, err := db.Update(ctx, acct, func(u *Update) error {
		return u.Put("TestNote", id, updated)
	})
	if err != nil {
		t.Fatal(err)
	}
	if states["TestNote"] == state1 {
		t.Error("state did not advance on update")
	}

	// Destroy; record gone, state advances again.
	states2, err := db.Update(ctx, acct, func(u *Update) error {
		return u.Destroy("TestNote", id)
	})
	if err != nil {
		t.Fatal(err)
	}
	if states2["TestNote"] == states["TestNote"] {
		t.Error("state did not advance on destroy")
	}
	if _, err := db.Get(ctx, acct, "TestNote", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("destroyed record: %v", err)
	}
	// Destroying again reports not found.
	_, err = db.Update(ctx, acct, func(u *Update) error {
		return u.Destroy("TestNote", id)
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("double destroy: %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	cases := map[string]Object{
		"unknown property": {"nope": json.RawMessage(`1`)},
		"wrong kind":       {"subject": json.RawMessage(`42`)},
		"carries id":       {"id": json.RawMessage(`"x"`)},
	}
	for name, obj := range cases {
		_, err := db.Update(ctx, acct, func(u *Update) error {
			_, err := u.Create("TestNote", obj)
			return err
		})
		if err == nil {
			t.Errorf("%s: create succeeded", name)
		}
	}
}

// Section 5.2: a record created and destroyed within the window leaves
// no trace at all.
func TestCreateDestroySameUpdateLeavesNoTrace(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	before, _ := db.TypeState(ctx, acct, "TestNote")
	_, err := db.Update(ctx, acct, func(u *Update) error {
		id, err := u.Create("TestNote", note("ghost", ""))
		if err != nil {
			return err
		}
		return u.Destroy("TestNote", id)
	})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := db.TypeState(ctx, acct, "TestNote")
	if after != before {
		t.Errorf("state moved %q -> %q for a no-op commit", before, after)
	}
	cs, err := db.Changes(ctx, acct, "TestNote", "0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Created)+len(cs.Updated)+len(cs.Destroyed) != 0 {
		t.Errorf("ghost left a trace: %+v", cs)
	}
}

func TestChangesCoalescing(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	id1, s1 := create(t, db, note("one", ""))
	// created then updated since "0" -> reported created only (5.2).
	obj, _ := db.Get(ctx, acct, "TestNote", id1)
	obj["body"] = json.RawMessage(`"edited"`)
	if _, err := db.Update(ctx, acct, func(u *Update) error { return u.Put("TestNote", id1, obj) }); err != nil {
		t.Fatal(err)
	}
	cs, err := db.Changes(ctx, acct, "TestNote", "0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Created) != 1 || cs.Created[0] != id1 || len(cs.Updated) != 0 {
		t.Errorf("created+updated: %+v", cs)
	}
	if cs.OldState != "0" {
		t.Errorf("oldState = %q", cs.OldState)
	}

	// updated then destroyed since s1 -> destroyed only (5.2).
	if _, err := db.Update(ctx, acct, func(u *Update) error { return u.Destroy("TestNote", id1) }); err != nil {
		t.Fatal(err)
	}
	cs, err = db.Changes(ctx, acct, "TestNote", s1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Destroyed) != 1 || cs.Destroyed[0] != id1 || len(cs.Updated) != 0 || len(cs.Created) != 0 {
		t.Errorf("updated+destroyed: %+v", cs)
	}
	// created then destroyed since "0" -> omitted entirely (5.2).
	cs, err = db.Changes(ctx, acct, "TestNote", "0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Created)+len(cs.Updated)+len(cs.Destroyed) != 0 {
		t.Errorf("created+destroyed: %+v", cs)
	}
	// Caught-up newState equals the /get state (one state space).
	state, _ := db.TypeState(ctx, acct, "TestNote")
	if cs.NewState != state {
		t.Errorf("newState %q != type state %q", cs.NewState, state)
	}
}

func TestChangesBadStates(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	create(t, db, note("x", ""))
	for _, since := range []string{"abc", "-1", "999999"} {
		if _, err := db.Changes(ctx, acct, "TestNote", since, 0); !errors.Is(err, ErrCannotCalculateChanges) {
			t.Errorf("since %q: err = %v, want ErrCannotCalculateChanges", since, err)
		}
	}
}

// Section 5.2: maxChanges paging through intermediate states, with the
// ordering guarantee across pages.
func TestChangesPaging(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	var ids []jmap.Id
	for i := 0; i < 5; i++ {
		id, _ := create(t, db, note("n", ""))
		ids = append(ids, id)
	}
	seen := make(map[jmap.Id]bool)
	state := "0"
	pages := 0
	for {
		cs, err := db.Changes(ctx, acct, "TestNote", state, 2)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(cs.Created) + len(cs.Updated) + len(cs.Destroyed); n > 2 {
			t.Fatalf("page has %d ids, maxChanges 2", n)
		}
		for _, id := range cs.Created {
			if seen[id] {
				t.Errorf("id %s reported twice", id)
			}
			seen[id] = true
		}
		state = cs.NewState
		pages++
		if !cs.HasMore {
			break
		}
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != len(ids) {
		t.Errorf("paged ids = %d, want %d", len(seen), len(ids))
	}
	final, _ := db.TypeState(ctx, acct, "TestNote")
	if state != final {
		t.Errorf("final paged state %q != type state %q", state, final)
	}
}

// A single commit larger than maxChanges cannot produce an intermediate
// state (5.2) -> cannotCalculateChanges.
func TestChangesEntryLargerThanMax(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	_, err := db.Update(ctx, acct, func(u *Update) error {
		for i := 0; i < 3; i++ {
			if _, err := u.Create("TestNote", note("bulk", "")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Changes(ctx, acct, "TestNote", "0", 2); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("oversized entry: %v", err)
	}
}

// Cross-type commits: one Update touching two types advances both
// states identically and each type's /changes sees only its own ids
// (the cross-type hook mechanism, e.g. Email/set bumping Mailbox).
func TestCrossTypeUpdate(t *testing.T) {
	db := newDB(t)
	other := &descriptor.Type{
		Name:       "TestCounter",
		Capability: "https://naust.email/test/notes",
		Properties: map[string]descriptor.Property{
			"total": {Kind: descriptor.KindUnsignedInt},
		},
	}
	if err := db.RegisterType(other); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var noteId, ctrId jmap.Id
	states, err := db.Update(ctx, acct, func(u *Update) error {
		var err error
		if noteId, err = u.Create("TestNote", note("n", "")); err != nil {
			return err
		}
		ctrId, err = u.Create("TestCounter", Object{"total": json.RawMessage(`1`)})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if states["TestNote"] != states["TestCounter"] || states["TestNote"] == "" {
		t.Errorf("states = %v, want both types at one sequence", states)
	}
	csNote, _ := db.Changes(ctx, acct, "TestNote", "0", 0)
	csCtr, _ := db.Changes(ctx, acct, "TestCounter", "0", 0)
	if len(csNote.Created) != 1 || csNote.Created[0] != noteId {
		t.Errorf("note changes: %+v", csNote)
	}
	if len(csCtr.Created) != 1 || csCtr.Created[0] != ctrId {
		t.Errorf("counter changes: %+v", csCtr)
	}
}

func TestAllIdsAndOverflow(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	want := make(map[jmap.Id]bool)
	for i := 0; i < 4; i++ {
		id, _ := create(t, db, note("s", ""))
		want[id] = true
	}
	ids, err := db.AllIds(ctx, acct, "TestNote", 0)
	if err != nil || len(ids) != 4 {
		t.Fatalf("AllIds: %v %v", ids, err)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected id %s", id)
		}
	}
	// max smaller than the population: max+1 returned to signal overflow.
	ids, err = db.AllIds(ctx, acct, "TestNote", 2)
	if err != nil || len(ids) != 3 {
		t.Errorf("overflow signal: got %d ids (%v)", len(ids), err)
	}
}

// Accounts are isolated: same type, different account, nothing leaks.
func TestAccountIsolation(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	create(t, db, note("mine", ""))
	otherAcct := jmap.Id("Aother")
	ids, err := db.AllIds(ctx, otherAcct, "TestNote", 0)
	if err != nil || len(ids) != 0 {
		t.Errorf("account leak: %v %v", ids, err)
	}
	if state, _ := db.TypeState(ctx, otherAcct, "TestNote"); state != "0" {
		t.Errorf("other account state = %q", state)
	}
}
