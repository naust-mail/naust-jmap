package emailstore

// Correcting the stored Mailbox counters when the trash role moves. The
// section 2.1 trash rules make unreadThreads depend on which Mailbox
// holds the trash role, and the counters are maintained by delta, so a
// role move leaves every Thread's folded contribution computed under
// the old rules (see threadStat.Trash). Each Thread corrects itself the
// next time a change touches it; this file covers the Threads nothing
// touches. Because role is a client-settable property, the role-moving
// commit itself must never do work proportional to the account (that
// would let a small request buy an unbounded amount of server work, on
// demand and repeatedly): it only records that the rules changed, and
// MigrateThreadCounters walks the Threads in bounded, separately
// committed slices on the embedder's maintenance schedule.
//
// The bounded window in which a counter can be stale is within the
// spec's own latitude: section 2.1 defines unreadThreads as "an
// indication of the number of 'unread' Threads" and states the way it
// is determined "is not mandated in this document". Once converged, the
// value matches the section's "quality implementation" formula exactly.
//
// The CounterRules marker type is registered with the store only (never
// with the runtime, so it has no protocol surface at all); the root
// package builds its descriptor (CapabilityURI is root-owned) and
// registers it as part of RegisterMailbox, and MigrateThreadCounters is
// re-exported from root unchanged, wrapping the engine function below.

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// TypeCounterRules names the internal singleton record marking that the
// trash rules changed and some Threads may still be counted under the
// old ones.
const TypeCounterRules = "CounterRules"

// counterRulesKey is the marker's fixed lookup value: record ids are
// server-assigned, so the singleton is found through this indexed
// property rather than a well-known id.
const counterRulesKey = "trash-rules"

// counterRulesMarker returns the marker record's id, or "" when none
// exists (the counters are known current).
func counterRulesMarker(u *objectdb.Update) (jmap.Id, error) {
	ids, err := u.IdsWhereEqual(TypeCounterRules, "k", json.RawMessage(`"`+counterRulesKey+`"`))
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

// NoteTrashRulesChange records, in the same commit as the role change
// that triggered it, that Threads may now be counted under outdated
// trash rules. The marker's generation moves on every change, so a
// migration pass that raced a further role change knows not to declare
// the account clean.
func NoteTrashRulesChange(u *objectdb.Update) error {
	id, err := counterRulesMarker(u)
	if err != nil {
		return err
	}
	if id == "" {
		_, err := u.Create(TypeCounterRules, objectdb.Object{
			"k":   record.MustJSON(counterRulesKey),
			"gen": record.MustJSON(int64(1)),
		})
		return err
	}
	obj, err := u.Get(TypeCounterRules, id)
	if err != nil {
		return err
	}
	var gen int64
	json.Unmarshal(obj["gen"], &gen)
	next := cloneObject(obj)
	next["gen"] = record.MustJSON(gen + 1)
	return u.PutInternal(TypeCounterRules, id, next)
}

// migrateExamineBatch bounds how many Thread records one migration
// commit reads, which bounds how long the account's writers wait behind
// each slice of the walk.
const migrateExamineBatch = 512

// MigrateThreadCounters corrects, for one account, the stored Mailbox
// counters of Threads still counted under outdated trash rules. It does
// nothing (one index lookup) when no rules change is pending. max
// bounds how many Threads one call corrects (<= 0 means no bound); done
// reports that the account's counters are fully current, so a caller
// drains with `for !done`. Each slice of the walk is its own commit, so
// the account's own traffic interleaves freely - a Thread the traffic
// touches first simply corrects itself and the walk finds it current.
//
// A call reads every Thread record of the account (in bounded slices)
// to find the stale ones; that cost is paid only while a rules change
// is pending, never on the steady state.
func MigrateThreadCounters(ctx context.Context, db *objectdb.DB, acct jmap.Id, max int) (done bool, err error) {
	// The marker probe takes no lease: a marker created concurrently is
	// found by the next call, the same outcome as running a moment
	// earlier, and the generation is re-read under the lease before the
	// marker is ever retired.
	markers, err := db.IdsWhereEqual(ctx, acct, TypeCounterRules, "k", json.RawMessage(`"`+counterRulesKey+`"`), 0)
	if err != nil {
		return false, err
	}
	if len(markers) == 0 {
		return true, nil
	}
	marker := markers[0]
	obj, err := db.Get(ctx, acct, TypeCounterRules, marker)
	if errors.Is(err, objectdb.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var startGen int64
	json.Unmarshal(obj["gen"], &startGen)

	ids, err := db.AllIds(ctx, acct, record.TypeThread, 0)
	if err != nil {
		return false, err
	}
	corrected := 0
	for start := 0; start < len(ids); start += migrateExamineBatch {
		end := min(start+migrateExamineBatch, len(ids))
		if _, err := db.Update(ctx, acct, func(u *objectdb.Update) error {
			trash, err := record.RoleMailboxId(u, "trash")
			if err != nil {
				return err
			}
			deltas := map[jmap.Id]*ctrDelta{}
			for _, tid := range ids[start:end] {
				if max > 0 && corrected >= max {
					break
				}
				thr, err := u.Get(record.TypeThread, tid)
				if errors.Is(err, objectdb.ErrNotFound) {
					continue // destroyed since the id listing
				}
				if err != nil {
					return err
				}
				st := statOf(thr)
				if st.Trash == trash {
					continue
				}
				for mb := range st.Mailboxes {
					d := deltas[mb]
					if d == nil {
						d = &ctrDelta{}
						deltas[mb] = d
					}
					// totalThreads is trash-independent; only the
					// unread flag can move under a rules change.
					d.unreadThreads += B2i(st.unreadIn(mb, trash)) - B2i(st.unreadIn(mb, st.Trash))
				}
				st.Trash = trash
				next := cloneObject(thr)
				next[StatProperty] = record.MustJSON(st)
				if err := u.PutInternal(record.TypeThread, tid, next); err != nil {
					return err
				}
				corrected++
			}
			return applyDeltas(u, deltas)
		}); err != nil {
			return false, err
		}
		if max > 0 && corrected >= max {
			return false, nil
		}
	}

	// The walk saw every Thread current (or made it so). Retire the
	// marker only if no further rules change landed while it ran -
	// otherwise Threads examined early may be stale again, and the
	// surviving marker sends the next call back over them.
	_, err = db.Update(ctx, acct, func(u *objectdb.Update) error {
		obj, err := u.Get(TypeCounterRules, marker)
		if errors.Is(err, objectdb.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var gen int64
		json.Unmarshal(obj["gen"], &gen)
		if gen != startGen {
			done = false
			return nil
		}
		done = true
		return u.Destroy(TypeCounterRules, marker)
	})
	return done, err
}
