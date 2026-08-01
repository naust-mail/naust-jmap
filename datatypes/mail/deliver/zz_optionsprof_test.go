package deliver

// Throwaway (zz_prof_test.go precedent): what do the delivery-side options
// cost on the paths that actually run per message? A 1.4 MiB delivery is
// dominated by parse + hash + store, and its run-to-run variance swamps a
// sequential A-then-B comparison, so each comparison here interleaves its
// two variants message-by-message and reports the paired delta:
//
//   bare vs wired    - WithReportIngestion + WithVacationResponder with the
//                      vacation record absent: the always-on overhead an
//                      embedder pays for wiring the options (per accepted
//                      recipient: the RFC 3834 header gates plus one
//                      vacation-config read)
//   bare vs enabled  - vacation on, sender already suppressed: the
//                      steady-state repeated-sender path (gates + config
//                      read + identity load + suppression pre-check, no
//                      reply built)
//   plain vs DSN     - small null-sender messages: an ordinary one against
//                      a multipart/report DSN whose ENVID matches nothing
//                      (report parse + failed correlation + ordinary-mail
//                      delivery), the shape an inbound bounce flood takes
//
// Same in-memory backend and fsstore as the bare-delivery profile. Deltas
// are the signal; absolute numbers drift as the shared db's indexes grow.

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/providers/blob/fsstore"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
)

func TestZZProfileDeliveryOptions(t *testing.T) {
	ts, db, _, q, _, _ := newEmailServer(t, mail.DefaultAccountCapability())
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
	createIdentity(t, ts, "john@example.com")
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resolve := mapResolver{"john@example.com": testAccount}
	bare := mustDeliverer(t, db, store, resolve)
	wired := mustDeliverer(t, db, store, resolve,
		WithReportIngestion(), WithVacationResponder(q))

	env := deliveryEnv("joe@remote.example", "john@example.com")
	seq := 0
	deliverOne := func(d *Deliverer, msg string) time.Duration {
		start := time.Now()
		evs := d.Deliver(context.Background(), env, strings.NewReader(msg))
		elapsed := time.Since(start)
		if evs[0].Outcome != mail.Accepted {
			t.Fatalf("delivery: %v %q", evs[0].Outcome, evs[0].Reason)
		}
		return elapsed
	}
	// pair interleaves a and b over n distinct messages each, returning the
	// mean per-op time of both sides. Alternating per message means clock
	// drift, GC pauses, and index growth land on both sides equally.
	const n = 30
	pair := func(a, b *Deliverer) (time.Duration, time.Duration) {
		var ta, tb time.Duration
		for i := 0; i < n; i++ {
			ta += deliverOne(a, benchMessage(seq, "john@example.com"))
			tb += deliverOne(b, benchMessage(seq+1, "john@example.com"))
			seq += 2
		}
		return ta / n, tb / n
	}

	bare1, wiredOff := pair(bare, wired)

	// Turn the responder on and burn one delivery to write the reply and
	// its suppression row, so the measured side is the suppressed steady
	// state of a repeated sender.
	enableVacation(t, ts, `,"textBody":"Away until August."`)
	deliverOne(wired, benchMessage(seq, "john@example.com"))
	seq++
	bare2, enabled := pair(bare, wired)

	// Small-message pair: ordinary mail vs an unmatched DSN, both from the
	// null sender so the ingestion gate is armed for both.
	env = deliveryEnv("", "john@example.com")
	const nr = 200
	var plainT, dsnT time.Duration
	var plainLen, dsnLen int
	// CPU profile of the small-message loop alone: with parse and hashing
	// near zero at this size, what remains is the fixed per-delivery cost.
	f, err := os.Create(profPath(t, "smalldeliver.cpu"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runtime.GC()
	var r0, r1 runtime.MemStats
	runtime.ReadMemStats(&r0)
	if err := pprof.StartCPUProfile(f); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < nr; i++ {
		plain := fmt.Sprintf("From: joe@remote.example\r\nTo: john@example.com\r\n"+
			"Subject: note %d\r\nMessage-ID: <n%d@remote.example>\r\n\r\nBody %d.\r\n", i, i, i)
		dsn := dsnFor("no-such-submission", "someone@far.example",
			"failed", "5.1.1", fmt.Sprintf("550 5.1.1 no user %d", i), "")
		plainT += deliverOne(wired, plain)
		dsnT += deliverOne(wired, dsn)
		plainLen, dsnLen = len(plain), len(dsn)
	}
	pprof.StopCPUProfile()
	runtime.ReadMemStats(&r1)
	writeAllocsProfile(t, "smalldeliver.allocs")

	// The serial small-message cost is dominated by the durable blob commit
	// (fsync waits, mostly off-CPU). Whether that amortizes under the
	// concurrent deliveries a real LMTP front end produces depends on what
	// holds the account lease: the blob commit runs inside
	// FinalizeBlobUploadThenUpdate, so same-account deliveries are the
	// worst case. Measure the aggregate rate to see how much overlaps.
	const workers = 8
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < nr/workers; i++ {
				msg := fmt.Sprintf("From: joe@remote.example\r\nTo: john@example.com\r\n"+
					"Subject: par %d-%d\r\nMessage-ID: <p%d-%d@remote.example>\r\n\r\nBody.\r\n", w, i, w, i)
				evs := wired.Deliver(context.Background(), env, strings.NewReader(msg))
				if evs[0].Outcome != mail.Accepted {
					t.Errorf("parallel %d-%d: %v %q", w, i, evs[0].Outcome, evs[0].Reason)
				}
			}
		}(w)
	}
	wg.Wait()
	parPer := time.Since(start) / time.Duration(nr/workers*workers)

	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	fmt.Printf("\n  delivery options, interleaved pairs, %d x 1401 KiB per side, in-memory backend\n", n)
	fmt.Printf("  bare %6.1f ms/op vs wired vac-off %6.1f ms/op   delta %+.2f ms\n",
		ms(bare1), ms(wiredOff), ms(wiredOff-bare1))
	fmt.Printf("  bare %6.1f ms/op vs vac suppressed %5.1f ms/op   delta %+.2f ms\n",
		ms(bare2), ms(enabled), ms(enabled-bare2))
	fmt.Printf("  small null-sender pair, %d x (%d B plain / %d B DSN), %d mallocs/op both sides:\n",
		nr, plainLen, dsnLen, (r1.Mallocs-r0.Mallocs)/(2*nr))
	fmt.Printf("  plain %.0f us/op vs unmatched DSN %.0f us/op   delta %+.0f us\n",
		float64((plainT / nr).Microseconds()), float64((dsnT / nr).Microseconds()),
		float64((dsnT/nr).Microseconds()-(plainT/nr).Microseconds()))
	fmt.Printf("  parallel x%d small plain: %.0f us/op aggregate  %.0f op/s (serial was %.0f op/s)\n\n",
		workers, float64(parPer.Microseconds()),
		float64(time.Second)/float64(parPer),
		float64(time.Second)/float64(plainT/nr))
}
