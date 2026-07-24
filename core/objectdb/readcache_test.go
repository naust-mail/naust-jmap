package objectdb

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// getCountBackend wraps a backend and counts Get calls per key. It does
// not implement backend.MultiGetter, so GetMany exercises the sequential
// fallback and every record fetch is visible in gets.
type getCountBackend struct {
	backend.Backend
	gets map[string]int
}

func (c *getCountBackend) Get(ctx context.Context, key []byte) ([]byte, error) {
	c.gets[string(key)]++
	return c.Backend.Get(ctx, key)
}

// modified returns a clone of obj with body replaced, the pattern write
// callers must follow: never mutate a Get result in place.
func modified(obj Object) Object {
	next := make(Object, len(obj))
	for k, v := range obj {
		next[k] = v
	}
	next["body"] = json.RawMessage(`"changed"`)
	return next
}

// A record read in an Update must be fetched from the backend exactly
// once, however many reads and write paths touch it afterwards: the
// write paths reuse the read as their pre-image.
func TestWritePathsReuseCommittedReads(t *testing.T) {
	be := &getCountBackend{Backend: memory.New(), gets: map[string]int{}}
	db := New(be, lease.NewInProcess(be), WithVerifyPreImages())
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	idA, _ := create(t, db, note("a", "body a"))
	idB, _ := create(t, db, note("b", "body b"))
	keyA := string(objKey(acct, "TestNote", idA))
	keyB := string(objKey(acct, "TestNote", idB))

	// Get then Put: one fetch.
	be.gets = map[string]int{}
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		obj, err := u.Get("TestNote", idA)
		if err != nil {
			return err
		}
		return u.Put("TestNote", idA, modified(obj))
	}); err != nil {
		t.Fatal(err)
	}
	if n := be.gets[keyA]; n != 1 {
		t.Fatalf("get+put fetched record %d times, want 1", n)
	}

	// Get then PutInternal then Get again: one fetch.
	be.gets = map[string]int{}
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		obj, err := u.Get("TestNote", idA)
		if err != nil {
			return err
		}
		if err := u.PutInternal("TestNote", idA, obj); err != nil {
			return err
		}
		_, err = u.Get("TestNote", idA)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := be.gets[keyA]; n != 1 {
		t.Fatalf("get+putinternal+get fetched record %d times, want 1", n)
	}

	// GetMany then Put of both: one fetch each.
	be.gets = map[string]int{}
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		objs, err := u.GetMany("TestNote", []jmap.Id{idA, idB})
		if err != nil {
			return err
		}
		for id, obj := range objs {
			if err := u.Put("TestNote", id, modified(obj)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if be.gets[keyA] != 1 || be.gets[keyB] != 1 {
		t.Fatalf("getmany+put fetched records %d/%d times, want 1/1", be.gets[keyA], be.gets[keyB])
	}

	// Get then Destroy: one fetch.
	be.gets = map[string]int{}
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		if _, err := u.Get("TestNote", idB); err != nil {
			return err
		}
		return u.Destroy("TestNote", idB)
	}); err != nil {
		t.Fatal(err)
	}
	if n := be.gets[keyB]; n != 1 {
		t.Fatalf("get+destroy fetched record %d times, want 1", n)
	}
}

// WithVerifyPreImages fails the commit when a caller mutates a shared
// Get result instead of cloning it, naming the record; clean commits
// under the flag are unaffected.
func TestVerifyPreImagesDetectsMutation(t *testing.T) {
	be := memory.New()
	db := New(be, lease.NewInProcess(be), WithVerifyPreImages())
	if err := db.RegisterType(noteType()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, _ := create(t, db, note("s", "b"))

	// Clean pattern: clone before modifying commits fine.
	if _, err := db.Update(ctx, acct, func(u *Update) error {
		obj, err := u.Get("TestNote", id)
		if err != nil {
			return err
		}
		return u.Put("TestNote", id, modified(obj))
	}); err != nil {
		t.Fatalf("clean clone-then-put commit failed: %v", err)
	}

	// The forgot-to-clone bug: mutate the Get result in place and Put it.
	_, err := db.Update(ctx, acct, func(u *Update) error {
		obj, err := u.Get("TestNote", id)
		if err != nil {
			return err
		}
		obj["body"] = json.RawMessage(`"mutated in place"`)
		return u.Put("TestNote", id, obj)
	})
	if err == nil || !strings.Contains(err.Error(), "modified after Get") {
		t.Fatalf("in-place mutation not detected, err = %v", err)
	}

	// Mutating a read that never became a write's pre-image is still a
	// contract violation, caught when the commit writes anything at all.
	id2, _ := create(t, db, note("s2", "b2"))
	_, err = db.Update(ctx, acct, func(u *Update) error {
		obj, err := u.Get("TestNote", id)
		if err != nil {
			return err
		}
		delete(obj, "body")
		obj2, err := u.Get("TestNote", id2)
		if err != nil {
			return err
		}
		return u.Put("TestNote", id2, modified(obj2))
	})
	if err == nil || !strings.Contains(err.Error(), "modified after Get") {
		t.Fatalf("bystander-read mutation not detected, err = %v", err)
	}
}
