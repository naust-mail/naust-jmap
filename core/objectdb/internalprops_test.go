package objectdb

// PutInternal: bookkeeping writes. A record write that changes only
// Internal properties (descriptor semantics: not part of the type's
// public schema) is persisted and indexed but is not a client-visible
// update - no change-log entry, no state move. RFC 8620 section 5.1:
// the state string SHOULD be stable while the Foo data is unchanged,
// and Internal properties are not part of the Foo data.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
)

// markedType has one visible property and one indexed Internal one.
func markedType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "TestMarked",
		Capability: "https://naust.email/test/marked",
		Properties: map[string]descriptor.Property{
			"subject": {Kind: descriptor.KindString},
			"marker":  {Kind: descriptor.KindString, Indexed: true, Internal: true},
		},
	}
}

func newMarkedDB(t *testing.T) *DB {
	t.Helper()
	be := memory.New()
	db := New(be, lease.NewInProcess(be), WithVerifyPreImages())
	if err := db.RegisterType(markedType()); err != nil {
		t.Fatal(err)
	}
	return db
}

func createMarked(t *testing.T, db *DB) (jmap.Id, string) {
	t.Helper()
	var id jmap.Id
	states, err := db.Update(context.Background(), acct, func(u *Update) error {
		var err error
		id, err = u.Create("TestMarked", Object{
			"subject": json.RawMessage(`"hello"`),
			"marker":  json.RawMessage(`"m1"`),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, states["TestMarked"]
}

// withMarker clones a record with the Internal property replaced.
func withMarker(obj Object, marker string) Object {
	next := make(Object, len(obj))
	for k, v := range obj {
		next[k] = v
	}
	raw, _ := json.Marshal(marker)
	next["marker"] = raw
	return next
}

// An Internal-only write is persisted and indexed but invisible to the
// sync surface: the type's state does not move and /changes from the
// prior state stays empty.
func TestPutInternalIsSilent(t *testing.T) {
	db := newMarkedDB(t)
	ctx := context.Background()
	id, state := createMarked(t, db)

	states, err := db.Update(ctx, acct, func(u *Update) error {
		obj, err := u.Get("TestMarked", id)
		if err != nil {
			return err
		}
		return u.PutInternal("TestMarked", id, withMarker(obj, "m2"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if s, moved := states["TestMarked"]; moved {
		t.Errorf("bookkeeping write moved the type state to %q", s)
	}

	cs, err := db.Changes(ctx, acct, "TestMarked", state, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Created)+len(cs.Updated)+len(cs.Destroyed) != 0 {
		t.Errorf("bookkeeping write is visible in /changes: %+v", cs)
	}

	// The write itself landed, and so did its index maintenance.
	obj, err := db.Get(ctx, acct, "TestMarked", id)
	if err != nil {
		t.Fatal(err)
	}
	if string(obj["marker"]) != `"m2"` {
		t.Errorf("marker = %s, want \"m2\"", obj["marker"])
	}
	ids, err := db.IdsWhereEqual(ctx, acct, "TestMarked", "marker", json.RawMessage(`"m2"`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != id {
		t.Errorf("index at new value = %v, want [%s]", ids, id)
	}
	stale, err := db.IdsWhereEqual(ctx, acct, "TestMarked", "marker", json.RawMessage(`"m1"`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Errorf("index still lists the old value: %v", stale)
	}
}

// PutInternal refuses, at the call, to change anything outside the
// type's Internal properties.
func TestPutInternalRejectsVisibleChange(t *testing.T) {
	db := newMarkedDB(t)
	ctx := context.Background()
	id, _ := createMarked(t, db)

	_, err := db.Update(ctx, acct, func(u *Update) error {
		obj, err := u.Get("TestMarked", id)
		if err != nil {
			return err
		}
		next := withMarker(obj, "m2")
		next["subject"] = json.RawMessage(`"changed"`)
		return u.PutInternal("TestMarked", id, next)
	})
	if err == nil || !strings.Contains(err.Error(), "non-Internal") {
		t.Fatalf("visible change through PutInternal: err = %v, want rejection", err)
	}
	// The failed Update committed nothing.
	obj, err := db.Get(ctx, acct, "TestMarked", id)
	if err != nil {
		t.Fatal(err)
	}
	if string(obj["subject"]) != `"hello"` {
		t.Errorf("subject = %s after rejected update", obj["subject"])
	}
}

// A commit that writes the record both ways reports it: loud wins, in
// either order. The identity Put (the touch pattern announcing a
// computed-property change) counts as loud.
func TestPutInternalLoudWins(t *testing.T) {
	db := newMarkedDB(t)
	ctx := context.Background()

	for _, order := range []string{"internal-then-touch", "touch-then-internal"} {
		t.Run(order, func(t *testing.T) {
			id, state := createMarked(t, db)
			_, err := db.Update(ctx, acct, func(u *Update) error {
				obj, err := u.Get("TestMarked", id)
				if err != nil {
					return err
				}
				internal := func() error { return u.PutInternal("TestMarked", id, withMarker(obj, "m2")) }
				touch := func() error { return u.Put("TestMarked", id, obj) }
				if order == "internal-then-touch" {
					if err := internal(); err != nil {
						return err
					}
					return touch()
				}
				if err := touch(); err != nil {
					return err
				}
				return internal()
			})
			if err != nil {
				t.Fatal(err)
			}
			cs, err := db.Changes(ctx, acct, "TestMarked", state, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(cs.Updated) != 1 || cs.Updated[0] != id {
				t.Errorf("updated = %v, want [%s]", cs.Updated, id)
			}
		})
	}
}

// A record created in the same commit stays a visible creation no
// matter how its Internal properties are then written.
func TestPutInternalOnCreatedRecord(t *testing.T) {
	db := newMarkedDB(t)
	ctx := context.Background()
	_, prior := createMarked(t, db)

	var id jmap.Id
	_, err := db.Update(ctx, acct, func(u *Update) error {
		var err error
		id, err = u.Create("TestMarked", Object{
			"subject": json.RawMessage(`"second"`),
			"marker":  json.RawMessage(`"m1"`),
		})
		if err != nil {
			return err
		}
		obj, err := u.Get("TestMarked", id)
		if err != nil {
			return err
		}
		return u.PutInternal("TestMarked", id, withMarker(obj, "m2"))
	})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := db.Changes(ctx, acct, "TestMarked", prior, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Created) != 1 || cs.Created[0] != id {
		t.Errorf("created = %v, want [%s]", cs.Created, id)
	}
}

// PutInternal needs an existing record and a registered type, like Put.
func TestPutInternalErrors(t *testing.T) {
	db := newMarkedDB(t)
	ctx := context.Background()
	createMarked(t, db)

	_, err := db.Update(ctx, acct, func(u *Update) error {
		raw, _ := json.Marshal(jmap.Id("Rmissing"))
		return u.PutInternal("TestMarked", "Rmissing", Object{"id": raw})
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("missing record: %v, want ErrNotFound", err)
	}

	_, err = db.Update(ctx, acct, func(u *Update) error {
		return u.PutInternal("Nope", "R1", Object{})
	})
	if !errors.Is(err, ErrUnknownType) {
		t.Errorf("unknown type: %v, want ErrUnknownType", err)
	}
}

// A loud update that also carries Internal-property changes reports the
// record updated, but the change log's property list names only the
// visible properties: Internal names are bookkeeping, and the list
// exists to answer questions about visible data (RFC 8621 section 2.2's
// updatedProperties is built from it).
func TestUpdatedPropsExcludeInternal(t *testing.T) {
	db := newMarkedDB(t)
	ctx := context.Background()
	id, state := createMarked(t, db)

	_, err := db.Update(ctx, acct, func(u *Update) error {
		obj, err := u.Get("TestMarked", id)
		if err != nil {
			return err
		}
		next := withMarker(obj, "m2")
		next["subject"] = json.RawMessage(`"changed"`)
		return u.Put("TestMarked", id, next)
	})
	if err != nil {
		t.Fatal(err)
	}

	cs, err := db.Changes(ctx, acct, "TestMarked", state, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Updated) != 1 || cs.Updated[0] != id {
		t.Fatalf("updated = %v, want [%s]", cs.Updated, id)
	}
	if !slices.Contains(cs.UpdatedProps, "subject") {
		t.Errorf("updatedProps %v must name the visible change", cs.UpdatedProps)
	}
	if slices.Contains(cs.UpdatedProps, "marker") {
		t.Errorf("updatedProps %v leaks an Internal property name", cs.UpdatedProps)
	}
}

// capturePublisher records every Publish call; the Subscribe side is
// never used by the DB and panics if reached.
type capturePublisher struct {
	published []jmap.TypeState
}

func (c *capturePublisher) Publish(_ context.Context, _ jmap.Id, types jmap.TypeState) {
	c.published = append(c.published, types)
}
func (c *capturePublisher) Subscribe(context.Context, []jmap.Id) (notify.Subscription, error) {
	panic("not used")
}
func (c *capturePublisher) SubscribeAll(context.Context) (notify.Subscription, error) {
	panic("not used")
}

// An Internal TYPE (descriptor.Type.Internal) is bookkeeping the way an
// Internal property is: its commits happen, but its name never reaches
// the push surface (RFC 8620 section 7.1 names data types) and never
// lists as a subscribable type name.
func TestInternalTypeOffPushSurface(t *testing.T) {
	be := memory.New()
	db := New(be, lease.NewInProcess(be), WithVerifyPreImages())
	if err := db.RegisterType(markedType()); err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterType(&descriptor.Type{
		Name:       "TestLedger",
		Capability: "https://naust.email/test/marked",
		Internal:   true,
		Properties: map[string]descriptor.Property{
			"n": {Kind: descriptor.KindInt, Internal: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	cap := &capturePublisher{}
	db.SetNotifier(cap)
	ctx := context.Background()

	// A commit touching both types publishes only the visible one.
	_, err := db.Update(ctx, acct, func(u *Update) error {
		if _, err := u.Create("TestMarked", Object{
			"subject": json.RawMessage(`"s"`),
			"marker":  json.RawMessage(`"m"`),
		}); err != nil {
			return err
		}
		_, err := u.Create("TestLedger", Object{"n": json.RawMessage("1")})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cap.published) != 1 {
		t.Fatalf("published %d times, want 1", len(cap.published))
	}
	if _, has := cap.published[0]["TestLedger"]; has {
		t.Errorf("internal type leaked into push: %v", cap.published[0])
	}
	if _, has := cap.published[0]["TestMarked"]; !has {
		t.Errorf("visible type missing from push: %v", cap.published[0])
	}

	// A commit touching ONLY the internal type publishes nothing at all.
	cap.published = nil
	ids, err := db.AllIds(ctx, acct, "TestLedger", 0)
	if err != nil || len(ids) != 1 {
		t.Fatalf("ledger ids = %v, %v", ids, err)
	}
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		return u.Destroy("TestLedger", ids[0])
	}); err != nil {
		t.Fatal(err)
	}
	if len(cap.published) != 0 {
		t.Fatalf("internal-only commit published %v", cap.published)
	}

	// And the name is not subscribable: TypeNames omits it.
	for _, name := range db.TypeNames() {
		if name == "TestLedger" {
			t.Error("TypeNames lists the internal type")
		}
	}
}
