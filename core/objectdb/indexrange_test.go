package objectdb

// Tests for the ordered index range read (IdsWhereAtMost) and the
// account tags (Accounts, TaggedAccounts, Update.Set/ClearAccountTag):
// the primitives backing queue-shaped consumers such as the mail
// module's submission sending worker.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// dueType has an indexed Date, the queue shape.
func dueType() *descriptor.Type {
	return &descriptor.Type{
		Name:       "TestDue",
		Capability: "https://naust.email/test/due",
		Properties: map[string]descriptor.Property{
			"at":   {Kind: descriptor.KindDate, Indexed: true},
			"note": {Kind: descriptor.KindString},
		},
	}
}

func rawStr(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func TestIdsWhereAtMost(t *testing.T) {
	be := memory.New()
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(dueType()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	times := []string{
		"2026-07-17T12:00:00Z",
		"2026-07-17T10:00:00Z",
		"2026-07-17T11:00:00Z",
		"2026-07-17T13:00:00Z",
	}
	ids := make(map[string]jmap.Id)
	_, err := db.Update(ctx, acct, func(u *Update) error {
		for _, at := range times {
			id, err := u.Create("TestDue", Object{"at": rawStr(at)})
			if err != nil {
				return err
			}
			ids[at] = id
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// No bound, limit 1: the earliest.
	got, err := db.IdsWhereAtMost(ctx, acct, "TestDue", "at", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != ids["2026-07-17T10:00:00Z"] {
		t.Fatalf("earliest = %v, want %v", got, ids["2026-07-17T10:00:00Z"])
	}

	// Bounded at 11:00 inclusive: the two entries at or before it, in
	// ascending value order.
	got, err = db.IdsWhereAtMost(ctx, acct, "TestDue", "at", rawStr("2026-07-17T11:00:00Z"), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []jmap.Id{ids["2026-07-17T10:00:00Z"], ids["2026-07-17T11:00:00Z"]}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("bounded = %v, want %v", got, want)
	}

	// A bound before everything matches nothing.
	got, err = db.IdsWhereAtMost(ctx, acct, "TestDue", "at", rawStr("2026-07-17T09:00:00Z"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("early bound = %v, want empty", got)
	}

	// Limit truncates.
	got, err = db.IdsWhereAtMost(ctx, acct, "TestDue", "at", nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("limit 3 returned %d ids", len(got))
	}

	// Removing the indexed property removes the record from the range:
	// the queue-exit contract.
	_, err = db.Update(ctx, acct, func(u *Update) error {
		obj, err := u.Get("TestDue", ids["2026-07-17T10:00:00Z"])
		if err != nil {
			return err
		}
		delete(obj, "at")
		return u.Put("TestDue", ids["2026-07-17T10:00:00Z"], obj)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = db.IdsWhereAtMost(ctx, acct, "TestDue", "at", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != ids["2026-07-17T11:00:00Z"] {
		t.Fatalf("after removal earliest = %v, want %v", got, ids["2026-07-17T11:00:00Z"])
	}

	// An unindexed property is refused.
	if _, err := db.IdsWhereAtMost(ctx, acct, "TestDue", "note", nil, 0); err == nil {
		t.Fatal("expected an error for an unindexed property")
	}
}

func TestAccounts(t *testing.T) {
	be := memory.New()
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(dueType()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got, err := db.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty backend lists accounts %v", got)
	}

	for _, a := range []jmap.Id{"Acharlie", "Aalpha", "Abravo"} {
		_, err := db.Update(ctx, a, func(u *Update) error {
			_, err := u.Create("TestDue", Object{"at": rawStr("2026-07-17T10:00:00Z")})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err = db.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []jmap.Id{"Aalpha", "Abravo", "Acharlie"}
	if len(got) != len(want) {
		t.Fatalf("accounts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("accounts = %v, want %v", got, want)
		}
	}
}

func TestAccountTags(t *testing.T) {
	be := memory.New()
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(dueType()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const tag = "test:worklist"

	// A tag set alongside record data lands in the same commit.
	_, err := db.Update(ctx, "Aone", func(u *Update) error {
		if _, err := u.Create("TestDue", Object{"at": rawStr("2026-07-17T10:00:00Z")}); err != nil {
			return err
		}
		return u.SetAccountTag(tag)
	})
	if err != nil {
		t.Fatal(err)
	}
	// A tag-only commit works too: the worklist consumer clears without
	// staging records.
	_, err = db.Update(ctx, "Atwo", func(u *Update) error {
		return u.SetAccountTag(tag)
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.TaggedAccounts(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "Aone" || got[1] != "Atwo" {
		t.Fatalf("tagged = %v, want [Aone Atwo]", got)
	}

	// Clearing removes only the cleared member.
	_, err = db.Update(ctx, "Atwo", func(u *Update) error {
		return u.ClearAccountTag(tag)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = db.TaggedAccounts(ctx, tag)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "Aone" {
		t.Fatalf("after clear tagged = %v, want [Aone]", got)
	}

	// Tags are namespaced: another tag's set is untouched, and the
	// built-in registry still lists both accounts.
	accts, err := db.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 2 {
		t.Fatalf("registry = %v, want both accounts", accts)
	}

	// Guard rails: empty names, and the registry tag cannot be cleared.
	_, err = db.Update(ctx, "Aone", func(u *Update) error {
		if err := u.SetAccountTag(""); err == nil {
			t.Error("empty tag name accepted by SetAccountTag")
		}
		if err := u.ClearAccountTag(""); err == nil {
			t.Error("empty tag name accepted by ClearAccountTag")
		}
		if err := u.ClearAccountTag("exists"); err == nil {
			t.Error("registry tag clear accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.TaggedAccounts(ctx, ""); err == nil {
		t.Error("empty tag name accepted by TaggedAccounts")
	}
}
