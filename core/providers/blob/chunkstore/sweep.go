package chunkstore

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// Sweep reclaims the pieces of runs whose markers have gone stale: a marker
// not re-stamped within UploadReclaimWindow belongs to an upload that crashed or
// stalled, and the pieces it parked are referenced by no manifest. It scans
// only the marker range, which holds one key per in-progress upload and is
// empty in steady state, so the cost is proportional to in-flight (or
// orphaned) uploads, never to stored blob content. It runs at construction
// and may be called again by the host, for example on a timer, and returns
// the number of runs reclaimed.
//
// Staleness is judged from the wall-clock stamp stored in each marker
// (markerValue), read fresh during the scan, so any process - including one
// just started with no in-memory state - decides identically. An actively
// writing upload keeps its marker stamped within UploadRefreshInterval, so it is
// never stale; a genuine orphan is never refreshed and ages out.
//
// Safety. Sweep only ever deletes runs that carry a marker, and a run
// referenced by a committed manifest carries no marker (Commit writes the
// manifest and deletes the marker in one atomic batch; the dedup loser
// deletes its whole run). So a committed blob is never a Sweep candidate.
// The reclaim batch asserts the marker still holds the exact value the scan
// saw: every piece write and refresh changes that value, so a run that made
// any progress after being judged stale is spared rather than losing pieces
// the scan never covered. The worst a misjudged stamp can then do is reclaim
// a genuinely idle run early, after which that writer's next piece or Commit
// assert fails (errUploadReclaimed) and the upload retries rather than
// publishing over deleted pieces. Deleting linked content is unreachable by
// construction, not merely avoided.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	// Collect candidates first: the scan's key and value slices are only
	// valid during the callback, and deleting while scanning the same
	// range is avoided by acting after the scan returns.
	type candidate struct {
		acct   jmap.Id
		run    runID
		stamp  time.Time
		marker []byte
	}
	var candidates []candidate
	start, end := markerRange()
	err := s.be.Scan(ctx, start, end, false, func(_, value []byte) bool {
		if acct, run, stamp, ok := decodeMarker(value); ok {
			mv := make([]byte, len(value))
			copy(mv, value)
			candidates = append(candidates, candidate{acct: acct, run: run, stamp: stamp, marker: mv})
		}
		return true
	})
	if err != nil {
		return 0, err
	}

	now := s.now()
	reclaimed := 0
	for _, c := range candidates {
		if now.Sub(c.stamp) <= tuning.UploadReclaimWindow {
			continue // stamped recently: still being written
		}
		switch err := s.reclaimRun(ctx, c.acct, c.run, c.marker); {
		case errors.Is(err, backend.ErrAssertFailed):
			continue // the run made progress since the scan: it is live, spare it
		case err != nil:
			return reclaimed, err
		}
		reclaimed++
	}
	if reclaimed > 0 {
		log.Printf("naust-jmap chunkstore: reclaimed %d orphaned upload run(s)", reclaimed)
	}
	return reclaimed, nil
}

// deleteRun removes every piece of a run and its marker in one atomic
// batch. The piece keys are gathered by scanning the run's range; the
// marker is addressed directly. It is the writer-owned cleanup (Abort, the
// dedup discard): the caller is the run's writer, so no marker guard is
// needed.
func (s *Store) deleteRun(ctx context.Context, acct jmap.Id, run runID) error {
	b := &backend.Batch{}
	if err := s.appendRunDeletes(ctx, b, acct, run); err != nil {
		return err
	}
	return s.be.WriteBatch(ctx, b)
}

// reclaimRun is deleteRun for a run the caller does NOT own (Sweep): the
// batch asserts the marker still holds the value the caller's scan saw, so
// a run whose writer wrote a piece or refreshed after the scan - either
// changes the marker - fails the assert (backend.ErrAssertFailed) and keeps
// every piece, including any the scan-time delete list never covered.
func (s *Store) reclaimRun(ctx context.Context, acct jmap.Id, run runID, expectMarker []byte) error {
	b := &backend.Batch{}
	b.Assert(markerKey(run), expectMarker)
	if err := s.appendRunDeletes(ctx, b, acct, run); err != nil {
		return err
	}
	return s.be.WriteBatch(ctx, b)
}

// appendRunDeletes adds the deletes for a run's pieces and marker to b.
func (s *Store) appendRunDeletes(ctx context.Context, b *backend.Batch, acct jmap.Id, run runID) error {
	start, end := pieceRange(acct, run)
	err := s.be.Scan(ctx, start, end, false, func(key, _ []byte) bool {
		k := make([]byte, len(key))
		copy(k, key)
		b.Delete(k)
		return true
	})
	if err != nil {
		return err
	}
	b.Delete(markerKey(run))
	return nil
}
