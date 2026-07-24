package objectdb

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// maxTrimPerCall bounds how many log entries one TrimChanges call deletes,
// so the first trim of a log that has grown for months does not build one
// unbounded batch. TrimChanges returns what it deleted; a caller with a
// large backlog calls again until it returns zero.
const maxTrimPerCall = 4096

// logFloor reads the oldest sequence the change log can still answer from.
// Zero means nothing has been trimmed, so the whole log survives.
func (db *DB) logFloor(ctx context.Context, acct jmap.Id) (int64, error) {
	raw, err := db.be.Get(ctx, logFloorKey(acct))
	if errors.Is(err, backend.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return backend.DecodeInt64(raw)
}

// TrimChanges deletes change log entries an account no longer needs to
// retain and raises its log floor past them, so a later Foo/changes from a
// state below the floor answers cannotCalculateChanges (RFC 8620 section
// 5.2) instead of a partial diff. It returns the number of entries deleted.
//
// Two independent triggers, whichever trims more:
//
//   - retention: an entry committed longer than this ago may go. This is the
//     client-facing guarantee - a client offline for less never resyncs.
//     A retention of zero or less disables the trigger.
//   - maxEntries: at most this many entries are retained whatever their age.
//     Retention alone does not bound disk, because every commit appends an
//     entry and nothing bounds the commit rate. Zero or less disables it.
//
// Entries are deleted oldest-first and always as a contiguous prefix, which
// is what lets one floor describe the whole log. The floor is written in the
// same batch as the deletions, so a crash mid-trim leaves a floor that
// matches exactly what is gone. An entry whose commit timestamp is unknown
// (written before the log carried one) is never aged out, only capped out.
//
// It runs under the account lease, so it cannot race the commit appending
// the newest entry.
func (db *DB) TrimChanges(ctx context.Context, acct jmap.Id, now time.Time, retention time.Duration, maxEntries int) (int, error) {
	if retention <= 0 && maxEntries <= 0 {
		return 0, nil
	}
	l, err := db.leases.Acquire(ctx, acct)
	if err != nil {
		return 0, err
	}
	defer l.Release()

	// Whether the cap can possibly fire, from two point reads. Sequences
	// are dense, so the span between the floor and the current sequence
	// bounds the retained entry count from above (commits that write no
	// entry, a bare state bump, only make the real count smaller). If that
	// bound is within the cap the real count is too, and the cap is out of
	// the picture without reading a single entry.
	//
	// This matters because the common call is on an idle account with
	// nothing to trim, and the alternative is counting the whole log every
	// time just to conclude that.
	floor, err := db.logFloor(ctx, acct)
	if err != nil {
		return 0, err
	}
	var global int64
	if raw, err := db.be.Get(ctx, seqKey(acct)); err == nil {
		if global, err = backend.DecodeInt64(raw); err != nil {
			return 0, err
		}
	} else if !errors.Is(err, backend.ErrNotFound) {
		return 0, err
	}
	oldest := floor
	if oldest < 1 {
		oldest = 1
	}
	if global < oldest {
		return 0, nil // nothing retained
	}
	overCap := maxEntries > 0 && global-oldest+1 > int64(maxEntries)

	start, end := prefixRange(seg(string(acct)), seg("g"))

	// Pass one counts, holding nothing: how many leading entries have aged
	// out, and - only when the cap is in play - how many entries there are.
	// Counting the expired prefix rather than every expired entry keeps
	// deletion contiguous: an entry is only ever dropped when everything
	// before it is dropped too.
	//
	// Once the prefix is known, nothing but the cap still needs the total,
	// so without it the scan stops there. An idle account reads one entry;
	// an account with work to do reads about what it deletes.
	total, expired := 0, 0
	prefixLive := false
	cutoff := now.Add(-retention).UnixMilli()
	var scanErr error
	err = db.be.Scan(ctx, start, end, false, func(_, v []byte) bool {
		total++
		if !prefixLive {
			var entry logEntry
			if scanErr = json.Unmarshal(v, &entry); scanErr != nil {
				return false
			}
			if retention <= 0 || entry.At == 0 || entry.At > cutoff {
				prefixLive = true
			} else if expired++; expired >= maxTrimPerCall && !overCap {
				// Already more than one call can delete, and the cap
				// cannot widen that.
				return false
			}
		}
		return !prefixLive || overCap
	})
	if err == nil {
		err = scanErr
	}
	if err != nil {
		return 0, err
	}

	drop := expired
	// total is only complete when the scan ran to the end for the cap.
	if overCap && total-maxEntries > drop {
		drop = total - maxEntries
	}
	if drop > maxTrimPerCall {
		drop = maxTrimPerCall
	}
	if drop <= 0 {
		return 0, nil
	}

	// Pass two deletes that prefix. The floor lands one past the last entry
	// deleted, which is the oldest sequence still answerable whether or not
	// an entry survives above it.
	batch := &backend.Batch{}
	deleted := 0
	var newFloor int64
	err = db.be.Scan(ctx, start, end, false, func(k, _ []byte) bool {
		if deleted == drop {
			return false
		}
		batch.Delete(k)
		newFloor = seqFromLogKey(k) + 1
		deleted++
		return true
	})
	if err != nil {
		return 0, err
	}
	if deleted == 0 {
		return 0, nil
	}
	batch.Set(logFloorKey(acct), backend.EncodeInt64(newFloor))
	l.Fence(batch)
	if err := db.be.WriteBatch(ctx, batch); err != nil {
		return 0, err
	}
	return deleted, nil
}
