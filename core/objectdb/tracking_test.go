package objectdb

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

func updateNote(t *testing.T, db *DB, id jmap.Id, mutate func(Object)) string {
	t.Helper()
	states, err := db.Update(context.Background(), acct, func(u *Update) error {
		obj, err := u.Get("TestNote", id)
		if err != nil {
			return err
		}
		mutate(obj)
		return u.Put("TestNote", id, obj)
	})
	if err != nil {
		t.Fatal(err)
	}
	return states["TestNote"]
}

// TestChangesUpdatedProps: the change log records which properties each
// commit's updates touched, and /changes surfaces the union across the
// consumed range (the raw material for RFC 8621's Mailbox/changes
// updatedProperties).
func TestChangesUpdatedProps(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	id, state1 := create(t, db, note("Hello", "World"))

	updateNote(t, db, id, func(obj Object) {
		obj["subject"] = json.RawMessage(`"Changed"`)
	})
	cs, err := db.Changes(ctx, acct, "TestNote", state1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cs.UpdatedProps, []string{"subject"}) {
		t.Errorf("UpdatedProps after subject update: %#v", cs.UpdatedProps)
	}

	// A second commit touching other properties widens the union;
	// removing a property counts as changing it.
	updateNote(t, db, id, func(obj Object) {
		obj["pinned"] = json.RawMessage(`true`)
		delete(obj, "body")
	})
	cs, err = db.Changes(ctx, acct, "TestNote", state1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cs.UpdatedProps, []string{"body", "pinned", "subject"}) {
		t.Errorf("UpdatedProps union: %#v", cs.UpdatedProps)
	}
	if len(cs.Updated) != 1 || cs.Updated[0] != id {
		t.Errorf("Updated: %v", cs.Updated)
	}

	// A range with no updates reports a known-empty set, distinct from
	// unknown (nil).
	_, state3 := create(t, db, note("Another", "Note"))
	cs, err = db.Changes(ctx, acct, "TestNote", state3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cs.UpdatedProps == nil || len(cs.UpdatedProps) != 0 {
		t.Errorf("UpdatedProps with no updates: %#v", cs.UpdatedProps)
	}
}

// TestChangesUpdatedPropsLegacyEntry: a log entry written before
// property tracking (nil updatedProps) with updates in it makes the
// whole answer unknown - the consumer MUST fall back to null.
func TestChangesUpdatedPropsLegacyEntry(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	id, state1 := create(t, db, note("Hello", "World"))

	// Hand-write a sequence-2 commit the way pre-tracking code did:
	// log entry without updatedProps, state and sequence advanced.
	raw, _ := json.Marshal(map[string]any{
		"types": map[string]any{
			"TestNote": map[string]any{"updated": []jmap.Id{id}},
		},
	})
	batch := &backend.Batch{}
	batch.Set(logKey(acct, 2), raw)
	batch.Set(typeStateKey(acct, "TestNote"), backend.EncodeInt64(2))
	batch.Set(seqKey(acct), backend.EncodeInt64(2))
	if err := db.be.WriteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}

	cs, err := db.Changes(ctx, acct, "TestNote", state1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Updated) != 1 || cs.Updated[0] != id {
		t.Fatalf("Updated: %v", cs.Updated)
	}
	if cs.UpdatedProps != nil {
		t.Errorf("UpdatedProps for legacy entry: %#v, want nil", cs.UpdatedProps)
	}
}

// TestUpdateIdsWhereEqual: the staged-aware index query sees creates,
// updates, and destroys staged earlier in the same commit, on top of
// the committed matches.
func TestUpdateIdsWhereEqual(t *testing.T) {
	db := newDB(t)
	idA, _ := create(t, db, note("Alpha", "1"))
	idB, _ := create(t, db, note("Beta", "2"))

	_, err := db.Update(context.Background(), acct, func(u *Update) error {
		if _, err := u.IdsWhereEqual("TestNote", "body", json.RawMessage(`"1"`)); err == nil {
			t.Error("IdsWhereEqual on unindexed property succeeded")
		}

		// Committed state first: only Alpha matches.
		ids, err := u.IdsWhereEqual("TestNote", "subject", json.RawMessage(`"alpha"`))
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0] != idA {
			t.Fatalf("committed match: %v", ids)
		}

		// Stage: destroy Alpha, rename Beta to Alpha, create a third
		// Alpha - the staged view must reflect all three.
		if err := u.Destroy("TestNote", idA); err != nil {
			t.Fatal(err)
		}
		objB, err := u.Get("TestNote", idB)
		if err != nil {
			t.Fatal(err)
		}
		objB["subject"] = json.RawMessage(`"Alpha"`)
		if err := u.Put("TestNote", idB, objB); err != nil {
			t.Fatal(err)
		}
		idC, err := u.Create("TestNote", note("ALPHA", "3"))
		if err != nil {
			t.Fatal(err)
		}

		ids, err = u.IdsWhereEqual("TestNote", "subject", json.RawMessage(`"alpha"`))
		if err != nil {
			t.Fatal(err)
		}
		want := map[jmap.Id]bool{idB: true, idC: true}
		if len(ids) != 2 || !want[ids[0]] || !want[ids[1]] {
			t.Fatalf("staged view: %v, want {%s, %s}", ids, idB, idC)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestNullableProperty: Nullable admits the literal null as a stored,
// indexed value distinct from every non-null value and sorting first.
func TestNullableProperty(t *testing.T) {
	nullable := descriptor.Property{Kind: descriptor.KindString, Nullable: true, Indexed: true}
	plain := descriptor.Property{Kind: descriptor.KindString}
	if err := plain.CheckValue(json.RawMessage(`null`)); err == nil {
		t.Error("non-nullable property accepted null")
	}
	if err := nullable.CheckValue(json.RawMessage(`null`)); err != nil {
		t.Errorf("nullable property rejected null: %v", err)
	}
	if err := nullable.CheckValue(json.RawMessage(`5`)); err == nil {
		t.Error("nullable property accepted a wrong-kind value")
	}

	kNull, err := SortKey(nullable, json.RawMessage(`null`))
	if err != nil {
		t.Fatal(err)
	}
	kVal, err := SortKey(nullable, json.RawMessage(`""`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Compare(kNull, kVal) >= 0 {
		t.Errorf("null does not sort before the empty string: %v >= %v", kNull, kVal)
	}

	be := memory.New()
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(&descriptor.Type{
		Name:       "Box",
		Capability: "urn:example:box",
		Properties: map[string]descriptor.Property{
			"parent": {Kind: descriptor.KindId, Nullable: true, Indexed: true},
			"name":   {Kind: descriptor.KindString},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var top, child jmap.Id
	_, err = db.Update(ctx, acct, func(u *Update) error {
		top, err = u.Create("Box", Object{"parent": json.RawMessage(`null`), "name": json.RawMessage(`"Top"`)})
		if err != nil {
			return err
		}
		parent, _ := json.Marshal(top)
		child, err = u.Create("Box", Object{"parent": parent, "name": json.RawMessage(`"Child"`)})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	ids, err := db.IdsWhereEqual(ctx, acct, "Box", "parent", json.RawMessage(`null`))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != top {
		t.Errorf("parent=null matches: %v, want [%s]", ids, top)
	}
	parent, _ := json.Marshal(top)
	ids, err = db.IdsWhereEqual(ctx, acct, "Box", "parent", parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != child {
		t.Errorf("parent=%s matches: %v, want [%s]", top, ids, child)
	}
}
