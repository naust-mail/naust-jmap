package mail

// SubmissionWorker tests: the queue engine driven deterministically
// through its single-pass internals with an injected clock (Run's loop
// gets one real-clock integration test). Chaos coverage per the step-8
// plan: crash between claim and finalize (stale-claim reclaim, no
// double-finalize), cancel racing the queue, restart resume, backoff
// schedule, give-up, and the Bcc strip on the wire.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// testClock is a settable clock for the worker's now seam.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeSubmitter records every attempt and answers via respond (accept
// everything when nil).
type fakeSubmitter struct {
	mu      sync.Mutex
	calls   []fakeCall
	respond func(env SubmissionEnvelope) ([]RecipientResult, error)
}

type fakeCall struct {
	env SubmissionEnvelope
	msg string
}

func (f *fakeSubmitter) Submit(_ context.Context, env SubmissionEnvelope, msg io.Reader) ([]RecipientResult, error) {
	b, err := io.ReadAll(msg)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{env: env, msg: string(b)})
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		return respond(env)
	}
	out := make([]RecipientResult, len(env.Recipients))
	for i, r := range env.Recipients {
		out[i] = RecipientResult{Recipient: r.Email, Outcome: Accepted, Reply: "250 2.0.0 accepted"}
	}
	return out, nil
}

func (f *fakeSubmitter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSubmitter) call(i int) fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

// newWorkerServer wires the full submission stack PLUS a worker with an
// injected clock. The worker is registered as the creation bell target
// but not started: tests drive sweep deterministically.
func newWorkerServer(t *testing.T, limits SubmissionLimits, wcfg SubmissionWorkerConfig) (*httptest.Server, *objectdb.DB, blob.Store, *SubmissionWorker, *fakeSubmitter, *testClock) {
	t.Helper()
	a := newStaticAuth()
	a.AddUser("john@example.com", "secret", testAccount)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(memory.New())
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	policy := &submissionPolicy{StaticSendPolicy: NewStaticSendPolicy()}
	policy.Allow(testAccount, "john@example.com", "*@corp.example")
	fake := &fakeSubmitter{}
	if err := RegisterMailbox(p, db, core); err != nil {
		t.Fatal(err)
	}
	if err := RegisterThread(p, db, core); err != nil {
		t.Fatal(err)
	}
	if err := RegisterEmail(p, db, store, core, DefaultAccountCapability(), nil); err != nil {
		t.Fatal(err)
	}
	if err := RegisterIdentity(p, db, policy, core); err != nil {
		t.Fatal(err)
	}
	q, err := RegisterEmailSubmission(p, db, store, core, policy, limits)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewSubmissionWorker(q, fake, wcfg)
	if err != nil {
		t.Fatal(err)
	}
	// The clock starts ahead of the real clock: records created over HTTP
	// are stamped by the server on the REAL clock at second granularity,
	// so a frozen clock taken before the commit can land a hair before
	// the record's (truncated) due second and a sweep would correctly
	// skip it - the margin keeps promptly-created records due.
	clock := &testClock{t: time.Now().Add(2 * time.Second)}
	w.now = clock.now
	// Pin the jitter seam to its midpoint: every jitter formula collapses
	// to exactly its base value, keeping timing assertions deterministic.
	w.rand = func() float64 { return 0.5 }
	srv, err := runtime.NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(CapabilityURI, struct{}{}, DefaultAccountCapability()); err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(SubmissionCapabilityURI, struct{}{}, SubmissionAccountCapabilityFor(limits)); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, db, store, w, fake, clock
}

// submitEnvelope creates a submission with an explicit envelope and
// returns the submission id.
func submitEnvelope(t *testing.T, ts *httptest.Server, identityId, emailId, envelope string) string {
	t.Helper()
	r := submitOne(t, ts, fmt.Sprintf(
		`{"identityId":%q,"emailId":%q,"envelope":%s}`, identityId, emailId, envelope))
	return createdEcho(t, r)["id"].(string)
}

// subRecord loads a submission record raw (internal properties
// included).
func subRecord(t *testing.T, db *objectdb.DB, id string) objectdb.Object {
	t.Helper()
	rec, err := db.Get(context.Background(), testAccount, TypeEmailSubmission, jmap.Id(id))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func recDeliveryStatus(t *testing.T, rec objectdb.Object) map[string]deliveryStatusObj {
	t.Helper()
	var ds map[string]deliveryStatusObj
	if err := json.Unmarshal(rec["deliveryStatus"], &ds); err != nil {
		t.Fatal(err)
	}
	return ds
}

func recString(rec objectdb.Object, prop string) string {
	var s string
	json.Unmarshal(rec[prop], &s)
	return s
}

func TestWorkerConfigInvariant(t *testing.T) {
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(memory.New())
	q := newSubmissionQueue(db, store)
	if _, err := NewSubmissionWorker(q, &fakeSubmitter{}, SubmissionWorkerConfig{
		ClaimWindow: 3 * time.Minute, TransmitTimeout: 2 * time.Minute,
	}); err == nil {
		t.Fatal("ClaimWindow < 3x TransmitTimeout must be rejected at construction")
	}
	if _, err := NewSubmissionWorker(q, nil, SubmissionWorkerConfig{}); err == nil {
		t.Fatal("nil Submitter must be rejected")
	}
	if _, err := NewSubmissionWorker(nil, &fakeSubmitter{}, SubmissionWorkerConfig{}); err == nil {
		t.Fatal("nil SubmissionQueue must be rejected")
	}
	if _, err := NewSubmissionWorker(q, &fakeSubmitter{}, SubmissionWorkerConfig{}); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
}

// TestWorkerSendFlow: submit, one pass, delivered. The message carries a
// Bcc recipient: the derived envelope includes it (section 7), but the
// transmitted bytes MUST NOT (section 7.5) - the strip is verified on
// the captured wire message.
func TestWorkerSendFlow(t *testing.T) {
	ts, db, store, w, fake, _ := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(map[string]string{"Bcc": "secret@corp.example"}),
		map[string]bool{drafts: true}, nil)
	r := submitOne(t, ts, fmt.Sprintf(`{"identityId":%q,"emailId":%q}`, identityId, emailId))
	id := createdEcho(t, r)["id"].(string)

	// The creation bell fired and the tag worklist names the account.
	select {
	case <-w.q.bell:
	default:
		t.Fatal("create did not ring the queue")
	}
	tagged, err := db.TaggedAccounts(context.Background(), submissionQueueTag)
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged) != 1 || tagged[0] != testAccount {
		t.Fatalf("queue tag = %v", tagged)
	}

	w.sweep(context.Background(), 0)

	if fake.callCount() != 1 {
		t.Fatalf("submitter called %d times", fake.callCount())
	}
	call := fake.call(0)
	if call.env.MailFrom != "john@example.com" {
		t.Errorf("mailFrom = %q", call.env.MailFrom)
	}
	if len(call.env.Recipients) != 2 {
		t.Fatalf("recipients = %v", call.env.Recipients)
	}
	if strings.Contains(call.msg, "Bcc") || strings.Contains(call.msg, "secret@corp.example") {
		t.Errorf("Bcc leaked into the transmitted message:\n%s", call.msg)
	}
	if !strings.Contains(call.msg, "Subject: Hello") || !strings.Contains(call.msg, "body") {
		t.Errorf("message mangled:\n%s", call.msg)
	}
	if call.env.Size <= 0 {
		t.Errorf("size = %d", call.env.Size)
	}

	rec := subRecord(t, db, id)
	if recString(rec, "undoStatus") != undoFinal {
		t.Errorf("undoStatus = %s", rec["undoStatus"])
	}
	for rcpt, st := range recDeliveryStatus(t, rec) {
		if st.Delivered != "unknown" || st.SmtpReply != "250 2.0.0 accepted" {
			t.Errorf("deliveryStatus[%s] = %+v", rcpt, st)
		}
	}
	if _, has := rec["nextAttemptAt"]; has {
		t.Error("nextAttemptAt survived completion")
	}
	if _, has := rec["claimedAt"]; has {
		t.Error("claimedAt survived finalize")
	}
	var attempts int
	json.Unmarshal(rec["attempts"], &attempts)
	if attempts != 1 {
		t.Errorf("attempts = %d", attempts)
	}
	// Queue drained: a sweep reports no pending work.
	if _, _, pending, _ := w.sweep(context.Background(), 0); pending {
		t.Error("empty queue still reports due work")
	}
}

// TestWorkerMixedRecipients: per-recipient outcomes land independently,
// the retry only re-attempts the temp-failed recipient, and recipient
// parameters ride along to the Submitter.
func TestWorkerMixedRecipients(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	fake.respond = func(env SubmissionEnvelope) ([]RecipientResult, error) {
		var out []RecipientResult
		for _, r := range env.Recipients {
			switch r.Email {
			case "ok@x.example":
				out = append(out, RecipientResult{r.Email, Accepted, "250 2.1.5 ok"})
			case "gone@x.example":
				out = append(out, RecipientResult{r.Email, Rejected, "550 5.1.1 no such user"})
			default:
				out = append(out, RecipientResult{r.Email, TempFailed, "451 4.7.0 greylisted"})
			}
		}
		return out, nil
	}
	id := submitEnvelope(t, ts, identityId, emailId, `{
		"mailFrom":{"email":"john@example.com"},
		"rcptTo":[{"email":"ok@x.example","parameters":{"NOTIFY":"SUCCESS,FAILURE"}},
		          {"email":"gone@x.example"},
		          {"email":"slow@x.example"}]}`)

	w.sweep(context.Background(), 0)
	if fake.callCount() != 1 {
		t.Fatalf("submitter called %d times", fake.callCount())
	}
	if p := fake.call(0).env.Recipients[0].Parameters["NOTIFY"]; p == nil || *p != "SUCCESS,FAILURE" {
		t.Errorf("recipient parameters lost: %v", fake.call(0).env.Recipients[0].Parameters)
	}
	rec := subRecord(t, db, id)
	ds := recDeliveryStatus(t, rec)
	if ds["ok@x.example"].Delivered != "unknown" || ds["gone@x.example"].Delivered != "no" ||
		ds["slow@x.example"].Delivered != "queued" {
		t.Fatalf("deliveryStatus = %+v", ds)
	}
	if ds["slow@x.example"].SmtpReply != "451 4.7.0 greylisted" {
		t.Errorf("temp reply = %q", ds["slow@x.example"].SmtpReply)
	}
	// One recipient irrevocably relayed: no longer cancelable, but the
	// temp failure keeps retrying.
	if recString(rec, "undoStatus") != undoFinal {
		t.Errorf("undoStatus = %s", rec["undoStatus"])
	}
	next, err := parseUTCDateValue(rec["nextAttemptAt"])
	if err != nil {
		t.Fatalf("nextAttemptAt: %v", err)
	}
	if d := next.Sub(clock.now()); d < 50*time.Second || d > 70*time.Second {
		t.Errorf("first backoff = %v, want ~1m", d)
	}

	// The retry attempts ONLY the still-queued recipient.
	fake.respond = nil // accept everything now
	clock.advance(61 * time.Second)
	w.sweep(context.Background(), 0)
	if fake.callCount() != 2 {
		t.Fatalf("submitter called %d times", fake.callCount())
	}
	retry := fake.call(1)
	if len(retry.env.Recipients) != 1 || retry.env.Recipients[0].Email != "slow@x.example" {
		t.Fatalf("retry recipients = %v", retry.env.Recipients)
	}
	rec = subRecord(t, db, id)
	if recDeliveryStatus(t, rec)["slow@x.example"].Delivered != "unknown" {
		t.Errorf("retry outcome = %+v", recDeliveryStatus(t, rec))
	}
	if _, has := rec["nextAttemptAt"]; has {
		t.Error("nextAttemptAt survived the final recipient")
	}
}

// TestWorkerPartialResultsWithError: results returned alongside a
// Submitter error are applied - the accepted recipient is settled and
// never re-sent, only the recipient without a verdict temp-fails with
// the synthetic reply and retries. This is the fan-out shape: one
// destination's transaction completed before another's failed.
func TestWorkerPartialResultsWithError(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	fake.respond = func(env SubmissionEnvelope) ([]RecipientResult, error) {
		return []RecipientResult{{"ok@x.example", Accepted, "250 2.1.5 ok"}},
			fmt.Errorf("connection to second destination lost")
	}
	id := submitEnvelope(t, ts, identityId, emailId, `{
		"mailFrom":{"email":"john@example.com"},
		"rcptTo":[{"email":"ok@x.example"},{"email":"lost@y.example"}]}`)

	w.sweep(context.Background(), 0)
	rec := subRecord(t, db, id)
	ds := recDeliveryStatus(t, rec)
	if ds["ok@x.example"].Delivered != "unknown" || ds["ok@x.example"].SmtpReply != "250 2.1.5 ok" {
		t.Fatalf("accepted result dropped on error: %+v", ds["ok@x.example"])
	}
	if ds["lost@y.example"].Delivered != "queued" ||
		ds["lost@y.example"].SmtpReply != "451 4.4.1 could not transmit to smarthost" {
		t.Fatalf("verdict-less recipient = %+v", ds["lost@y.example"])
	}
	// One recipient irrevocably relayed despite the error: not cancelable.
	if recString(rec, "undoStatus") != undoFinal {
		t.Errorf("undoStatus = %s", rec["undoStatus"])
	}

	// The retry re-attempts ONLY the recipient the error left unsettled.
	fake.respond = nil
	clock.advance(61 * time.Second)
	w.sweep(context.Background(), 0)
	if fake.callCount() != 2 {
		t.Fatalf("submitter called %d times", fake.callCount())
	}
	retry := fake.call(1)
	if len(retry.env.Recipients) != 1 || retry.env.Recipients[0].Email != "lost@y.example" {
		t.Fatalf("retry recipients = %v", retry.env.Recipients)
	}
}

// TestWorkerMissingResultTempFails: a normal return that omits a
// recipient's result temp-fails that recipient with a synthetic reply
// instead of leaving it stuck - the merge guarantees one verdict per
// attempted recipient whatever the Submitter reports.
func TestWorkerMissingResultTempFails(t *testing.T) {
	ts, db, store, w, fake, _ := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	fake.respond = func(env SubmissionEnvelope) ([]RecipientResult, error) {
		return []RecipientResult{{"ok@x.example", Accepted, "250 2.1.5 ok"}}, nil
	}
	id := submitEnvelope(t, ts, identityId, emailId, `{
		"mailFrom":{"email":"john@example.com"},
		"rcptTo":[{"email":"ok@x.example"},{"email":"forgotten@y.example"}]}`)

	w.sweep(context.Background(), 0)
	ds := recDeliveryStatus(t, subRecord(t, db, id))
	if ds["ok@x.example"].Delivered != "unknown" {
		t.Fatalf("accepted recipient = %+v", ds["ok@x.example"])
	}
	if ds["forgotten@y.example"].Delivered != "queued" ||
		ds["forgotten@y.example"].SmtpReply != "451 4.4.1 no result reported for recipient" {
		t.Fatalf("omitted recipient = %+v", ds["forgotten@y.example"])
	}
}

// TestWorkerBackoffGiveUp: the schedule walks 1m, 2m, then plateaus at
// 4m, and once the submission's age passes GiveUpAfter the recipients
// are abandoned with a synthetic permanent failure. All-tempfail keeps
// undoStatus pending (cancelable) until then.
func TestWorkerBackoffGiveUp(t *testing.T) {
	cfg := SubmissionWorkerConfig{
		RetrySchedule: []time.Duration{time.Minute, 2 * time.Minute},
		RetryPlateau:  4 * time.Minute,
		GiveUpAfter:   10 * time.Minute,
	}
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), cfg)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	fake.respond = func(env SubmissionEnvelope) ([]RecipientResult, error) {
		return nil, fmt.Errorf("connection refused") // could-not-attempt
	}
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)

	wantBackoffs := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 4 * time.Minute}
	for i, want := range wantBackoffs {
		w.sweep(context.Background(), 0)
		if fake.callCount() != i+1 {
			t.Fatalf("attempt %d: submitter called %d times", i+1, fake.callCount())
		}
		rec := subRecord(t, db, id)
		if recString(rec, "undoStatus") != undoPending {
			t.Fatalf("attempt %d: all-tempfail should stay pending, got %s", i+1, rec["undoStatus"])
		}
		st := recDeliveryStatus(t, rec)["jane@remote.example"]
		if st.Delivered != "queued" || !strings.HasPrefix(st.SmtpReply, "451 4.4.1") {
			t.Fatalf("attempt %d: status = %+v", i+1, st)
		}
		next, err := parseUTCDateValue(rec["nextAttemptAt"])
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		if d := next.Sub(clock.now()); d < want-5*time.Second || d > want+5*time.Second {
			t.Fatalf("attempt %d: backoff %v, want %v", i+1, d, want)
		}
		clock.advance(want + time.Second)
	}
	// Age is now past GiveUpAfter: the next attempt abandons.
	w.sweep(context.Background(), 0)
	rec := subRecord(t, db, id)
	st := recDeliveryStatus(t, rec)["jane@remote.example"]
	if st.Delivered != "no" || !strings.HasPrefix(st.SmtpReply, "554 5.4.7") {
		t.Fatalf("give-up status = %+v", st)
	}
	if recString(rec, "undoStatus") != undoFinal {
		t.Errorf("undoStatus = %s", rec["undoStatus"])
	}
	if _, has := rec["nextAttemptAt"]; has {
		t.Error("nextAttemptAt survived give-up")
	}
}

// TestWorkerFutureReleaseAndCancel: a held submission is invisible to
// passes until sendAt, and canceling removes it from the queue so the
// release moment sends nothing.
func TestWorkerFutureReleaseAndCancel(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId, `{
		"mailFrom":{"email":"john@example.com","parameters":{"HOLDFOR":"3600"}},
		"rcptTo":[{"email":"jane@remote.example"}]}`)

	w.sweep(context.Background(), 0)
	if fake.callCount() != 0 {
		t.Fatal("held submission transmitted early")
	}

	r := callSub(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"undoStatus":"canceled"}}}`, testAccount, id), "0"))
	if _, ok := methodArgs(t, r, 0, "EmailSubmission/set")["updated"].(map[string]any)[id]; !ok {
		t.Fatalf("cancel failed: %v", r.MethodResponses[0].Args)
	}

	clock.advance(2 * time.Hour)
	if _, _, pending, _ := w.sweep(context.Background(), 0); pending {
		t.Error("canceled submission still reports due work")
	}
	if fake.callCount() != 0 {
		t.Fatal("canceled submission transmitted")
	}
	rec := subRecord(t, db, id)
	if recString(rec, "undoStatus") != undoCanceled {
		t.Errorf("undoStatus = %s", rec["undoStatus"])
	}
}

// TestWorkerClaimRecovery is the crash chaos case: a worker that claimed
// a record (parking its due index) then died mid-transmit blocks the
// record only until ClaimWindow passes, then exactly one reclaim sends
// it.
func TestWorkerClaimRecovery(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	ctx := context.Background()
	clock.advance(time.Second) // clear the record's creation-second boundary

	// A worker claims the record (parking its due index) then dies before
	// transmitting - simulated by claiming and never sending.
	if _, ok := w.claim(ctx, testAccount, jmap.Id(id)); !ok {
		t.Fatal("initial claim failed")
	}

	// Within the window the record is parked out of the due set: no send.
	w.sweep(ctx, 0)
	if fake.callCount() != 0 {
		t.Fatal("parked (live-claimed) record was sent early")
	}

	// Past the window it reappears as due and is reclaimed - once.
	clock.advance(16 * time.Minute)
	w.sweep(ctx, 0)
	w.sweep(ctx, 0)
	if fake.callCount() != 1 {
		t.Fatalf("reclaim sent %d times", fake.callCount())
	}
	rec := subRecord(t, db, id)
	if recString(rec, "undoStatus") != undoFinal {
		t.Errorf("undoStatus = %s", rec["undoStatus"])
	}
}

// TestWorkerClaimParksDueIndex is the chaos #1 fix: claiming a record
// parks its due index a full ClaimWindow ahead, so a live claim (this or
// another process) is never simultaneously "due". The claimed record
// leaves the due scan, and a no-progress pass reports a FUTURE next
// rather than a past one - the busy-loop precondition is gone.
func TestWorkerClaimParksDueIndex(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	ctx := context.Background()
	clock.advance(time.Second) // clear the record's creation-second boundary

	// A peer claims the record but has not sent it yet.
	if _, ok := w.claim(ctx, testAccount, jmap.Id(id)); !ok {
		t.Fatal("claim of a due record failed")
	}

	// The claimed record is no longer due: nextAttemptAt is parked ~one
	// ClaimWindow ahead, so the due-index scan returns nothing.
	rec := subRecord(t, db, id)
	next, err := parseUTCDateValue(rec["nextAttemptAt"])
	if err != nil {
		t.Fatal(err)
	}
	if d := next.Sub(clock.now()); d < 14*time.Minute || d > 16*time.Minute {
		t.Fatalf("parked due = %v ahead, want ~ClaimWindow (15m)", d)
	}
	ids, err := db.IdsWhereAtMost(ctx, testAccount, TypeEmailSubmission, "nextAttemptAt",
		mustJSON(clock.now().UTC().Format(time.RFC3339)), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("claimed record still appears due: %v", ids)
	}

	// A no-progress pass reports a FUTURE next (the claim expiry), never a
	// past time - so the Run loop sleeps instead of spinning.
	sent, due, err := w.ProcessDue(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 0 || fake.callCount() != 0 {
		t.Fatalf("no-progress pass sent %d (submitter saw %d)", sent, fake.callCount())
	}
	if due.IsZero() || !due.After(clock.now()) {
		t.Fatalf("no-progress next = %v, want a future time", due)
	}
}

// TestWorkerClaimTokenCollisionResistant is the token-nonce guard: two
// workers that claim the same record at the identical wall-clock instant
// (the backward-clock-step collision the timestamp alone cannot survive)
// still mint distinct tokens, so the superseded worker's finalize is
// dropped by the exact-match identity check.
func TestWorkerClaimTokenCollisionResistant(t *testing.T) {
	ts, db, store, wA, _, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	ctx := context.Background()

	// A second process's worker over the same store, its own nonce, the
	// same (frozen) clock.
	wB, err := NewSubmissionWorker(newSubmissionQueue(db, store), &fakeSubmitter{}, SubmissionWorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	wB.now = clock.now
	clock.advance(time.Second) // clear the record's creation-second boundary

	// A claims (parking the record).
	cA, ok := wA.claim(ctx, testAccount, jmap.Id(id))
	if !ok {
		t.Fatal("A's claim failed")
	}
	// The parked time "arrives" without moving the clock, so B's reclaim
	// shares A's exact timestamp - the collision the nonce must survive.
	if _, err := db.Update(ctx, testAccount, func(u *objectdb.Update) error {
		obj, err := u.Get(TypeEmailSubmission, jmap.Id(id))
		if err != nil {
			return err
		}
		obj["nextAttemptAt"] = mustJSON(clock.now().UTC().Format(time.RFC3339))
		return u.Put(TypeEmailSubmission, jmap.Id(id), obj)
	}); err != nil {
		t.Fatal(err)
	}
	cB, ok := wB.claim(ctx, testAccount, jmap.Id(id))
	if !ok {
		t.Fatal("B's reclaim failed")
	}

	// Identical timestamp prefixes, distinct whole tokens - the nonce is
	// carrying the uniqueness.
	tsA := strings.SplitN(cA.stamp, "|", 2)
	tsB := strings.SplitN(cB.stamp, "|", 2)
	if len(tsA) != 2 || len(tsB) != 2 {
		t.Fatalf("token missing nonce: %q / %q", cA.stamp, cB.stamp)
	}
	if tsA[0] != tsB[0] {
		t.Fatalf("test setup: timestamps should collide, got %q vs %q", tsA[0], tsB[0])
	}
	if cA.stamp == cB.stamp {
		t.Fatal("colliding claim tokens: the nonce failed to make them distinct")
	}

	// A's superseded leg tries to finalize with its own token; it must be
	// dropped and B's claim left intact.
	wA.finalize(ctx, testAccount, jmap.Id(id), cA.stamp,
		[]RecipientResult{{Recipient: "jane@remote.example", Outcome: Accepted, Reply: "250 stolen"}})
	rec := subRecord(t, db, id)
	if recDeliveryStatus(t, rec)["jane@remote.example"].Delivered != "queued" {
		t.Fatal("A's superseded finalize mutated the record")
	}
	if recString(rec, "claimedAt") != cB.stamp {
		t.Fatal("A's superseded finalize touched B's claim")
	}

	// B's own token finalizes normally.
	wB.finalize(ctx, testAccount, jmap.Id(id), cB.stamp,
		[]RecipientResult{{Recipient: "jane@remote.example", Outcome: Accepted, Reply: "250 2.0.0 ok"}})
	if recDeliveryStatus(t, subRecord(t, db, id))["jane@remote.example"].Delivered != "unknown" {
		t.Fatal("B's valid finalize was dropped")
	}
}

// TestWorkerStaleFinalizeDropped: a finalize whose claim stamp was
// superseded must not touch the record - the double-finalize guard.
func TestWorkerStaleFinalizeDropped(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	_ = fake

	// The record carries a reclaimer's stamp; this worker's older leg
	// tries to finalize with its own.
	current := clock.now().UTC().Format(time.RFC3339Nano)
	_, err := db.Update(context.Background(), testAccount, func(u *objectdb.Update) error {
		obj, err := u.Get(TypeEmailSubmission, jmap.Id(id))
		if err != nil {
			return err
		}
		obj["claimedAt"] = mustJSON(current)
		return u.Put(TypeEmailSubmission, jmap.Id(id), obj)
	})
	if err != nil {
		t.Fatal(err)
	}
	stale := clock.now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	w.finalize(context.Background(), testAccount, jmap.Id(id), stale,
		[]RecipientResult{{Recipient: "jane@remote.example", Outcome: Accepted, Reply: "250 stolen"}})

	rec := subRecord(t, db, id)
	if recDeliveryStatus(t, rec)["jane@remote.example"].Delivered != "queued" {
		t.Fatal("stale finalize mutated the record")
	}
	if recString(rec, "undoStatus") != undoPending {
		t.Fatal("stale finalize changed undoStatus")
	}
	if recString(rec, "claimedAt") != current {
		t.Fatal("stale finalize touched the claim")
	}
}

// TestWorkerRestartResume: a fresh worker over the same store rebuilds
// the queue from durable state alone - the due record sends, the held
// one waits.
func TestWorkerRestartResume(t *testing.T) {
	ts, db, store, w1, fake1, _ := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	dueEmail := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	heldEmail := putEmail(t, db, store, sendableMsg(map[string]string{"X-N": "2"}), map[string]bool{drafts: true}, nil)
	dueId := submitEnvelope(t, ts, identityId, dueEmail,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	heldId := submitEnvelope(t, ts, identityId, heldEmail, `{
		"mailFrom":{"email":"john@example.com","parameters":{"HOLDFOR":"3600"}},
		"rcptTo":[{"email":"jane@remote.example"}]}`)
	_, _ = w1, fake1 // the "crashed" worker never ran

	fake2 := &fakeSubmitter{}
	w2, err := NewSubmissionWorker(newSubmissionQueue(db, store), fake2, SubmissionWorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	clock2 := &testClock{t: time.Now()}
	w2.now = clock2.now
	ctx := context.Background()
	if err := w2.healTags(ctx); err != nil {
		t.Fatal(err)
	}
	tagged, err := db.TaggedAccounts(ctx, submissionQueueTag)
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged) != 1 || tagged[0] != testAccount {
		t.Fatalf("startup pass missed the account: %v", tagged)
	}
	_, wake, pending, err := w2.sweep(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fake2.callCount() != 1 {
		t.Fatalf("restarted worker sent %d times", fake2.callCount())
	}
	if recString(subRecord(t, db, dueId), "undoStatus") != undoFinal {
		t.Error("due submission not sent after restart")
	}
	if recString(subRecord(t, db, heldId), "undoStatus") != undoPending {
		t.Error("held submission sent early after restart")
	}
	if !pending {
		t.Fatal("held submission dropped from the queue view")
	}
	if d := wake.Sub(clock2.now()); d <= 0 {
		t.Errorf("held wake in the past: %v", d)
	}
}

// TestWorkerRingLeavesBellToken: a ring always leaves exactly one bell
// token for the run loop to consume - the wake carries no payload (the
// sweep reads what and when from durable state), and rings coalesce
// instead of blocking or accumulating.
func TestWorkerRingLeavesBellToken(t *testing.T) {
	_, _, _, w, _, _ := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	w.q.ring()
	w.q.ring() // a second ring coalesces, never blocks
	select {
	case <-w.q.bell:
	default:
		t.Fatal("ring left no bell token")
	}
	select {
	case <-w.q.bell:
		t.Fatal("rings did not coalesce into one token")
	default:
	}
}

// TestWorkerCrossProcessDiscovery: the database is the coordination
// point - a submission committed by another process (its ring landed in
// a bell this worker never sees, and no Notifier is attached) is picked
// up by the sweep alone, and the drained account's tag is cleared
// afterwards. This is the reconciliation floor working with every
// accelerator absent.
func TestWorkerCrossProcessDiscovery(t *testing.T) {
	ts, db, store, w1, fake1, _ := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	// "Process B": its own queue view over the same store, started
	// before the work exists - its startup scan sees nothing.
	fakeB := &fakeSubmitter{}
	wB, err := NewSubmissionWorker(newSubmissionQueue(db, store), fakeB, SubmissionWorkerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	clockB := &testClock{t: time.Now()}
	wB.now = clockB.now
	ctx := context.Background()
	if err := wB.healTags(ctx); err != nil {
		t.Fatal(err)
	}

	// Process A commits a submission; only A's bell rings.
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)
	_, _ = w1, fake1 // process A's worker never runs

	// The tag worklist carries the account...
	tagged, err := db.TaggedAccounts(ctx, submissionQueueTag)
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged) != 1 || tagged[0] != testAccount {
		t.Fatalf("queue tag = %v", tagged)
	}
	clockB.advance(time.Second) // clear the record's creation-second boundary
	// ...so B's sweep discovers and sends it.
	wB.sweep(ctx, 0)
	if fakeB.callCount() != 1 {
		t.Fatalf("process B sent %d times", fakeB.callCount())
	}
	if recString(subRecord(t, db, id), "undoStatus") != undoFinal {
		t.Error("discovered submission not finalized")
	}
	// The next sweep confirms the drain under the lease and clears the
	// tag.
	wB.sweep(ctx, 0)
	tagged, err = db.TaggedAccounts(ctx, submissionQueueTag)
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged) != 0 {
		t.Fatalf("drained account still tagged: %v", tagged)
	}
}

// TestWorkerProcessDue: the exported manual drain. It discovers work
// from durable state alone (the worker retains nothing between calls
// by construction), honors its limit across calls, and reports the
// remaining earliest due time, so a queue flush or a pacer needs no
// Run loop.
func TestWorkerProcessDue(t *testing.T) {
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		submitEnvelope(t, ts, identityId, emailId, fmt.Sprintf(
			`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"r%d@remote.example"}]}`, i))
	}
	clock.advance(time.Second)

	sent, next, err := w.ProcessDue(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 2 || fake.callCount() != 2 {
		t.Fatalf("limited drain sent %d (submitter saw %d), want 2", sent, fake.callCount())
	}
	if next.IsZero() || next.After(clock.now()) {
		t.Fatalf("next = %v, want a still-due time", next)
	}

	sent, next, err = w.ProcessDue(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sent != 1 || fake.callCount() != 3 {
		t.Fatalf("unlimited drain sent %d (submitter saw %d), want 1", sent, fake.callCount())
	}
	if !next.IsZero() {
		t.Fatalf("drained queue reports next = %v", next)
	}
	// Nothing left: another drain is a no-op that also clears the tag.
	sent, _, err = w.ProcessDue(ctx, 0)
	if err != nil || sent != 0 {
		t.Fatalf("idle drain sent %d, err %v", sent, err)
	}
	tagged, err := db.TaggedAccounts(ctx, submissionQueueTag)
	if err != nil {
		t.Fatal(err)
	}
	if len(tagged) != 0 {
		t.Fatalf("drained account still tagged: %v", tagged)
	}
}

// TestWorkerMinAttemptsFloor: a submission already past GiveUpAfter
// (the worker was down) still gets MinAttempts real attempts before
// abandonment - an outage must not convert a transient smarthost
// failure into an instant bounce.
func TestWorkerMinAttemptsFloor(t *testing.T) {
	cfg := SubmissionWorkerConfig{
		RetrySchedule: []time.Duration{time.Minute},
		RetryPlateau:  time.Minute,
		GiveUpAfter:   5 * time.Minute,
		MinAttempts:   3,
	}
	ts, db, store, w, fake, clock := newWorkerServer(t, DefaultSubmissionLimits(), cfg)
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)
	fake.respond = func(env SubmissionEnvelope) ([]RecipientResult, error) {
		return nil, fmt.Errorf("connection refused")
	}
	id := submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)

	// The worker was down well past GiveUpAfter.
	clock.advance(time.Hour)

	// Attempts 1 and 2: already stale, but under the floor - retried.
	for i := 1; i <= 2; i++ {
		w.sweep(context.Background(), 0)
		if fake.callCount() != i {
			t.Fatalf("attempt %d: submitter called %d times", i, fake.callCount())
		}
		st := recDeliveryStatus(t, subRecord(t, db, id))["jane@remote.example"]
		if st.Delivered != "queued" {
			t.Fatalf("attempt %d: abandoned under the MinAttempts floor: %+v", i, st)
		}
		clock.advance(2 * time.Minute)
	}
	// Attempt 3 reaches the floor: now the age rule abandons.
	w.sweep(context.Background(), 0)
	rec := subRecord(t, db, id)
	st := recDeliveryStatus(t, rec)["jane@remote.example"]
	if st.Delivered != "no" || !strings.HasPrefix(st.SmtpReply, "554 5.4.7") {
		t.Fatalf("give-up status = %+v", st)
	}
	if recString(rec, "undoStatus") != undoFinal {
		t.Errorf("undoStatus = %s", rec["undoStatus"])
	}
}

// TestWorkerEmptyRetryScheduleUsesDefault: an explicit empty RetrySchedule
// is treated as unset, not as "plateau only" (chaos finding #5), so the
// first retry uses the default fast backoff rather than the 8h plateau.
func TestWorkerEmptyRetryScheduleUsesDefault(t *testing.T) {
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(memory.New())
	w, err := NewSubmissionWorker(newSubmissionQueue(db, store), &fakeSubmitter{},
		SubmissionWorkerConfig{RetrySchedule: []time.Duration{}})
	if err != nil {
		t.Fatal(err)
	}
	w.rand = func() float64 { return 0.5 } // pin jitter to the exact schedule
	if got := w.backoff(1); got != time.Minute {
		t.Errorf("first backoff with empty schedule = %v, want the 1m default (not the plateau)", got)
	}
}

// TestWorkerBackoffJitterBounds: the retry delay spreads by exactly
// +-backoffJitter around the schedule value - r=0 and r=1 land on the
// bounds (within float truncation), the plateau jitters the same way,
// and r=0.5 collapses to the EXACT schedule value, which is what every
// pinned-seam timing assertion in this file relies on.
func TestWorkerBackoffJitterBounds(t *testing.T) {
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(memory.New())
	w, err := NewSubmissionWorker(newSubmissionQueue(db, store), &fakeSubmitter{},
		SubmissionWorkerConfig{RetrySchedule: []time.Duration{time.Minute}, RetryPlateau: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	w.rand = func() float64 { return 0.5 }
	if got := w.backoff(1); got != time.Minute {
		t.Errorf("midpoint backoff = %v, want exactly 1m", got)
	}
	approx := func(got, want time.Duration) bool {
		d := got - want
		return d > -time.Microsecond && d < time.Microsecond
	}
	cases := []struct {
		r        float64
		attempts uint64
		want     time.Duration
	}{
		{0.0, 1, 48 * time.Second}, // schedule lower bound: 1m * 0.8
		{1.0, 1, 72 * time.Second}, // schedule upper bound: 1m * 1.2
		{0.0, 2, 8 * time.Minute},  // plateau lower bound
		{1.0, 2, 12 * time.Minute}, // plateau upper bound
	}
	for _, c := range cases {
		w.rand = func() float64 { return c.r }
		if got := w.backoff(c.attempts); !approx(got, c.want) {
			t.Errorf("backoff(attempts=%d, r=%v) = %v, want ~%v", c.attempts, c.r, got, c.want)
		}
	}
}

// TestWorkerRunLoop: the real loop end to end on the real clock - start
// empty (waiting on the bell or the next scheduled scan), submit,
// observe the transmission, then shut down via ctx.
func TestWorkerRunLoop(t *testing.T) {
	ts, db, store, w, fake, _ := newWorkerServer(t, DefaultSubmissionLimits(), SubmissionWorkerConfig{})
	w.now = time.Now // real clock for the live loop
	drafts := createMailbox(t, ts, `{"name":"Drafts"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	emailId := putEmail(t, db, store, sendableMsg(nil), map[string]bool{drafts: true}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	submitEnvelope(t, ts, identityId, emailId,
		`{"mailFrom":{"email":"john@example.com"},"rcptTo":[{"email":"jane@remote.example"}]}`)

	deadline := time.After(5 * time.Second)
	for fake.callCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("worker never transmitted")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on ctx cancel")
	}
}
