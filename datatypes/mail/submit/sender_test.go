package submit

// Sender tests: the server-side send seam - one Email and one
// EmailSubmission written in one commit, the role mailbox the Email is
// filed in, the hooks a caller runs inside that commit, and the injected
// clock the records are stamped from.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
	mailrecord "github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// senderRaw is the message a send stores: a normal outgoing message from
// the account's own address.
const senderRaw = "From: John <john@example.com>\r\n" +
	"To: Visitor <visitor@remote.example>\r\n" +
	"Subject: Notice\r\n" +
	"Date: Fri, 17 Jul 2026 10:00:00 +0000\r\n" +
	"Message-ID: <n1@example.com>\r\n" +
	"\r\nThe message body.\r\n"

// senderOutgoing is what every test here sends: one recipient with a
// parameter, filed seen in the sent mailbox.
func senderOutgoing(identityId string) Outgoing {
	return Outgoing{
		IdentityId: jmap.Id(identityId),
		Envelope: subEnvelope{
			MailFrom: &subAddress{Email: "john@example.com"},
			RcptTo: []subAddress{{Email: "visitor@remote.example", Parameters: map[string]*string{
				"NOTIFY": ptrString("NEVER"),
			}}},
		},
		MailboxRole: "sent",
		Keywords:    json.RawMessage(`{"$seen":true}`),
	}
}

// drainBell empties the queue bell (Register rings it
// unconditionally) so a ring by the send under test is observable.
func drainBell(q *Queue) {
	select {
	case <-q.bell:
	default:
	}
}

func bellRang(q *Queue) bool {
	select {
	case <-q.bell:
		return true
	default:
		return false
	}
}

func emailCount(t *testing.T, db *objectdb.DB) int {
	t.Helper()
	ids, err := db.AllIds(context.Background(), testAccount, mailrecord.TypeEmail, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(ids)
}

// TestSenderSendWritesEmailAndSubmission: one call stores the message,
// files the Email in the role's mailbox with the caller's keywords, and
// queues the EmailSubmission that carries it (RFC 8621 sections 4 and 7),
// tagging the account and ringing the queue.
func TestSenderSendWritesEmailAndSubmission(t *testing.T) {
	ts, db, _, q, _, _ := newSenderServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	sent := createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	drainBell(q)

	now := time.Date(2026, 7, 17, 10, 30, 0, 0, time.UTC)
	res, err := q.Sender().Send(context.Background(), testAccount, strings.NewReader(senderRaw),
		senderOutgoing(identityId), SendOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.EmailId == "" || res.SubmissionId == "" || res.ThreadId == "" || res.BlobId == "" {
		t.Fatalf("incomplete result: %+v", res)
	}
	if n := emailCount(t, db); n != 1 {
		t.Fatalf("emails = %d, want 1", n)
	}
	if n := submissionCount(t, db); n != 1 {
		t.Fatalf("submissions = %d, want 1", n)
	}

	ctx := context.Background()
	email, err := db.Get(ctx, testAccount, mailrecord.TypeEmail, res.EmailId)
	if err != nil {
		t.Fatal(err)
	}
	if boxes := emailstore.MailboxIdsOf(email); len(boxes) != 1 || !boxes[jmap.Id(sent)] {
		t.Fatalf("mailboxIds = %v, want only the sent mailbox %s", boxes, sent)
	}
	if kw := emailstore.ObjectKeys(email["keywords"]); len(kw) != 1 || !kw["$seen"] {
		t.Fatalf("keywords = %v, want $seen only", kw)
	}
	if id, _ := decodeString(email["blobId"]); jmap.Id(id) != res.BlobId {
		t.Fatalf("email blobId = %q, want %q", id, res.BlobId)
	}
	if id, _ := decodeString(email["threadId"]); jmap.Id(id) != res.ThreadId {
		t.Fatalf("email threadId = %q, want %q", id, res.ThreadId)
	}
	var size uint64
	json.Unmarshal(email["size"], &size)
	if size != uint64(len(senderRaw)) {
		t.Fatalf("size = %d, want %d", size, len(senderRaw))
	}

	sub, err := db.Get(ctx, testAccount, mailrecord.TypeEmailSubmission, res.SubmissionId)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := decodeString(sub["identityId"]); id != identityId {
		t.Fatalf("submission identityId = %q, want %q", id, identityId)
	}
	if id, _ := decodeString(sub["emailId"]); jmap.Id(id) != res.EmailId {
		t.Fatalf("submission emailId = %q, want %q", id, res.EmailId)
	}
	if id, _ := decodeString(sub["threadId"]); jmap.Id(id) != res.ThreadId {
		t.Fatalf("submission threadId = %q, want %q", id, res.ThreadId)
	}
	if id, _ := decodeString(sub["blobId"]); jmap.Id(id) != res.BlobId {
		t.Fatalf("submission blobId = %q, want %q", id, res.BlobId)
	}
	if s, _ := decodeString(sub["undoStatus"]); s != undoPending {
		t.Fatalf("undoStatus = %q, want %q", s, undoPending)
	}
	if s, _ := decodeString(sub["messageId"]); s != "n1@example.com" {
		t.Fatalf("messageId = %q", s)
	}
	var env subEnvelope
	if err := json.Unmarshal(sub["envelope"], &env); err != nil {
		t.Fatal(err)
	}
	if env.MailFrom == nil || env.MailFrom.Email != "john@example.com" {
		t.Fatalf("envelope mailFrom = %+v", env.MailFrom)
	}
	if len(env.RcptTo) != 1 || env.RcptTo[0].Email != "visitor@remote.example" {
		t.Fatalf("envelope rcptTo = %+v", env.RcptTo)
	}
	if v := env.RcptTo[0].Parameters["NOTIFY"]; v == nil || *v != "NEVER" {
		t.Fatalf("envelope NOTIFY = %v", v)
	}
	var ds map[string]deliveryStatusObj
	if err := json.Unmarshal(sub["deliveryStatus"], &ds); err != nil {
		t.Fatal(err)
	}
	if got := ds["visitor@remote.example"]; got.Delivered != "queued" || got.Displayed != "unknown" {
		t.Fatalf("deliveryStatus = %+v", ds)
	}

	// The account is on the worker's worklist and the bell rang, so a
	// worker in any process finds the new work.
	accts, err := db.TaggedAccounts(ctx, submissionQueueTag)
	if err != nil || len(accts) != 1 || accts[0] != testAccount {
		t.Fatalf("tagged accounts = %v %v", accts, err)
	}
	if !bellRang(q) {
		t.Fatal("queue was not rung")
	}
}

// TestSenderSendNoRoleMailbox: with no mailbox holding the requested
// role the send fails with ErrNoRoleMailbox, nothing is written, and the
// finalized message blob is left unreferenced for the blob sweep - the
// same reclamation any abandoned upload gets (RFC 8620 section 6).
func TestSenderSendNoRoleMailbox(t *testing.T) {
	ts, db, store, q, _, _ := newSenderServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	identityId := createIdentity(t, ts, "john@example.com")
	drainBell(q)

	now := time.Date(2026, 7, 17, 10, 30, 0, 0, time.UTC)
	res, err := q.Sender().Send(context.Background(), testAccount, strings.NewReader(senderRaw),
		senderOutgoing(identityId), SendOptions{Now: func() time.Time { return now }})
	if !errors.Is(err, ErrNoRoleMailbox) {
		t.Fatalf("err = %v, want ErrNoRoleMailbox", err)
	}
	if res != (SendResult{}) {
		t.Fatalf("result on failure = %+v, want zero", res)
	}
	assertNothingSent(t, db, q)

	deleted, _, err := db.SweepBlobs(context.Background(), testAccount, store, now.Add(24*time.Hour), 0)
	if err != nil || len(deleted) != 1 {
		t.Fatalf("sweep = %v %v, want the one abandoned upload", deleted, err)
	}
	if ok, err := db.BlobReferenced(context.Background(), testAccount, deleted[0]); ok || err != nil {
		t.Fatalf("swept blob was referenced: %v %v", ok, err)
	}
}

// assertNothingSent asserts an aborted send left no record and no ring.
func assertNothingSent(t *testing.T, db *objectdb.DB, q *Queue) {
	t.Helper()
	if n := emailCount(t, db); n != 0 {
		t.Fatalf("emails = %d, want 0", n)
	}
	if n := submissionCount(t, db); n != 0 {
		t.Fatalf("submissions = %d, want 0", n)
	}
	if bellRang(q) {
		t.Fatal("queue was rung by a send that wrote nothing")
	}
}

// TestSenderBeforeHook: the Before hook sees the commit's update handle
// with nothing written yet; ErrSkipSend aborts the send as a refusal, any
// other error aborts it as a failure, and neither writes anything.
func TestSenderBeforeHook(t *testing.T) {
	hookErr := errors.New("caller precondition failed")
	for _, tc := range []struct {
		name string
		ret  error
		want error
	}{
		{"skip", ErrSkipSend, ErrSkipSend},
		{"error", hookErr, hookErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, db, _, q, _, _ := newSenderServer(t)
			createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
			sent := createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
			identityId := createIdentity(t, ts, "john@example.com")
			drainBell(q)

			now := time.Date(2026, 7, 17, 10, 30, 0, 0, time.UTC)
			ran := false
			opts := SendOptions{
				Now: func() time.Time { return now },
				Before: func(u *objectdb.Update, hookNow time.Time) error {
					ran = true
					if !hookNow.Equal(now) {
						t.Errorf("hook now = %v, want %v", hookNow, now)
					}
					ids, err := u.IdsWhereMember(mailrecord.TypeEmail, "mailboxIds", sent)
					if err != nil {
						t.Error(err)
					}
					if len(ids) != 0 {
						t.Errorf("Before ran after %d Emails were written", len(ids))
					}
					return tc.ret
				},
			}
			res, err := q.Sender().Send(context.Background(), testAccount, strings.NewReader(senderRaw),
				senderOutgoing(identityId), opts)
			if !ran {
				t.Fatal("Before hook did not run")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if res != (SendResult{}) {
				t.Fatalf("result = %+v, want zero", res)
			}
			assertNothingSent(t, db, q)
		})
	}
}

// TestSenderAfterHook: the After hook runs once the Email and the
// EmailSubmission exist, with the ids of both, and what it writes through
// the update handle commits with them - or, when it fails, nothing
// commits at all.
func TestSenderAfterHook(t *testing.T) {
	t.Run("writes in the same commit", func(t *testing.T) {
		ts, db, _, q, _, _ := newSenderServer(t)
		createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
		sent := createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
		identityId := createIdentity(t, ts, "john@example.com")
		drainBell(q)

		now := time.Date(2026, 7, 17, 10, 30, 0, 0, time.UTC)
		var seen SendResult
		opts := SendOptions{
			Now: func() time.Time { return now },
			After: func(u *objectdb.Update, r SendResult, hookNow time.Time) error {
				seen = r
				// Both records are visible to the hook.
				if _, err := u.Get(mailrecord.TypeEmail, r.EmailId); err != nil {
					t.Errorf("Email not visible to After: %v", err)
				}
				if _, err := u.Get(mailrecord.TypeEmailSubmission, r.SubmissionId); err != nil {
					t.Errorf("EmailSubmission not visible to After: %v", err)
				}
				ids, err := u.IdsWhereMember(mailrecord.TypeEmail, "mailboxIds", sent)
				if err != nil || len(ids) != 1 {
					t.Errorf("sent mailbox at After time = %v %v", ids, err)
				}
				_, err = u.Create(mailrecord.TypeVacationNotified, objectdb.Object{
					"sender": mailrecord.MustJSON("visitor@remote.example"),
					"sentAt": mailrecord.MustJSON(hookNow.UTC().Format(time.RFC3339)),
				})
				return err
			},
		}
		res, err := q.Sender().Send(context.Background(), testAccount, strings.NewReader(senderRaw),
			senderOutgoing(identityId), opts)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		if seen != res {
			t.Fatalf("After saw %+v, Send returned %+v", seen, res)
		}
		ids, err := db.AllIds(context.Background(), testAccount, mailrecord.TypeVacationNotified, 0)
		if err != nil || len(ids) != 1 {
			t.Fatalf("hook record = %v %v, want one committed with the send", ids, err)
		}
	})

	t.Run("error aborts the whole commit", func(t *testing.T) {
		ts, db, _, q, _, _ := newSenderServer(t)
		createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
		createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
		identityId := createIdentity(t, ts, "john@example.com")
		drainBell(q)

		hookErr := errors.New("ledger write failed")
		opts := SendOptions{After: func(u *objectdb.Update, r SendResult, hookNow time.Time) error {
			return hookErr
		}}
		res, err := q.Sender().Send(context.Background(), testAccount, strings.NewReader(senderRaw),
			senderOutgoing(identityId), opts)
		if !errors.Is(err, hookErr) {
			t.Fatalf("err = %v, want the hook's error", err)
		}
		if res != (SendResult{}) {
			t.Fatalf("result = %+v, want zero", res)
		}
		assertNothingSent(t, db, q)
	})
}

// TestSenderInjectedNow: the injected clock is what the records carry -
// the Email's internal date and the submission's sendAt and
// nextAttemptAt, which is when the worker considers it due.
func TestSenderInjectedNow(t *testing.T) {
	ts, db, _, q, _, _ := newSenderServer(t)
	createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	createMailbox(t, ts, `{"name":"Sent","role":"sent"}`)
	identityId := createIdentity(t, ts, "john@example.com")

	now := time.Date(2026, 7, 17, 10, 30, 0, 0, time.UTC)
	res, err := q.Sender().Send(context.Background(), testAccount, strings.NewReader(senderRaw),
		senderOutgoing(identityId), SendOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	want := now.UTC().Format(time.RFC3339)
	ctx := context.Background()
	sub, err := db.Get(ctx, testAccount, mailrecord.TypeEmailSubmission, res.SubmissionId)
	if err != nil {
		t.Fatal(err)
	}
	for _, prop := range []string{"sendAt", "nextAttemptAt"} {
		if s, _ := decodeString(sub[prop]); s != want {
			t.Errorf("submission %s = %q, want %q", prop, s, want)
		}
	}
	email, err := db.Get(ctx, testAccount, mailrecord.TypeEmail, res.EmailId)
	if err != nil {
		t.Fatal(err)
	}
	at, err := parseUTCDateValue(email["receivedAt"])
	if err != nil || !at.Equal(now) {
		t.Errorf("receivedAt = %v %v, want %v", at, err, now)
	}
}
