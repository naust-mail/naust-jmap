package mail

// End-to-end submission lifecycle and a property test over the queue's
// state transitions. These exercise the whole slice as one system - the
// JMAP surface, the stored queue, the sending worker, and the clock -
// rather than any single method in isolation.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// TestWorkerEndToEndFutureRelease walks a message from composition to a
// final delivery status through every layer: compose a draft over JMAP,
// submit it held (FUTURERELEASE) with onSuccessUpdateEmail, watch the
// implicit set file it into Sent while it queues, confirm the worker
// leaves a held message alone, then release the clock and watch the send
// drive undoStatus and deliveryStatus to their final values.
func TestWorkerEndToEndFutureRelease(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	ctx := context.Background()
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	sent := createMailbox(t, ts, `{"name":"Sent"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil),
		map[string]bool{drafts: true}, map[string]bool{"$draft": true})

	// Submit held two hours out, moving the draft to Sent on success.
	until := clock.now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,
		  "create":{"s":{"identityId":%q,"emailId":%q,
		    "envelope":{"mailFrom":{"email":"john@example.com","parameters":{"HOLDUNTIL":%q}},
		      "rcptTo":[{"email":"jane@remote.example"}]}}},
		  "onSuccessUpdateEmail":{"#s":{
		    "mailboxIds/%s":null,"mailboxIds/%s":true,"keywords/$draft":null}}}`,
		testAccount, identityId, emailId, until.Format(time.RFC3339), drafts, sent), "0"))
	echo := createdEcho(t, r)
	subId := echo["id"].(string)
	if echo["undoStatus"] != "pending" {
		t.Fatalf("held submission undoStatus = %v, want pending", echo["undoStatus"])
	}
	if echo["sendAt"] != until.Format(time.RFC3339) {
		t.Fatalf("sendAt = %v, want %v", echo["sendAt"], until.Format(time.RFC3339))
	}

	// The implicit Email/set already moved it to Sent, before it is sent.
	got := emailGet(t, ts, emailId, `,"properties":["mailboxIds","keywords"]`)
	if boxes := got["mailboxIds"].(map[string]any); len(boxes) != 1 || boxes[sent] != true {
		t.Fatalf("mailboxIds after submit = %v, want only Sent", got["mailboxIds"])
	}

	// A held message is not due: a worker pass sends nothing.
	w.sweep(ctx, 0)
	if fake.callCount() != 0 {
		t.Fatalf("held message transmitted early (%d calls)", fake.callCount())
	}
	stored := subRecord(t, db, subId)
	if recDeliveryStatus(t, stored)["jane@remote.example"].Delivered != "queued" {
		t.Fatalf("held message not queued: %v", stored["deliveryStatus"])
	}

	// Release: past sendAt, the pass transmits exactly once and finalizes.
	clock.advance(3 * time.Hour)
	w.sweep(ctx, 0)
	if fake.callCount() != 1 {
		t.Fatalf("released message transmitted %d times, want 1", fake.callCount())
	}
	call := fake.call(0)
	if call.env.MailFrom != "john@example.com" || len(call.env.Recipients) != 1 ||
		call.env.Recipients[0].Email != "jane@remote.example" {
		t.Fatalf("transmitted envelope = %+v", call.env)
	}
	if strings.Contains(call.msg, "HOLDUNTIL") {
		t.Error("hold parameter leaked onto the wire")
	}

	// Final state: relayed (delivered "unknown" - external relay is not
	// proof of delivery), undoStatus final, and out of the queue.
	final := subRecord(t, db, subId)
	if recString(final, "undoStatus") != undoFinal {
		t.Errorf("undoStatus = %s, want final", final["undoStatus"])
	}
	ds := recDeliveryStatus(t, final)["jane@remote.example"]
	if ds.Delivered != "unknown" {
		t.Errorf("delivered = %s, want unknown", ds.Delivered)
	}
	if _, queued := final["nextAttemptAt"]; queued {
		t.Error("delivered submission still carries nextAttemptAt")
	}
	// The queue drained: a sweep reports nothing pending, and clears the
	// tag once the drain is confirmed under the lease.
	if _, _, pending, err := w.sweep(ctx, 0); err != nil {
		t.Fatal(err)
	} else if pending {
		t.Error("drained queue still reports due work")
	}
	tagged, err := db.TaggedAccounts(ctx, submissionQueueTag)
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged) != 0 {
		t.Errorf("drained account still tagged: %v", tagged)
	}
}

// TestWorkerQueueTransitionInvariants drives many submissions through
// random sequences of per-recipient smarthost verdicts, cancels, and
// clock jumps, asserting the queue's state machine never violates its
// invariants no matter the order:
//
//   - undoStatus never leaves a terminal state (final/canceled) once
//     reached, and never regresses to pending;
//   - a recipient that reached a final delivered value (yes/no/unknown)
//     never returns to "queued";
//   - attempts is monotonic non-decreasing;
//   - a record with no queued recipient carries no nextAttemptAt (it left
//     the queue), and one still queued keeps both nextAttemptAt and the
//     account tag;
//   - abandonment (the 5.4.7 give-up) happens only at or past GiveUpAfter
//     AND at or past MinAttempts.
func TestWorkerQueueTransitionInvariants(t *testing.T) {
	const seed = 20260718
	rng := rand.New(rand.NewSource(seed))
	cfg := SubmissionWorkerConfig{
		RetrySchedule: []time.Duration{time.Minute, 2 * time.Minute},
		RetryPlateau:  5 * time.Minute,
		GiveUpAfter:   30 * time.Minute,
		MinAttempts:   3,
	}
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), cfg)
	ctx := context.Background()
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")

	// A randomized smarthost: each recipient independently accepts,
	// rejects, or temp-fails, so a submission can partially progress and
	// keep retrying the rest.
	fake.respond = func(env SubmissionEnvelope) ([]RecipientResult, error) {
		out := make([]RecipientResult, len(env.Recipients))
		for i, r := range env.Recipients {
			switch rng.Intn(3) {
			case 0:
				out[i] = RecipientResult{Recipient: r.Email, Outcome: Accepted, Reply: "250 2.0.0 ok"}
			case 1:
				out[i] = RecipientResult{Recipient: r.Email, Outcome: Rejected, Reply: "550 5.1.1 no such user"}
			default:
				out[i] = RecipientResult{Recipient: r.Email, Outcome: TempFailed, Reply: "451 4.3.0 try later"}
			}
		}
		return out, nil
	}
	// Occasionally the whole attempt fails to connect.
	baseRespond := fake.respond

	// Create a handful of multi-recipient submissions.
	var ids []string
	for n := 0; n < 6; n++ {
		emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
		rcpts := fmt.Sprintf(`[{"email":"a%d@x.example"},{"email":"b%d@x.example"}]`, n, n)
		id := submitEnvelope(t, ts, identityId, emailId, fmt.Sprintf(
			`{"mailFrom":{"email":"john@example.com"},"rcptTo":%s}`, rcpts))
		ids = append(ids, id)
	}

	// prev tracks the last observed state per submission for the
	// monotonicity checks.
	type snap struct {
		undo     string
		attempts uint64
		final    map[string]bool // recipients that reached a terminal delivered value
		gone     bool            // no nextAttemptAt
	}
	prev := map[string]snap{}
	terminal := func(d string) bool { return d == "yes" || d == "no" || d == "unknown" }

	check := func(step int) {
		for _, id := range ids {
			rec := subRecord(t, db, id)
			undo := recString(rec, "undoStatus")
			var attempts uint64
			json.Unmarshal(rec["attempts"], &attempts)
			ds := recDeliveryStatus(t, rec)
			_, hasNext := rec["nextAttemptAt"]

			p, seen := prev[id]
			if seen {
				// undoStatus never leaves a terminal state or regresses.
				if (p.undo == undoFinal || p.undo == undoCanceled) && undo != p.undo {
					t.Fatalf("step %d %s: undoStatus %s -> %s (terminal regressed)", step, id, p.undo, undo)
				}
				if p.undo == undoFinal && undo == "pending" {
					t.Fatalf("step %d %s: final -> pending", step, id)
				}
				// attempts monotonic.
				if attempts < p.attempts {
					t.Fatalf("step %d %s: attempts %d -> %d", step, id, p.attempts, attempts)
				}
				// a terminal recipient never returns to queued.
				for rcpt := range p.final {
					if ds[rcpt].Delivered == "queued" {
						t.Fatalf("step %d %s: recipient %s regressed to queued", step, id, rcpt)
					}
				}
			}

			// Structural invariant: queued iff still on the queue.
			anyQueued := false
			for _, st := range ds {
				if st.Delivered == "queued" {
					anyQueued = true
				}
			}
			if anyQueued && !hasNext {
				t.Fatalf("step %d %s: queued recipient but no nextAttemptAt", step, id)
			}
			if !anyQueued && hasNext {
				t.Fatalf("step %d %s: nextAttemptAt lingers with nothing queued", step, id)
			}
			// If it is still queued, the account must still be tagged.
			if anyQueued {
				tagged, err := db.TaggedAccounts(ctx, submissionQueueTag)
				if err != nil {
					t.Fatal(err)
				}
				if len(tagged) == 0 {
					t.Fatalf("step %d %s: queued work but account untagged", step, id)
				}
			}
			// Give-up (5.4.7) only at/past both thresholds.
			for rcpt, st := range ds {
				if st.Delivered == "no" && strings.Contains(st.SmtpReply, "5.4.7") {
					sendAt, err := parseUTCDateValue(rec["sendAt"])
					if err != nil {
						t.Fatalf("step %d %s: unparseable sendAt", step, id)
					}
					if clock.now().Sub(sendAt) < cfg.GiveUpAfter {
						t.Fatalf("step %d %s: %s abandoned before GiveUpAfter", step, id, rcpt)
					}
					if attempts < uint64(cfg.MinAttempts) {
						t.Fatalf("step %d %s: %s abandoned at attempt %d (< MinAttempts)", step, id, rcpt, attempts)
					}
				}
			}

			nf := map[string]bool{}
			for rcpt, st := range ds {
				if terminal(st.Delivered) {
					nf[rcpt] = true
				}
			}
			prev[id] = snap{undo: undo, attempts: attempts, final: nf, gone: !hasNext}
		}
	}

	// Drive: random passes interleaved with clock jumps and the occasional
	// cancel of a still-cancelable submission, well past GiveUpAfter so the
	// give-up path is exercised too.
	for step := 0; step < 60; step++ {
		switch rng.Intn(4) {
		case 0:
			// A connection-level failure this pass (every recipient tempfails).
			fake.respond = func(env SubmissionEnvelope) ([]RecipientResult, error) {
				return nil, fmt.Errorf("dial failed")
			}
		default:
			fake.respond = baseRespond
		}
		w.sweep(ctx, 0)
		clock.advance(time.Duration(1+rng.Intn(15)) * time.Minute)

		// Occasionally try to cancel a random submission (legal only while
		// pending and unclaimed; an illegal cancel is a harmless no-op here).
		if rng.Intn(3) == 0 {
			id := ids[rng.Intn(len(ids))]
			callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
				`{"accountId":%q,"update":{%q:{"undoStatus":"canceled"}}}`, testAccount, id), "0"))
		}
		check(step)
	}
}
