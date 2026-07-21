package objectdb

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// TestGetManyChunksAcrossBatchBoundary is specifically for the tuning.
// MaxMultiGetBatch chunking loop: a request spanning more than one chunk
// must still return every record, in the same order as the ids given, with
// not-found ids correctly nil regardless of which chunk they land in.
func TestGetManyChunksAcrossBatchBoundary(t *testing.T) {
	orig := tuning.MaxMultiGetBatch
	tuning.MaxMultiGetBatch = 4 // small, so this test exercises several chunks without creating hundreds of records
	t.Cleanup(func() { tuning.MaxMultiGetBatch = orig })

	db := newDB(t)
	ctx := context.Background()

	const n = 10 // spans 3 chunks at size 4: [0:4) [4:8) [8:10)
	ids := make([]jmap.Id, n)
	for i := 0; i < n; i++ {
		id, _ := create(t, db, note(fmt.Sprintf("subject-%d", i), "body"))
		ids[i] = id
	}
	// Interleave a few not-found ids across chunk boundaries.
	requested := []jmap.Id{
		ids[0], "missing-a", ids[1], ids[2], ids[3],
		"missing-b", ids[4], ids[5], ids[6], ids[7],
		ids[8], ids[9], "missing-c",
	}

	got, err := db.GetMany(ctx, acct, "TestNote", requested)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(requested) {
		t.Fatalf("got %d results, want %d", len(got), len(requested))
	}
	for i, id := range requested {
		wantMissing := id == "missing-a" || id == "missing-b" || id == "missing-c"
		if wantMissing {
			if got[i] != nil {
				t.Errorf("index %d (%q): got %v, want nil (not found)", i, id, got[i])
			}
			continue
		}
		if got[i] == nil {
			t.Errorf("index %d (%q): got nil, want a record", i, id)
			continue
		}
		want, err := db.Get(ctx, acct, "TestNote", id)
		if err != nil {
			t.Fatal(err)
		}
		if string(got[i]["subject"]) != string(want["subject"]) {
			t.Errorf("index %d (%q): subject = %s, want %s", i, id, got[i]["subject"], want["subject"])
		}
	}
}

// TestUpdateGetManyStagedOverlay is specifically for Update.GetMany's
// read-your-own-writes contract (matching Update.Get): a record created,
// modified, or destroyed earlier in the same commit must be visible through
// GetMany exactly as it would through Get, without a batched read seeing
// stale pre-commit state from the backend.
func TestUpdateGetManyStagedOverlay(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	committedId, _ := create(t, db, note("committed", "unchanged"))
	toDestroyId, _ := create(t, db, note("will-be-destroyed", "body"))

	_, err := db.Update(ctx, acct, func(u *Update) error {
		newId, err := u.Create("TestNote", note("staged-new", "fresh in this commit"))
		if err != nil {
			return err
		}
		modified := note("staged-modified", "changed in this commit")
		modified["id"], _ = json.Marshal(committedId)
		if err := u.Put("TestNote", committedId, modified); err != nil {
			return err
		}
		if err := u.Destroy("TestNote", toDestroyId); err != nil {
			return err
		}

		ids := []jmap.Id{committedId, toDestroyId, newId, "never-existed"}
		got, err := u.GetMany("TestNote", ids)
		if err != nil {
			return err
		}
		if len(got) != 2 {
			t.Errorf("got %d entries, want 2 (destroyed and never-existed must be absent): %v", len(got), got)
		}
		if subj := string(got[committedId]["subject"]); subj != `"staged-modified"` {
			t.Errorf("committedId: subject = %s, want the staged modification, not the pre-commit value", subj)
		}
		if subj := string(got[newId]["subject"]); subj != `"staged-new"` {
			t.Errorf("newId: subject = %s, want the staged creation, not a miss", subj)
		}
		if _, ok := got[toDestroyId]; ok {
			t.Errorf("toDestroyId: present in result, want absent (staged as destroyed)")
		}
		if _, ok := got["never-existed"]; ok {
			t.Errorf("never-existed: present in result, want absent")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
