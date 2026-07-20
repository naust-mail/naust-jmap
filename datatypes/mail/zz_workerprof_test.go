package mail

// Throwaway (zz_prof_test.go precedent): where do the submission
// worker's CPU cycles and allocations go? In-memory backend plus an
// accept-all fake Submitter, so what these burn is the worker's own
// bookkeeping (tag scan, probe, claim, blob open, finalize JSON), not
// storage or the network. Profiles land in profPath's dir; analyze with
// go tool pprof -top <file>. MemProfileRate is raised at test entry, so
// the allocs profile also samples setup traffic - use -focus on worker
// frames, and trust the MemStats deltas (timed region only) for totals.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"
)

func writeAllocsProfile(t *testing.T, name string) {
	t.Helper()
	f, err := os.Create(profPath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runtime.GC() // flush current allocation state into the profile
	if err := pprof.Lookup("allocs").WriteTo(f, 0); err != nil {
		t.Fatal(err)
	}
}

// TestZZProfileWorkerSend: N due submissions drained through the full
// claim -> transmit -> finalize path.
func TestZZProfileWorkerSend(t *testing.T) {
	runtime.MemProfileRate = 4096
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	const n = 400
	for i := 0; i < n; i++ {
		submitEnvelope(t, ts, identityId, emailId, fmt.Sprintf(
			`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"a%d@x.example"},{"email":"b%d@x.example"}]}`, i, i))
	}
	// Creation runs on the REAL clock (HTTP server stamps records) and can
	// outlast the frozen clock's 2s head start, especially under -race;
	// jump the worker clock well past the creation window so every record
	// is due.
	clock.advance(time.Hour)

	f, err := os.Create(profPath(t, "workersend.cpu"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	sent, _, err := w.ProcessDue(context.Background(), 0)
	elapsed := time.Since(start)
	pprof.StopCPUProfile()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	if err != nil || sent != n {
		t.Fatalf("drained %d (want %d), err %v", sent, n, err)
	}
	if fake.callCount() != n {
		t.Fatalf("submitter called %d times", fake.callCount())
	}
	writeAllocsProfile(t, "workersend.allocs")

	fmt.Printf("\n  send path: %d submissions x 2 rcpt, in-memory backend, accept-all submitter\n", n)
	fmt.Printf("  %.2f ms/submission  %.0f KiB alloc/submission  %d mallocs/submission\n\n",
		float64(elapsed.Microseconds())/1000/n,
		float64(m1.TotalAlloc-m0.TotalAlloc)/1024/n,
		(m1.Mallocs-m0.Mallocs)/n)
}

// TestZZProfileWorkerIdle: the reconciliation tick with a deep future
// queue - what every QueueScanInterval costs when there is nothing to
// send.
func TestZZProfileWorkerIdle(t *testing.T) {
	runtime.MemProfileRate = 4096
	ts, db, store, w, fake, _ := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	const n = 500
	hold := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for i := 0; i < n; i++ {
		submitEnvelope(t, ts, identityId, emailId, fmt.Sprintf(
			`{"mailFrom":{"email":"john@example.com","parameters":{"HOLDUNTIL":%q}},"rcptTo":[{"email":"a%d@x.example"}]}`, hold, i))
	}

	const ticks = 2000
	f, err := os.Create(profPath(t, "workeridle.cpu"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < ticks; i++ {
		sent, _, pending, err := w.sweep(context.Background(), 0)
		if err != nil || sent != 0 || !pending {
			t.Fatalf("tick %d: sent %d pending %v err %v", i, sent, pending, err)
		}
	}
	elapsed := time.Since(start)
	pprof.StopCPUProfile()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	if fake.callCount() != 0 {
		t.Fatalf("idle queue transmitted %d times", fake.callCount())
	}
	writeAllocsProfile(t, "workeridle.allocs")

	fmt.Printf("\n  idle tick: %d held submissions, %d sweeps\n", n, ticks)
	fmt.Printf("  %.1f us/sweep  %.1f KiB alloc/sweep  %d mallocs/sweep\n\n",
		float64(elapsed.Microseconds())/ticks,
		float64(m1.TotalAlloc-m0.TotalAlloc)/1024/ticks,
		(m1.Mallocs-m0.Mallocs)/ticks)
}
