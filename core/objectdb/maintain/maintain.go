// Package maintain schedules objectdb's storage reclamation: trimming
// change logs (RFC 8620 section 5.2 permits answering
// cannotCalculateChanges once old entries are gone) and collecting
// unreferenced blobs (section 6 leaves the collection schedule to the
// server). Neither the runtime nor the database runs these on its own;
// a host that does not schedule them retains change log entries and
// unreferenced blobs indefinitely.
//
// Run is the whole opt-in: it loops over every account applying the
// passes objectdb already exposes (TrimChanges, SweepBlobs) with the
// tuning defaults. A host that wants its own schedule, account policy,
// or knobs calls those passes directly instead - this package composes
// public API and holds no privileged access.
package maintain

import (
	"context"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// defaultInterval is how often Run repeats when Config.Interval is zero.
// Reclamation is never urgent - what accumulates between passes is bounded
// by the windows the passes enforce, not by how promptly they run - and the
// blob sweep examines every upload record an account has, so running it much
// more often than tuning.BlobMinUnreferencedAge spends scans to reclaim
// nothing.
const defaultInterval = time.Hour

// now is the clock reclamation windows are measured against; a test seam,
// time.Now outside tests.
var now = time.Now

// Config tunes Run and RunOnce. The zero value is usable: it trims change
// logs on the default schedule with the tuning defaults and skips blob
// collection.
type Config struct {
	// Blobs is the store SweepBlobs collects into. Nil skips blob
	// collection entirely, for hosts that sweep on their own schedule or
	// have no blob store wired.
	Blobs blob.Store
	// Interval is how often Run repeats. Zero or negative means the
	// default (one hour).
	Interval time.Duration
	// OnError observes failures: acct names the account whose pass
	// failed, or is empty when listing the accounts themselves failed.
	// A failure never stops the other accounts, and the next pass
	// retries, so a nil OnError only forgoes visibility, not progress.
	OnError func(acct jmap.Id, err error)
	// Extra runs after the built-in passes for each account, on the same
	// schedule and account loop. It is how an embedder attaches passes
	// this package cannot know about (a datatype module's own correction
	// or reclamation work) without duplicating the scheduling. The
	// callback owns its error handling - report through OnError to keep
	// one channel. Nil is skipped.
	Extra func(ctx context.Context, acct jmap.Id)
}

// Run reclaims storage for every account of db, immediately and then on
// every interval, until ctx ends. It only returns on ctx cancellation, so
// the usual call is `go maintain.Run(...)` - or wrapping it in
// lease.RunSingleton so one elected instance carries the work for a fleet
// sharing a store.
func Run(ctx context.Context, db *objectdb.DB, cfg Config) {
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		RunOnce(ctx, db, cfg)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce reclaims storage for every account of db once, for hosts that
// bring their own scheduler (a cron job, a systemd timer). Failures are
// reported through cfg.OnError and stepped over: one unreadable account
// must not stop the rest, and the next pass retries.
func RunOnce(ctx context.Context, db *objectdb.DB, cfg Config) {
	accts, err := db.Accounts(ctx)
	if err != nil {
		cfg.report("", err)
		return
	}
	for _, acct := range accts {
		if ctx.Err() != nil {
			return
		}
		if cfg.Blobs != nil {
			// SweepBlobs bounds how many candidates one call inspects, so
			// a backlog (a mass destroy since the last pass) needs several
			// calls to drain. more is the drain signal: it stays true only
			// while a call both hit its bound and made progress, so this
			// loop always terminates.
			for {
				_, more, err := db.SweepBlobs(ctx, acct, cfg.Blobs, now(), tuning.BlobMinUnreferencedAge)
				if err != nil {
					cfg.report(acct, err)
					break
				}
				if !more {
					break
				}
			}
		}
		// TrimChanges bounds how much one call deletes, so a log with a
		// backlog (the first pass ever, or after long downtime) needs
		// several calls to drain.
		for {
			n, err := db.TrimChanges(ctx, acct, now(), tuning.ChangeLogRetention, tuning.ChangeLogMaxEntries)
			if err != nil {
				cfg.report(acct, err)
				break
			}
			if n == 0 {
				break
			}
		}
		if cfg.Extra != nil {
			cfg.Extra(ctx, acct)
		}
	}
}

func (c Config) report(acct jmap.Id, err error) {
	if c.OnError != nil {
		c.OnError(acct, err)
	}
}
