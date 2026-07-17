package mail

// Delivery hardening tests: the panic boundary at the shared delivery seam (a
// hostile message must not crash the co-hosted process) and the thread-size
// cap that bounds the per-insert thread scan.

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
)

// panicResolver panics on every Resolve, to exercise the delivery panic
// boundary: a hostile message must not crash the process that co-hosts the
// JMAP server.
type panicResolver struct{}

func (panicResolver) Resolve(context.Context, string) (jmap.Id, bool) {
	panic("resolver boom")
}

// TestDeliverPanicRecovered: a panic below the delivery seam is recovered and
// turned into a TempFailed verdict for every recipient (RFC 5321: a local
// processing failure is transient - the MTA retries), not a process crash.
func TestDeliverPanicRecovered(t *testing.T) {
	_, db, store := emailServer(t)
	d := NewDeliverer(db, store, panicResolver{})

	evs := d.Deliver(context.Background(),
		deliveryEnv("s@example.com", "a@example.com", "b@example.com"),
		strings.NewReader(simpleMessage))
	if len(evs) != 2 {
		t.Fatalf("want one event per recipient (2) after recovered panic, got %d", len(evs))
	}
	for i, ev := range evs {
		if ev.Outcome != TempFailed {
			t.Fatalf("event %d outcome = %v, want TempFailed", i, ev.Outcome)
		}
	}
}

// TestOutcomeZeroValueIsSafe pins the structural invariant the delivery panic
// boundary rests on: the zero value of Outcome is TempFailed, the safe default.
// A recipient whose verdict is never reached (a panic mid-delivery) then reads
// as a transient failure the MTA retries, never a false "delivered".
func TestOutcomeZeroValueIsSafe(t *testing.T) {
	var o Outcome
	if o != TempFailed {
		t.Fatalf("zero-value Outcome = %v, want TempFailed", o)
	}
}

// mixedPanicResolver resolves one recipient, rejects a second, and panics on a
// third, so a panic strikes after a per-recipient verdict (the rejection) has
// already been decided.
type mixedPanicResolver struct{}

func (mixedPanicResolver) Resolve(_ context.Context, rcpt string) (jmap.Id, bool) {
	switch rcpt {
	case "good@example.com":
		return testAccount, true
	case "bad@example.com":
		return "", false
	default:
		panic("resolver boom")
	}
}

// TestDeliverPanicPreservesDecidedVerdicts: a panic below the seam keeps the
// verdicts already decided rather than rebuilding them all as transient. The
// recipient rejected before the panic stays Rejected (the MTA bounces it, not
// retries a non-existent address); the recipients whose verdict was not reached
// are the safe default (TempFailed). RFC 5321: a permanent rejection (5yz) and
// a transient failure (4yz) are different answers and must not be conflated.
func TestDeliverPanicPreservesDecidedVerdicts(t *testing.T) {
	_, db, store := emailServer(t)
	d := NewDeliverer(db, store, mixedPanicResolver{})

	evs := d.Deliver(context.Background(),
		deliveryEnv("s@example.com", "good@example.com", "bad@example.com", "boom@example.com"),
		strings.NewReader(simpleMessage))
	if len(evs) != 3 {
		t.Fatalf("want one event per recipient (3) after recovered panic, got %d", len(evs))
	}
	if evs[1].Outcome != Rejected || evs[1].Reason != "no such recipient" {
		t.Errorf("bad@ verdict = %v/%q, want Rejected/%q (a decided verdict must survive the panic)",
			evs[1].Outcome, evs[1].Reason, "no such recipient")
	}
	if evs[0].Outcome != TempFailed {
		t.Errorf("good@ verdict = %v, want TempFailed (undecided at panic -> safe default)", evs[0].Outcome)
	}
	if evs[2].Outcome != TempFailed {
		t.Errorf("boom@ verdict = %v, want TempFailed (undecided at panic -> safe default)", evs[2].Outcome)
	}
}

// TestDeliverStoreFullTempFails: when the blob store is at capacity, the write
// fails and the recipient tempfails (the MTA retries once space frees) rather
// than the message being lost.
func TestDeliverStoreFullTempFails(t *testing.T) {
	ts, db, _ := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	// A blob store far too small to hold the message.
	full := kvstore.New(memory.New(memory.WithCapacity(10)))
	d := NewDeliverer(db, full, mapResolver{"jane@example.com": testAccount})

	evs := d.Deliver(context.Background(),
		deliveryEnv("joe@example.com", "jane@example.com"),
		strings.NewReader(simpleMessage))
	if len(evs) != 1 || evs[0].Outcome != TempFailed {
		t.Fatalf("want tempfail when the store is full, got %+v", evs)
	}
}

// TestThreadSizeCapSplits: once a thread reaches threadSizeCap members,
// the next message with the same join keys (Message-ID + base subject) starts a
// fresh threadId instead of joining, bounding the per-insert thread scan. RFC
// 8621 section 3 does not mandate the join algorithm, so this split is allowed.
func TestThreadSizeCapSplits(t *testing.T) {
	// Exercise the split boundary at a small size (the default 1024 would build
	// a 1024-member thread, paying the full per-insert scan the cap exists to
	// bound). Package tests run sequentially, so restoring the default is safe.
	defer func(orig int) { threadSizeCap = orig }(threadSizeCap)
	threadSizeCap = 8

	ts, db, store := emailServer(t)
	// Same Message-ID and subject (identical join keys) with a varied body so
	// each is a distinct blob - the same-thread flood the cap bounds.
	msg := func(i int) string {
		return "Subject: Flood\r\nMessage-ID: <flood@x>\r\n\r\nbody " + strconv.Itoa(i) + "\r\n"
	}

	first := putEmail(t, db, store, msg(0), mbInbox, nil)
	firstThread := threadOf(t, ts, first)

	// Insert up to and including the cap boundary. Messages 1..cap-1 join (the
	// thread fills to the cap); message at the cap is the one that finds a full
	// thread and must split.
	capN := threadSizeCap
	var capID string
	for i := 1; i <= capN; i++ {
		id := putEmail(t, db, store, msg(i), mbInbox, nil)
		if i == capN {
			capID = id
		} else if i == capN-1 && threadOf(t, ts, id) != firstThread {
			t.Fatalf("message %d did not join the not-yet-full thread", i)
		}
	}
	if threadOf(t, ts, capID) == firstThread {
		t.Fatalf("message at the cap joined the full thread; want a new threadId (cap %d)", capN)
	}
}
