package objectdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
)

func registerCounter(t *testing.T, db *DB) {
	t.Helper()
	err := db.RegisterType(&descriptor.Type{
		Name:       "TestCounter",
		Capability: "https://naust.email/test/notes",
		Properties: map[string]descriptor.Property{
			"total": {Kind: descriptor.KindUnsignedInt},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestChangedSinceParity: over a history exercising every coalescing
// shape across two types, ChangedSince must agree with Changes (RFC
// 8620 section 5.2's coalescing rules are shared) from every possible
// since, while reading the log only once for both types.
func TestChangedSinceParity(t *testing.T) {
	db := newDB(t)
	registerCounter(t, db)
	ctx := context.Background()

	// A scripted history: creates, updates, destroys, destroy+recreate,
	// create+destroy inside one window, and cross-type commits.
	var noteIds, ctrIds []jmap.Id
	step := func(fn func(u *Update) error) {
		t.Helper()
		if _, err := db.Update(ctx, acct, fn); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		step(func(u *Update) error {
			id, err := u.Create("TestNote", note(fmt.Sprintf("n%d", i), ""))
			noteIds = append(noteIds, id)
			return err
		})
	}
	step(func(u *Update) error {
		id, err := u.Create("TestCounter", Object{"total": json.RawMessage(`1`)})
		ctrIds = append(ctrIds, id)
		return err
	})
	step(func(u *Update) error { // update + cross-type commit
		obj, err := u.Get("TestNote", noteIds[0])
		if err != nil {
			return err
		}
		next := make(Object, len(obj))
		for k, v := range obj {
			next[k] = v
		}
		next["body"] = json.RawMessage(`"edited"`)
		if err := u.Put("TestNote", noteIds[0], next); err != nil {
			return err
		}
		ctr, err := u.Get("TestCounter", ctrIds[0])
		if err != nil {
			return err
		}
		nextCtr := make(Object, len(ctr))
		for k, v := range ctr {
			nextCtr[k] = v
		}
		nextCtr["total"] = json.RawMessage(`2`)
		return u.Put("TestCounter", ctrIds[0], nextCtr)
	})
	step(func(u *Update) error { return u.Destroy("TestNote", noteIds[1]) })
	step(func(u *Update) error { // create then destroy across commits
		id, err := u.Create("TestNote", note("ephemeral", ""))
		noteIds = append(noteIds, id)
		return err
	})
	step(func(u *Update) error { return u.Destroy("TestNote", noteIds[3]) })

	global, err := strconv.ParseInt(mustTypeState(t, db, "TestNote"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	for since := int64(0); since <= global; since++ {
		state := strconv.FormatInt(since, 10)
		got, upTo, err := db.ChangedSince(ctx, acct, []string{"TestNote", "TestCounter"}, since, 0, 0)
		if err != nil {
			t.Fatalf("since %d: %v", since, err)
		}
		if upTo != global {
			t.Fatalf("since %d: upTo = %d, want %d", since, upTo, global)
		}
		for _, name := range []string{"TestNote", "TestCounter"} {
			want, err := db.Changes(ctx, acct, name, state, 0)
			if err != nil {
				t.Fatalf("Changes(%s, %d): %v", name, since, err)
			}
			tc := got[name]
			if !idsEqual(tc.Created, want.Created) || !idsEqual(tc.Updated, want.Updated) || !idsEqual(tc.Destroyed, want.Destroyed) {
				t.Errorf("since %d, %s: ChangedSince %+v vs Changes {C:%v U:%v D:%v}",
					since, name, tc, want.Created, want.Updated, want.Destroyed)
			}
		}
	}
}

func mustTypeState(t *testing.T, db *DB, name string) string {
	t.Helper()
	s, err := db.TypeState(context.Background(), acct, name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestChangedSinceRefusals: every refusal is O(1)-decided and reported
// as ErrCannotCalculateChanges; unknown types are the caller's bug.
func TestChangedSinceRefusals(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		create(t, db, note(fmt.Sprintf("n%d", i), ""))
	}

	if _, _, err := db.ChangedSince(ctx, acct, []string{"NoSuch"}, 0, 0, 0); !errors.Is(err, ErrUnknownType) {
		t.Errorf("unknown type: %v", err)
	}
	if _, _, err := db.ChangedSince(ctx, acct, []string{"TestNote"}, -1, 0, 0); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("negative since: %v", err)
	}
	if _, _, err := db.ChangedSince(ctx, acct, []string{"TestNote"}, 99, 0, 0); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("future since: %v", err)
	}
	// 5 commits behind with a cap of 4: refused before any walk.
	if _, _, err := db.ChangedSince(ctx, acct, []string{"TestNote"}, 0, 4, 0); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("beyond maxBehind: %v", err)
	}
	// Exactly at the cap: allowed.
	if _, _, err := db.ChangedSince(ctx, acct, []string{"TestNote"}, 0, 5, 0); err != nil {
		t.Errorf("at maxBehind: %v", err)
	}
	// The id budget: 5 coalesced ids over a cap of 4.
	if _, _, err := db.ChangedSince(ctx, acct, []string{"TestNote"}, 0, 0, 4); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("beyond maxIds: %v", err)
	}
	if _, _, err := db.ChangedSince(ctx, acct, []string{"TestNote"}, 0, 0, 5); err != nil {
		t.Errorf("at maxIds: %v", err)
	}

	// A trimmed log refuses states older than the surviving floor, same
	// as Changes (section 5.2: the complete set of changes or nothing).
	if _, err := db.TrimChanges(ctx, acct, time.Now(), 0, 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ChangedSince(ctx, acct, []string{"TestNote"}, 0, 0, 0); !errors.Is(err, ErrCannotCalculateChanges) {
		t.Errorf("below floor: %v", err)
	}
	// A state at or past the floor still answers.
	if _, _, err := db.ChangedSince(ctx, acct, []string{"TestNote"}, 3, 0, 0); err != nil {
		t.Errorf("at floor: %v", err)
	}
}

// TestChangedSinceEmptyWindow: fully caught up answers empty sets, and
// a type untouched inside the window answers an empty set while the
// other reports - one walk, several verdicts.
func TestChangedSinceEmptyWindow(t *testing.T) {
	db := newDB(t)
	registerCounter(t, db)
	ctx := context.Background()
	_, state := create(t, db, note("only", ""))
	since, _ := strconv.ParseInt(state, 10, 64)

	got, upTo, err := db.ChangedSince(ctx, acct, []string{"TestNote", "TestCounter"}, since, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if upTo != since || got["TestNote"].Touched() || got["TestCounter"].Touched() {
		t.Fatalf("caught up: upTo=%d %+v", upTo, got)
	}

	// A counter-only commit: the note's set stays empty.
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		_, err := u.Create("TestCounter", Object{"total": json.RawMessage(`1`)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, upTo, err = db.ChangedSince(ctx, acct, []string{"TestNote", "TestCounter"}, since, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got["TestNote"].Touched() {
		t.Errorf("untouched type reported: %+v", got["TestNote"])
	}
	if len(got["TestCounter"].Created) != 1 {
		t.Errorf("counter create missing: %+v", got["TestCounter"])
	}
	if upTo != since+1 {
		t.Errorf("upTo = %d, want %d", upTo, since+1)
	}
}
