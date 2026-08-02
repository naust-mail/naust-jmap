package deliver

// Delivery socket tests: the per-recipient verdict pipeline, the
// resolve-before-parse ordering, the size cap, transient failure -> tempfail,
// and the parse concurrency gate.

import (
	"context"
	"errors"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
)

// mapResolver is a fixed recipient->account map (the reference Resolver).
type mapResolver map[string]jmap.Id

func (m mapResolver) Resolve(_ context.Context, rcpt string) (jmap.Id, bool) {
	id, ok := m[rcpt]
	return id, ok
}

// captureSink records every event it is handed.
type captureSink struct{ events []Event }

func (c *captureSink) Record(_ context.Context, e []Event) {
	c.events = append(c.events, e...)
}

// trackReader reports whether it was ever read from.
type trackReader struct {
	r    io.Reader
	read bool
}

func (t *trackReader) Read(p []byte) (int, error) {
	t.read = true
	return t.r.Read(p)
}

// failStore wraps a blob.Store and fails every Create, to simulate a transient
// store fault on the delivery write path.
type failStore struct{ blob.Store }

func (failStore) Create(context.Context, jmap.Id) (blob.BlobWriter, error) {
	return nil, errors.New("disk on fire")
}

func deliveryEnv(from string, rcpts ...string) Envelope {
	return Envelope{MailFrom: from, Recipients: rcpts}
}

// TestDeliverHappyPath: a resolvable recipient gets mail.Accepted, the message
// lands in the inbox as an Email, the EmailDelivery state advances, and the
// event carries the blob id, size, and Message-ID.
func TestDeliverHappyPath(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	sink := &captureSink{}
	d := mustDeliverer(t, db, store, mapResolver{"jane@example.com": testAccount}, Config{MaxMessageSize: defaultMaxMessageSize, Sink: sink})

	evs := d.Deliver(context.Background(), deliveryEnv("joe@example.com", "jane@example.com"), strings.NewReader(simpleMessage))
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Outcome != mail.Accepted {
		t.Fatalf("outcome = %v (%q), want mail.Accepted", ev.Outcome, ev.Reason)
	}
	if ev.EmailId == "" || ev.Account != testAccount {
		t.Fatalf("event missing EmailId/Account: %+v", ev)
	}
	// The stored blob is the trace-stamped message (RFC 5321 4.4): the
	// Return-Path prefix followed by the original octets, and BlobId/Size
	// name exactly those stored octets.
	rc, _, err := store.Open(context.Background(), testAccount, ev.BlobId)
	if err != nil {
		t.Fatalf("open delivered blob: %v", err)
	}
	stored, _ := io.ReadAll(rc)
	rc.Close()
	want := "Return-Path: <joe@example.com>\r\n" + simpleMessage
	if string(stored) != want {
		t.Fatalf("stored blob = %q, want Return-Path prefix + message", stored)
	}
	if ev.BlobId != blob.IdFor(stored) {
		t.Fatalf("blob id = %q, want content address of stored octets", ev.BlobId)
	}
	if ev.Size != int64(len(want)) || ev.MessageId == "" {
		t.Fatalf("size/messageId wrong: %+v", ev)
	}

	// EmailDelivery advanced (section 1.5) and the Email is retrievable.
	if s, _ := db.TypeState(context.Background(), testAccount, mail.TypeEmailDelivery); s == "0" {
		t.Fatal("EmailDelivery state did not advance on delivery")
	}
	if obj := emailGet(t, ts, string(ev.EmailId), ""); obj["id"] != string(ev.EmailId) {
		t.Fatalf("delivered Email not gettable: %v", obj)
	}
	if len(sink.events) != 1 {
		t.Fatalf("sink recorded %d events, want 1", len(sink.events))
	}
}

// TestDeliverFanout: one message, two resolvable recipients and one unknown.
// The two are mail.Accepted (two Emails created), the unknown is mail.Rejected, and
// order is preserved.
func TestDeliverFanout(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	d := mustDeliverer(t, db, store, mapResolver{
		"a@example.com": testAccount,
		"b@example.com": testAccount,
	})

	evs := d.Deliver(context.Background(),
		deliveryEnv("s@example.com", "a@example.com", "nobody@example.com", "b@example.com"),
		strings.NewReader(simpleMessage))
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d", len(evs))
	}
	if evs[0].Outcome != mail.Accepted || evs[2].Outcome != mail.Accepted {
		t.Fatalf("resolvable recipients not accepted: %+v", evs)
	}
	if evs[1].Outcome != mail.Rejected || evs[1].Reason != "no such recipient" {
		t.Fatalf("unknown recipient not rejected: %+v", evs[1])
	}
	if evs[0].EmailId == evs[2].EmailId {
		t.Fatal("two deliveries produced the same Email id")
	}
}

// TestDeliverNoRecipientSkipsBody: when no recipient resolves, the body is
// never read - an unknown address cannot make the server buffer a message.
func TestDeliverNoRecipientSkipsBody(t *testing.T) {
	_, db, store := emailServer(t)
	d := mustDeliverer(t, db, store, mapResolver{})
	tr := &trackReader{r: strings.NewReader(simpleMessage)}

	evs := d.Deliver(context.Background(), deliveryEnv("s@example.com", "ghost@example.com"), tr)
	if len(evs) != 1 || evs[0].Outcome != mail.Rejected {
		t.Fatalf("want one rejection, got %+v", evs)
	}
	if tr.read {
		t.Fatal("body was read despite no resolvable recipient")
	}
}

// TestDeliverTooLarge: a body over the size cap is mail.Rejected and nothing is
// stored.
func TestDeliverTooLarge(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	d := mustDeliverer(t, db, store, mapResolver{"jane@example.com": testAccount}, Config{MaxMessageSize: 16})

	evs := d.Deliver(context.Background(), deliveryEnv("joe@example.com", "jane@example.com"), strings.NewReader(simpleMessage))
	if len(evs) != 1 || evs[0].Outcome != mail.Rejected || evs[0].Reason != "message too large" {
		t.Fatalf("want too-large rejection, got %+v", evs)
	}
	if s, _ := db.TypeState(context.Background(), testAccount, mail.TypeEmailDelivery); s != "0" {
		t.Fatal("EmailDelivery advanced despite oversize rejection")
	}
}

// TestDeliverTransientStoreFault: a store Put failure tempfails the recipient
// (so the MTA retries) rather than bouncing.
func TestDeliverTransientStoreFault(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	d := mustDeliverer(t, db, failStore{store}, mapResolver{"jane@example.com": testAccount})

	evs := d.Deliver(context.Background(), deliveryEnv("joe@example.com", "jane@example.com"), strings.NewReader(simpleMessage))
	if len(evs) != 1 || evs[0].Outcome != mail.TempFailed {
		t.Fatalf("want tempfail on store fault, got %+v", evs)
	}
}

// nthCreateFailStore fails the n-th call to Create, so a fan-out test can
// break exactly one recipient's blob write while the rest succeed.
type nthCreateFailStore struct {
	blob.Store
	mu   sync.Mutex
	seen int
	fail int
	err  error
}

func (s *nthCreateFailStore) Create(ctx context.Context, acct jmap.Id) (blob.BlobWriter, error) {
	s.mu.Lock()
	s.seen++
	cur := s.seen
	s.mu.Unlock()
	if cur == s.fail {
		return nil, s.err
	}
	return s.Store.Create(ctx, acct)
}

// TestDeliverFanoutCopyFault: with two recipients, a store fault while
// copying the message to the second recipient tempfails that recipient only.
// The first recipient stays mail.Accepted and its blob is intact and recorded, so
// one recipient's failed copy neither strands the message nor poisons the
// others (the per-recipient finalize is independent).
func TestDeliverFanoutCopyFault(t *testing.T) {
	ts, db, store := emailServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	// Create #1 is the first recipient's streaming writer (succeeds);
	// create #2 is the second recipient's copy (fails).
	fs := &nthCreateFailStore{Store: store, fail: 2, err: errors.New("copy target on fire")}
	d := mustDeliverer(t, db, fs, mapResolver{
		"a@example.com": testAccount,
		"b@example.com": testAccount,
	})

	evs := d.Deliver(context.Background(),
		deliveryEnv("s@example.com", "a@example.com", "b@example.com"),
		strings.NewReader(simpleMessage))
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	if evs[0].Outcome != mail.Accepted {
		t.Fatalf("first recipient not accepted: %+v", evs[0])
	}
	if evs[1].Outcome != mail.TempFailed {
		t.Fatalf("second recipient not tempfailed on copy fault: %+v", evs[1])
	}
	// The first recipient's message is fully stored and recorded: content
	// readable and an upload record present, so nothing leaked or half-wrote.
	ctx := context.Background()
	if _, _, err := store.Open(ctx, testAccount, evs[0].BlobId); err != nil {
		t.Fatalf("accepted recipient's content missing: %v", err)
	}
	if _, err := db.BlobUpload(ctx, testAccount, evs[0].BlobId); err != nil {
		t.Fatalf("accepted recipient's blob has no record: %v", err)
	}
}

// TestDeliverNoInbox: an account with no inbox tempfails (the MTA holds and
// retries while an operator fixes the account), it does not bounce.
func TestDeliverNoInbox(t *testing.T) {
	_, db, store := emailServer(t) // no inbox created
	d := mustDeliverer(t, db, store, mapResolver{"jane@example.com": testAccount})

	evs := d.Deliver(context.Background(), deliveryEnv("joe@example.com", "jane@example.com"), strings.NewReader(simpleMessage))
	if len(evs) != 1 || evs[0].Outcome != mail.TempFailed || evs[0].Reason != "no inbox mailbox" {
		t.Fatalf("want no-inbox tempfail, got %+v", evs)
	}
}
