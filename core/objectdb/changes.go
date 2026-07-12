package objectdb

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// ChangeSet is the payload of a Foo/changes response (RFC 8620
// section 5.2).
type ChangeSet struct {
	OldState  string
	NewState  string
	HasMore   bool
	Created   []jmap.Id
	Updated   []jmap.Id
	Destroyed []jmap.Id
	// UpdatedProps is the union of property names that may have changed
	// across the records reported in Updated (per-page when paging).
	// nil means unknown - a consumed entry predates property tracking,
	// or an id landed in Updated via destroy-then-create coalescing, so
	// its whole property set may differ. Non-nil and empty means no
	// property-level updates happened.
	UpdatedProps []string
}

// Changes computes the ids created, updated, and destroyed for a type
// since a client state, honoring the section 5.2 rules: coalescing of
// multiple changes to one record, maxChanges paging via intermediate
// states, and the ordering guarantee that a record is never reported
// created after a page reported it updated or destroyed (chronological
// paging makes this structural). maxChanges <= 0 means unlimited.
func (db *DB) Changes(ctx context.Context, acct jmap.Id, typeName, sinceState string, maxChanges int) (*ChangeSet, error) {
	if db.types[typeName] == nil {
		return nil, ErrUnknownType
	}
	since, err := strconv.ParseInt(sinceState, 10, 64)
	if err != nil || since < 0 {
		return nil, ErrCannotCalculateChanges
	}
	var global int64
	if raw, err := db.be.Get(ctx, seqKey(acct)); err == nil {
		if global, err = backend.DecodeInt64(raw); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, backend.ErrNotFound) {
		return nil, err
	}
	// A state the server never issued (from the future) is uncomputable.
	if since > global {
		return nil, ErrCannotCalculateChanges
	}

	// dispositions per id, in chronological entry order.
	const (
		dispCreated = iota
		dispUpdated
		dispDestroyed
	)
	disp := make(map[jmap.Id]int)
	propsKnown := true
	updatedProps := []string{}
	apply := func(id jmap.Id, next int) {
		prev, seen := disp[id]
		if !seen {
			disp[id] = next
			return
		}
		switch {
		case prev == dispCreated && next == dispUpdated:
			// created then updated -> created (5.2 SHOULD).
		case prev == dispCreated && next == dispDestroyed:
			delete(disp, id) // created then destroyed -> omit (5.2 SHOULD)
		case prev == dispUpdated && next == dispDestroyed:
			disp[id] = dispDestroyed
		case prev == dispDestroyed && next == dispCreated:
			disp[id] = dispUpdated // net effect: it changed
			// The record reappears with a possibly-unrelated property
			// set, which per-update property tracking cannot describe.
			propsKnown = false
		default:
			disp[id] = next
		}
	}

	out := &ChangeSet{OldState: sinceState}
	lastConsumed := since
	start := logKey(acct, since+1)
	_, end := prefixRange(seg(string(acct)), seg("g"))
	var scanErr error
	err = db.be.Scan(ctx, start, end, false, func(k, v []byte) bool {
		var entry logEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			scanErr = err
			return false
		}
		te, touches := entry.Types[typeName]
		seqOfEntry := seqFromLogKey(k)
		if !touches {
			// Entries for other types advance the cursor for free.
			if len(disp) == 0 {
				lastConsumed = seqOfEntry
			}
			return true
		}
		if maxChanges > 0 {
			// Entries are consumed whole so an intermediate state is
			// always a real boundary; check the worst case before
			// consuming.
			projected := len(disp) + len(te.Created) + len(te.Updated) + len(te.Destroyed)
			if projected > maxChanges && len(disp) > 0 {
				out.HasMore = true
				return false
			}
			if projected > maxChanges {
				// A single entry larger than maxChanges cannot yield an
				// intermediate state (5.2).
				scanErr = ErrCannotCalculateChanges
				return false
			}
		}
		for _, id := range te.Created {
			apply(id, dispCreated)
		}
		for _, id := range te.Updated {
			apply(id, dispUpdated)
		}
		for _, id := range te.Destroyed {
			apply(id, dispDestroyed)
		}
		if len(te.Updated) > 0 {
			if te.UpdatedProps == nil {
				propsKnown = false // entry predates property tracking
			} else {
				updatedProps = mergeProps(updatedProps, te.UpdatedProps)
			}
		}
		lastConsumed = seqOfEntry
		return true
	})
	if err != nil {
		return nil, err
	}
	if scanErr != nil {
		return nil, scanErr
	}

	for id, d := range disp {
		switch d {
		case dispCreated:
			out.Created = append(out.Created, id)
		case dispUpdated:
			out.Updated = append(out.Updated, id)
		case dispDestroyed:
			out.Destroyed = append(out.Destroyed, id)
		}
	}
	if propsKnown {
		out.UpdatedProps = updatedProps
	}
	for _, ids := range [][]jmap.Id{out.Created, out.Updated, out.Destroyed} {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}

	if out.HasMore {
		out.NewState = strconv.FormatInt(lastConsumed, 10)
		return out, nil
	}
	// Fully caught up: report the type's own state so it compares equal
	// to the state a Foo/get returns (5.1/5.2 use one state space).
	newState, err := db.TypeState(ctx, acct, typeName)
	if err != nil {
		return nil, err
	}
	out.NewState = newState
	return out, nil
}

// seqFromLogKey recovers the sequence number from a log key: the last
// segment is the escaped 8-byte encoding.
func seqFromLogKey(k []byte) int64 {
	// Strip terminator, unescape the final segment.
	body := k[:len(k)-2]
	var segStart int
	for i := len(body) - 2; i >= 0; i-- {
		if body[i] == 0x00 && body[i+1] == 0x01 {
			segStart = i + 2
			break
		}
	}
	raw := make([]byte, 0, 8)
	for i := segStart; i < len(body); i++ {
		if body[i] == 0x00 && i+1 < len(body) && body[i+1] == 0xFF {
			raw = append(raw, 0x00)
			i++
			continue
		}
		raw = append(raw, body[i])
	}
	n, err := backend.DecodeInt64(raw)
	if err != nil {
		return 0
	}
	return n
}
