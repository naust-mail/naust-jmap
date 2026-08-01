package deliver

// Delivery hardening tests: the panic boundary at the shared delivery seam (a
// hostile message must not crash the co-hosted process) and the thread-size
// cap that bounds the per-insert thread scan.

import (
	"context"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
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
// turned into a mail.TempFailed verdict for every recipient (RFC 5321: a local
// processing failure is transient - the MTA retries), not a process crash.
func TestDeliverPanicRecovered(t *testing.T) {
	_, db, store := emailServer(t)
	d := mustDeliverer(t, db, store, panicResolver{})

	evs := d.Deliver(context.Background(),
		deliveryEnv("s@example.com", "a@example.com", "b@example.com"),
		strings.NewReader(simpleMessage))
	if len(evs) != 2 {
		t.Fatalf("want one event per recipient (2) after recovered panic, got %d", len(evs))
	}
	for i, ev := range evs {
		if ev.Outcome != mail.TempFailed {
			t.Fatalf("event %d outcome = %v, want mail.TempFailed", i, ev.Outcome)
		}
	}
}

// TestOutcomeZeroValueIsSafe pins the structural invariant the delivery panic
// boundary rests on: the zero value of Outcome is mail.TempFailed, the safe default.
// A recipient whose verdict is never reached (a panic mid-delivery) then reads
// as a transient failure the MTA retries, never a false "delivered".
func TestOutcomeZeroValueIsSafe(t *testing.T) {
	var o mail.Outcome
	if o != mail.TempFailed {
		t.Fatalf("zero-value Outcome = %v, want mail.TempFailed", o)
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
// recipient rejected before the panic stays mail.Rejected (the MTA bounces it, not
// retries a non-existent address); the recipients whose verdict was not reached
// are the safe default (mail.TempFailed). RFC 5321: a permanent rejection (5yz) and
// a transient failure (4yz) are different answers and must not be conflated.
func TestDeliverPanicPreservesDecidedVerdicts(t *testing.T) {
	_, db, store := emailServer(t)
	d := mustDeliverer(t, db, store, mixedPanicResolver{})

	evs := d.Deliver(context.Background(),
		deliveryEnv("s@example.com", "good@example.com", "bad@example.com", "boom@example.com"),
		strings.NewReader(simpleMessage))
	if len(evs) != 3 {
		t.Fatalf("want one event per recipient (3) after recovered panic, got %d", len(evs))
	}
	if evs[1].Outcome != mail.Rejected || evs[1].Reason != "no such recipient" {
		t.Errorf("bad@ verdict = %v/%q, want mail.Rejected/%q (a decided verdict must survive the panic)",
			evs[1].Outcome, evs[1].Reason, "no such recipient")
	}
	if evs[0].Outcome != mail.TempFailed {
		t.Errorf("good@ verdict = %v, want mail.TempFailed (undecided at panic -> safe default)", evs[0].Outcome)
	}
	if evs[2].Outcome != mail.TempFailed {
		t.Errorf("boom@ verdict = %v, want mail.TempFailed (undecided at panic -> safe default)", evs[2].Outcome)
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
	d := mustDeliverer(t, db, full, mapResolver{"jane@example.com": testAccount})

	evs := d.Deliver(context.Background(),
		deliveryEnv("joe@example.com", "jane@example.com"),
		strings.NewReader(simpleMessage))
	if len(evs) != 1 || evs[0].Outcome != mail.TempFailed {
		t.Fatalf("want tempfail when the store is full, got %+v", evs)
	}
}
