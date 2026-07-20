package mail

import (
	"strings"
	"sync/atomic"
	"time"
)

// futureStampTolerance is the line past which a peer's claim stamp reads
// as future-dated: healthy NTP keeps machines within milliseconds, so a
// stamp a full second past this worker's clock means genuinely broken
// timekeeping somewhere - three orders of magnitude beyond healthy
// scatter, three below the ClaimWindow scale where harm (systematic
// duplicate relays) begins. A detection threshold only; correctness
// never depends on it.
const futureStampTolerance = time.Second

// workerStats holds the worker's counters. All increments happen outside
// store transactions (a lease retry must not double-count) and outside
// locks; an idle worker increments nothing but sweeps.
type workerStats struct {
	sweeps                   atomic.Uint64
	zeroProgressSweeps       atomic.Uint64
	claimsTaken              atomic.Uint64
	claimsReclaimed          atomic.Uint64
	claimsLostBeforeTransmit atomic.Uint64
	finalizesSuperseded      atomic.Uint64
	futureStamps             atomic.Uint64
}

// SubmissionWorkerStats is a point-in-time snapshot of one worker's
// counters, read via Stats. Counters are per worker and in-process -
// each node reports its own view, which is what makes the skew signal
// (FutureStamps) attributable to a machine; aggregation across workers
// is the host's metric pipeline's job. All values are monotonic for the
// life of the worker.
type SubmissionWorkerStats struct {
	// Sweeps counts queue passes (timer, bell, and Notifier wakes
	// alike). The denominator for ZeroProgressSweeps, and the idle wake
	// rate when the queue is empty.
	Sweeps uint64
	// ZeroProgressSweeps counts sweeps that saw at least one due record
	// but claimed none. Occasionally nonzero is healthy claim racing
	// between workers; nearly every sweep means a stuck queue.
	ZeroProgressSweeps uint64
	// ClaimsTaken counts records this worker claimed for an attempt.
	ClaimsTaken uint64
	// ClaimsReclaimed counts claims taken over from a leftover expired
	// stamp - a worker (possibly this one, before a restart) died or
	// stalled mid-send and its claim aged out.
	ClaimsReclaimed uint64
	// ClaimsLostBeforeTransmit counts claims a peer took over before
	// this worker transmitted: the pre-transmit re-read caught it, so
	// no duplicate was sent. Measures claim contention.
	ClaimsLostBeforeTransmit uint64
	// FinalizesSuperseded counts claims a peer took over after this
	// worker transmitted: the message went out and the peer may relay
	// it again, so a duplicate is possible. Zero on a healthy
	// deployment; treat any growth as a real event.
	FinalizesSuperseded uint64
	// FutureStamps counts peer claim stamps dated more than a second
	// past this worker's own clock. A worker seeing these repeatedly
	// should suspect its own clock of lagging - the lagging node is
	// both the dangerous one and the one that observes future stamps.
	// Meaningful in aggregate; zero on a healthy fleet.
	FutureStamps uint64
}

// Stats returns a snapshot of this worker's counters. Cheap to call
// (seven atomic loads); poll it into logs or a metrics endpoint.
func (w *SubmissionWorker) Stats() SubmissionWorkerStats {
	return SubmissionWorkerStats{
		Sweeps:                   w.stats.sweeps.Load(),
		ZeroProgressSweeps:       w.stats.zeroProgressSweeps.Load(),
		ClaimsTaken:              w.stats.claimsTaken.Load(),
		ClaimsReclaimed:          w.stats.claimsReclaimed.Load(),
		ClaimsLostBeforeTransmit: w.stats.claimsLostBeforeTransmit.Load(),
		FinalizesSuperseded:      w.stats.finalizesSuperseded.Load(),
		FutureStamps:             w.stats.futureStamps.Load(),
	}
}

// claimStampTime extracts the wall-clock prefix of a claim token
// ("<rfc3339nano>|<nonce>"). ok is false for anything unparseable -
// absent stamps and foreign formats are silently not a signal.
func claimStampTime(token string) (time.Time, bool) {
	prefix, _, found := strings.Cut(token, "|")
	if !found {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, prefix)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// noteForeignStamp feeds the FutureStamps signal: called at the three
// places a peer's claim token is read (reclaiming a leftover stamp,
// and both lost-claim discoveries), never on a schedule.
func (w *SubmissionWorker) noteForeignStamp(token string) {
	if ts, ok := claimStampTime(token); ok && ts.Sub(w.now()) > futureStampTolerance {
		w.stats.futureStamps.Add(1)
	}
}
