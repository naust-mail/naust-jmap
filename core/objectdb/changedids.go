package objectdb

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// TypeChanges is the coalesced outcome of an account's change log for
// one type over a sequence window: each id appears at most once, under
// its net effect (the RFC 8620 section 5.2 coalescing rules, so a
// record created and destroyed inside the window leaves no trace, and
// one destroyed and recreated reports as updated).
type TypeChanges struct {
	Created   []jmap.Id
	Updated   []jmap.Id
	Destroyed []jmap.Id
}

// Touched reports whether anything changed.
func (tc *TypeChanges) Touched() bool {
	return tc != nil && (len(tc.Created) > 0 || len(tc.Updated) > 0 || len(tc.Destroyed) > 0)
}

// ChangedSince reads the account's change log once and returns, for
// each requested type, the coalesced ids changed after sequence number
// since, together with the sequence number the answer is current to.
// It is the read behind Foo/queryChanges (RFC 8620 section 5.6), which
// diffs a query result from the records that changed - possibly across
// several types at once - and unlike Changes (section 5.2) it never
// pages: 5.6 has no intermediate states, so an answer that cannot be
// afforded whole is refused as ErrCannotCalculateChanges, which the
// caller maps to the section 5.6 cannotCalculateChanges error telling
// the client to re-run the query. Refusal is always safe there - the
// re-run costs the client roughly what a full answer would have.
//
// Refusals, all decided in O(1) before the log is read:
//   - since is negative or ahead of the account (a state this server
//     never issued);
//   - since precedes the trimmed portion of the log (the entries that
//     would describe the difference are gone; re-checked after the
//     walk, since a trim may commit concurrently - the reader holds no
//     lease);
//   - the account is more than maxBehind commits past since (the walk
//     itself would be unbounded work; 0 means unlimited).
//
// maxIds bounds the memory the answer may hold: if the coalesced ids
// across all requested types exceed it, the walk stops and refuses
// (0 means unlimited).
//
// The answer covers exactly the window (since, upTo]: entries a
// concurrent writer commits past the sequence read at entry are left
// for the client's next call, so upTo is always a sequence whose
// changes are fully reported, never one mid-seen.
func (db *DB) ChangedSince(ctx context.Context, acct jmap.Id, typeNames []string, since int64, maxBehind, maxIds int) (map[string]*TypeChanges, int64, error) {
	for _, name := range typeNames {
		if db.types[name] == nil {
			return nil, 0, ErrUnknownType
		}
	}
	if since < 0 {
		return nil, 0, ErrCannotCalculateChanges
	}
	var upTo int64
	if raw, err := db.be.Get(ctx, seqKey(acct)); err == nil {
		if upTo, err = backend.DecodeInt64(raw); err != nil {
			return nil, 0, err
		}
	} else if !errors.Is(err, backend.ErrNotFound) {
		return nil, 0, err
	}
	if since > upTo {
		return nil, 0, ErrCannotCalculateChanges
	}
	if maxBehind > 0 && upTo-since > int64(maxBehind) {
		return nil, 0, ErrCannotCalculateChanges
	}
	floor, err := db.logFloor(ctx, acct)
	if err != nil {
		return nil, 0, err
	}
	if since+1 < floor {
		return nil, 0, ErrCannotCalculateChanges
	}

	// Net disposition per (type, id), in chronological entry order - the
	// same section 5.2 coalescing Changes applies, minus its paging.
	const (
		dispCreated = iota
		dispUpdated
		dispDestroyed
		dispOmitted // created then destroyed inside the window
	)
	type disposition struct {
		byId map[jmap.Id]int
	}
	disp := make(map[string]*disposition, len(typeNames))
	for _, name := range typeNames {
		disp[name] = &disposition{byId: make(map[jmap.Id]int)}
	}
	held := 0
	apply := func(d *disposition, id jmap.Id, next int) bool {
		prev, seen := d.byId[id]
		if !seen {
			d.byId[id] = next
			held++
			return maxIds <= 0 || held <= maxIds
		}
		switch {
		case prev == dispOmitted:
			// Reappearing after create+destroy: a fresh creation.
			d.byId[id] = dispCreated
		case prev == dispCreated && next == dispDestroyed:
			d.byId[id] = dispOmitted
		case prev == dispCreated: // created then updated stays created
		case prev == dispDestroyed && next == dispCreated:
			d.byId[id] = dispUpdated // net effect: it changed
		default:
			d.byId[id] = next
		}
		return true
	}

	start := logKey(acct, since+1)
	_, end := prefixRange(seg(string(acct)), seg("g"))
	budgetHit := false
	var scanErr error
	err = db.be.Scan(ctx, start, end, false, func(k, v []byte) bool {
		if seqFromLogKey(k) > upTo {
			return false // committed after this read began; not covered
		}
		var entry logEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			scanErr = err
			return false
		}
		for name, d := range disp {
			te, touches := entry.Types[name]
			if !touches {
				continue
			}
			for _, id := range te.Created {
				if !apply(d, id, dispCreated) {
					budgetHit = true
					return false
				}
			}
			for _, id := range te.Updated {
				if !apply(d, id, dispUpdated) {
					budgetHit = true
					return false
				}
			}
			for _, id := range te.Destroyed {
				if !apply(d, id, dispDestroyed) {
					budgetHit = true
					return false
				}
			}
		}
		return true
	})
	if err != nil {
		return nil, 0, err
	}
	if scanErr != nil {
		return nil, 0, scanErr
	}
	if budgetHit {
		return nil, 0, ErrCannotCalculateChanges
	}

	// A trim may have deleted entries this walk needed after the floor
	// check above; the floor moves in the same batch as the deletions,
	// so a floor still at or below since+1 proves the walk was complete.
	floor, err = db.logFloor(ctx, acct)
	if err != nil {
		return nil, 0, err
	}
	if since+1 < floor {
		return nil, 0, ErrCannotCalculateChanges
	}

	out := make(map[string]*TypeChanges, len(typeNames))
	for name, d := range disp {
		tc := &TypeChanges{}
		for id, dp := range d.byId {
			switch dp {
			case dispCreated:
				tc.Created = append(tc.Created, id)
			case dispUpdated:
				tc.Updated = append(tc.Updated, id)
			case dispDestroyed:
				tc.Destroyed = append(tc.Destroyed, id)
			}
		}
		for _, ids := range [][]jmap.Id{tc.Created, tc.Updated, tc.Destroyed} {
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		}
		out[name] = tc
	}
	return out, upTo, nil
}
