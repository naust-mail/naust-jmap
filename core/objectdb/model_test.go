package objectdb

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"testing"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// Property-based store-vs-model test: random op sequences run against
// the real DB and a naive model that snapshots the full id->version map
// after every commit. The model computes what RFC 8620 section 5.2 says
// a /changes answer must convey - existence and change between two
// states - with none of the store's machinery (no log, no coalescing
// code), so agreement means the log+coalescing implementation derives
// the right answer, including across maxChanges page boundaries.

// model mirrors one account+type. snapshots[s] is the id->version map
// after sequence s committed (versions bump on update); snapshots[0] is
// empty. Cancelled commits still consume a sequence, so a snapshot is
// appended per commit whether or not it had net effect.
type model struct {
	snapshots []map[jmap.Id]int
	effective int64 // seq of the last commit with net effect (= TypeState)
}

func newModel() *model {
	return &model{snapshots: []map[jmap.Id]int{{}}}
}

func (m *model) current() map[jmap.Id]int {
	return m.snapshots[len(m.snapshots)-1]
}

func (m *model) commit(next map[jmap.Id]int, hadEffect bool) {
	m.snapshots = append(m.snapshots, next)
	if hadEffect {
		m.effective = int64(len(m.snapshots) - 1)
	}
}

// diff computes the section 5.2 answer between state since and now:
// created if absent then and present now, destroyed if the reverse,
// updated if present in both with a different version.
func (m *model) diff(since int64) (created, updated, destroyed []jmap.Id) {
	old := m.snapshots[since]
	now := m.current()
	for id, v := range now {
		ov, existed := old[id]
		switch {
		case !existed:
			created = append(created, id)
		case ov != v:
			updated = append(updated, id)
		}
	}
	for id := range old {
		if _, exists := now[id]; !exists {
			destroyed = append(destroyed, id)
		}
	}
	sortIds(created)
	sortIds(updated)
	sortIds(destroyed)
	return
}

func sortIds(ids []jmap.Id) {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
}

func copySnapshot(s map[jmap.Id]int) map[jmap.Id]int {
	out := make(map[jmap.Id]int, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

func TestStoreMatchesModel(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 42, 1000} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			runStoreVsModel(t, seed, 120)
		})
	}
}

func runStoreVsModel(t *testing.T, seed int64, commits int) {
	ctx := context.Background()
	be := memory.New()
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(&descriptor.Type{
		Name:       "TestNote",
		Capability: "urn:example:notes",
		Properties: map[string]descriptor.Property{
			"subject": {Kind: descriptor.KindString, Indexed: true},
			"body":    {Kind: descriptor.KindString},
		},
	}); err != nil {
		t.Fatal(err)
	}
	acct := jmap.Id("acct1")
	rng := rand.New(rand.NewSource(seed))
	m := newModel()

	for c := 0; c < commits; c++ {
		next := copySnapshot(m.current())
		_, err := db.Update(ctx, acct, func(u *Update) error {
			ops := 1 + rng.Intn(3)
			for i := 0; i < ops; i++ {
				switch pickOp(rng, next) {
				case "create":
					id, err := u.Create("TestNote", Object{
						"subject": jsonStr(fmt.Sprintf("s%d", rng.Intn(10))),
						"body":    jsonStr("b"),
					})
					if err != nil {
						return err
					}
					next[id] = 0
					// Sometimes destroy it in the same commit to exercise
					// the leaves-no-trace rule.
					if rng.Intn(6) == 0 {
						if err := u.Destroy("TestNote", id); err != nil {
							return err
						}
						delete(next, id)
					}
				case "update":
					id := pickId(rng, next)
					if err := u.Put("TestNote", id, Object{
						"id":      jsonStr(string(id)),
						"subject": jsonStr(fmt.Sprintf("s%d", rng.Intn(10))),
						"body":    jsonStr("b2"),
					}); err != nil {
						return err
					}
					next[id]++
				case "destroy":
					id := pickId(rng, next)
					if err := u.Destroy("TestNote", id); err != nil {
						return err
					}
					delete(next, id)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("commit %d: %v", c, err)
		}
		hadEffect := !snapshotsEqual(m.current(), next)
		m.commit(next, hadEffect)

		// Invariant: state string always equals the model's last
		// effective sequence.
		state, err := db.TypeState(ctx, acct, "TestNote")
		if err != nil {
			t.Fatal(err)
		}
		if want := strconv.FormatInt(m.effective, 10); state != want {
			t.Fatalf("commit %d: TypeState = %s, model says %s", c, state, want)
		}

		// Invariant: AllIds equals the model's current id set.
		ids, err := db.AllIds(ctx, acct, "TestNote", 0)
		if err != nil {
			t.Fatal(err)
		}
		wantIds := make([]jmap.Id, 0, len(next))
		for id := range next {
			wantIds = append(wantIds, id)
		}
		sortIds(wantIds)
		if !idsEqual(ids, wantIds) {
			t.Fatalf("commit %d: AllIds = %v, model says %v", c, ids, wantIds)
		}
	}

	// /changes from every historical state, unpaged, must match the
	// model's naive diff.
	for since := int64(0); since < int64(len(m.snapshots)); since++ {
		cs, err := db.Changes(ctx, acct, "TestNote", strconv.FormatInt(since, 10), 0)
		if err != nil {
			t.Fatalf("since %d: %v", since, err)
		}
		if cs.HasMore {
			t.Fatalf("since %d: unpaged Changes reported HasMore", since)
		}
		wc, wu, wd := m.diff(since)
		if !idsEqual(cs.Created, wc) || !idsEqual(cs.Updated, wu) || !idsEqual(cs.Destroyed, wd) {
			t.Fatalf("since %d: store (c=%v u=%v d=%v) model (c=%v u=%v d=%v)",
				since, cs.Created, cs.Updated, cs.Destroyed, wc, wu, wd)
		}
		if want := strconv.FormatInt(m.effective, 10); cs.NewState != want {
			t.Fatalf("since %d: NewState = %s, want %s", since, cs.NewState, want)
		}
	}

	// Paged traversal: follow HasMore with a small maxChanges, applying
	// each page to a client-side disposition map with the section 5.2
	// coalescing rules. The end result must equal the naive diff, and no
	// page may report an id created after an earlier page reported it
	// updated or destroyed.
	for _, since := range []int64{0, int64(len(m.snapshots)) / 2} {
		client := map[jmap.Id]string{}
		state := strconv.FormatInt(since, 10)
		for pages := 0; ; pages++ {
			if pages > len(m.snapshots)+1 {
				t.Fatalf("since %d: paging did not terminate", since)
			}
			cs, err := db.Changes(ctx, acct, "TestNote", state, 5)
			if err != nil {
				t.Fatalf("since %d page %d: %v", since, pages, err)
			}
			for _, id := range cs.Created {
				if d, seen := client[id]; seen && d != "created" {
					t.Fatalf("since %d: id %s created after being %s", since, id, d)
				}
				client[id] = "created"
			}
			for _, id := range cs.Updated {
				if client[id] != "created" {
					client[id] = "updated"
				}
			}
			for _, id := range cs.Destroyed {
				if client[id] == "created" {
					delete(client, id) // created then destroyed: net nothing
				} else {
					client[id] = "destroyed"
				}
			}
			state = cs.NewState
			if !cs.HasMore {
				break
			}
		}
		var gotC, gotU, gotD []jmap.Id
		for id, d := range client {
			switch d {
			case "created":
				gotC = append(gotC, id)
			case "updated":
				gotU = append(gotU, id)
			case "destroyed":
				gotD = append(gotD, id)
			}
		}
		sortIds(gotC)
		sortIds(gotU)
		sortIds(gotD)
		wc, wu, wd := m.diff(since)
		if !idsEqual(gotC, wc) || !idsEqual(gotU, wu) || !idsEqual(gotD, wd) {
			t.Fatalf("paged since %d: store (c=%v u=%v d=%v) model (c=%v u=%v d=%v)",
				since, gotC, gotU, gotD, wc, wu, wd)
		}
	}
}

func pickOp(rng *rand.Rand, live map[jmap.Id]int) string {
	if len(live) == 0 {
		return "create"
	}
	switch rng.Intn(3) {
	case 0:
		return "create"
	case 1:
		return "update"
	default:
		return "destroy"
	}
}

func pickId(rng *rand.Rand, live map[jmap.Id]int) jmap.Id {
	ids := make([]jmap.Id, 0, len(live))
	for id := range live {
		ids = append(ids, id)
	}
	sortIds(ids)
	return ids[rng.Intn(len(ids))]
}

func jsonStr(s string) []byte { return []byte(strconv.Quote(s)) }

func snapshotsEqual(a, b map[jmap.Id]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func idsEqual(a, b []jmap.Id) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
