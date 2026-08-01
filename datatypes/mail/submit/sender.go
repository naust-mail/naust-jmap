package submit

// The server-side send seam: one call that stores a message an account is
// sending, files it as an Email, and queues its EmailSubmission through
// the one outbound queue. Mail-driven senders (an automatic responder, a
// server-generated MDN) all need the same thing a client's send produces -
// a real Email in a role mailbox and a real EmailSubmission the worker
// relays - and they must produce it without reaching into the JMAP
// Email/set and EmailSubmission/set pipelines, which exist to validate a
// client's request.
//
// The shape follows what the store requires: the message blob is written
// outside the account lease, then the Email record, the EmailSubmission
// record, and whatever the caller writes alongside them commit under one
// hold of that lease (RFC 8620 section 6's upload record before the
// content, then the objects that reference it). A crash leaves either
// everything or nothing, and the queue is rung only once the commit is
// durable.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
	mailrecord "github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// Sender queues messages the server itself originates. Obtain one from
// (*Queue).Sender: the queue is what a submission is for, so it
// is what hands out the ability to make one.
type Sender struct {
	q *Queue
}

// Sender returns the send facility for this queue.
func (q *Queue) Sender() *Sender { return &Sender{q: q} }

// Outgoing is what to send and how to file it: the Identity the
// EmailSubmission is attributed to (RFC 8621 section 7), the SMTP
// envelope to relay with (section 7's Envelope, mailFrom and rcptTo
// including their parameters), the role of the mailbox the stored Email
// goes in (section 2's role, "sent" for a normal send), and the keywords
// that Email carries (section 4.1.1, the stored String[Boolean] form).
type Outgoing struct {
	IdentityId  jmap.Id
	Envelope    subEnvelope
	MailboxRole string
	Keywords    json.RawMessage
}

// NullPathEnvelope builds an Outgoing.Envelope with a null reverse-path
// (mailFrom "") and one recipient, its rcpt-parameters as given. subEnvelope
// is this package's internal wire-decoded shape, kept unexported; this is
// the seam a caller outside the create pipeline (the deliver package's
// vacation responder, RFC 3834's auto-reply) uses to build one without it.
func NullPathEnvelope(rcpt string, params map[string]*string) subEnvelope {
	return subEnvelope{
		MailFrom: &subAddress{Email: ""},
		RcptTo:   []subAddress{{Email: rcpt, Parameters: params}},
	}
}

// SendResult identifies what one Send wrote: the Email, its
// EmailSubmission, the Thread the Email was assigned to, and the blob
// holding the message.
type SendResult struct {
	EmailId      jmap.Id
	SubmissionId jmap.Id
	ThreadId     jmap.Id
	BlobId       jmap.Id
}

// SendOptions carries the caller's own work into the send commit and the
// clock the records are stamped from. The zero value is valid: no hooks,
// and time.Now.
//
// Before runs inside the commit before anything is written, so a caller
// that keeps its own ledger (a suppression history, a per-recipient
// budget) can read it under the account lease and refuse the send with
// ErrSkipSend. After runs inside the same commit once the Email and the
// EmailSubmission exist, so what it writes commits with them or not at
// all. Both receive the instant the send is stamped with, so a record a
// hook writes carries the same timestamp as the submission it belongs to.
type SendOptions struct {
	Before func(u *objectdb.Update, now time.Time) error
	After  func(u *objectdb.Update, res SendResult, now time.Time) error
	Now    func() time.Time // test seam; time.Now outside tests
}

// ErrSkipSend aborts a send for a non-error reason. A Before hook returns
// it to mean "this message must not be sent, and that is not a failure":
// nothing is written, nothing is queued, and Send returns it so the
// caller can tell a refusal from a success without inspecting the result.
// Test it with errors.Is; Send returns it unwrapped.
var ErrSkipSend = errors.New("mail: send skipped")

// ErrNoRoleMailbox reports that the account has no mailbox with the role
// the send asked for, so there is nowhere to file the message. A role is
// optional (RFC 8621 section 2), so this is an ordinary state of an
// account, not a corruption.
var ErrNoRoleMailbox = errors.New("mail: no mailbox with the requested role")

// Send stores msg for acct, files it as an Email in the Outgoing role's
// mailbox, and queues an EmailSubmission for it, then rings the queue.
//
// The caller is server code, not a JMAP client: Send applies neither
// SendPolicy nor the strict RFC 5322 validity check that
// EmailSubmission/set applies to a client's create (RFC 8621 section
// 7.5). A caller composing its own message is responsible for what it
// composes, and the identity is recorded as given - Send resolves nothing
// on the caller's behalf.
//
// The blob is written first, outside the account lease. The Email record,
// the EmailSubmission record, the queue tag, and both hooks then commit
// under one hold of that lease. On any error inside the commit nothing is
// written; the finalized blob is left unreferenced and the blob sweep
// reclaims it like any abandoned upload.
func (s *Sender) Send(ctx context.Context, acct jmap.Id, msg io.Reader, out Outgoing, opts SendOptions) (SendResult, error) {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()

	bw, err := s.q.store.Create(ctx, acct)
	if err != nil {
		return SendResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = bw.Abort()
		}
	}()
	// The stored Email record needs the headers and the two
	// content-derived fast fields (RFC 8621 section 4.1.4), so the capture
	// asks for the preview and nothing else. The blob store and the parser
	// see the same octets, from one pass over the caller's reader.
	c := parse.NewCapture()
	c.Preview = true
	counted := &countingReader{r: msg}
	parsed, err := parse.ParseMessage(io.TeeReader(counted, bw), c)
	if err != nil {
		return SendResult{}, err
	}

	blobID := bw.ID()
	var res SendResult
	// The upload is recorded against the account itself (RFC 8620 section
	// 6's uploader): the message was originated by the server on that
	// account's behalf, not received from any peer.
	finalized, _, err := s.q.db.FinalizeBlobUploadThenUpdate(ctx, acct, bw, string(acct), now,
		s.commit(blobID, counted.n, parsed, out, opts, now, &res))
	committed = finalized != ""
	if err != nil {
		return SendResult{}, err
	}
	s.q.ring()
	return res, nil
}

// commit is the lease-held half of one send: the caller's precondition,
// the Email in its role mailbox, the EmailSubmission that queues it, the
// account tag that puts the account on the worker's worklist, and the
// caller's own record - one commit.
func (s *Sender) commit(blobID jmap.Id, size int64, parsed *parse.Parsed, out Outgoing, opts SendOptions, now time.Time, res *SendResult) func(u *objectdb.Update) error {
	return func(u *objectdb.Update) error {
		if opts.Before != nil {
			if err := opts.Before(u, now); err != nil {
				return err
			}
		}
		mailbox, err := mailrecord.RoleMailboxId(u, out.MailboxRole)
		if err != nil {
			return err
		}
		if mailbox == "" {
			return ErrNoRoleMailbox
		}
		emailId, err := emailstore.InsertEmail(u, parsed, emailstore.EmailMeta{
			BlobID:     blobID,
			MailboxIds: emailstore.MailboxIdsJSON(map[jmap.Id]bool{mailbox: true}),
			Keywords:   out.Keywords,
			Size:       uint64(size),
			ReceivedAt: now,
		})
		if err != nil {
			return err
		}
		email, err := u.Get(mailrecord.TypeEmail, emailId)
		if err != nil {
			return err
		}

		// The queued submission is the same record EmailSubmission/set
		// creates (RFC 8621 section 7): due now, undo still possible, and
		// one synthetic deliveryStatus per recipient recording our own
		// acceptance into the queue - no SMTP exchange has happened yet.
		nowRaw := mailrecord.MustJSON(now.UTC().Format(time.RFC3339))
		ds := make(map[string]deliveryStatusObj, len(out.Envelope.RcptTo))
		for _, r := range out.Envelope.RcptTo {
			ds[r.Email] = deliveryStatusObj{
				SmtpReply: "250 2.0.0 message queued for delivery",
				Delivered: "queued",
				Displayed: "unknown",
			}
		}
		record := objectdb.Object{
			"identityId":     mailrecord.MustJSON(out.IdentityId),
			"emailId":        mailrecord.MustJSON(emailId),
			"threadId":       email["threadId"],
			"envelope":       mailrecord.MustJSON(out.Envelope),
			"sendAt":         nowRaw,
			"undoStatus":     mailrecord.MustJSON(undoPending),
			"deliveryStatus": mailrecord.MustJSON(ds),
			"attempts":       json.RawMessage(`0`),
			"nextAttemptAt":  nowRaw,
			"blobId":         mailrecord.MustJSON(blobID),
		}
		// The message's Message-ID, indexed for inbound report correlation
		// (RFC 8098 section 3.2.5's Original-Message-ID; the DSN fallback).
		if mid := parse.MessageID(parsed.Msg.Headers); mid != "" {
			record["messageId"] = mailrecord.MustJSON(mid)
		}
		submissionId, err := u.Create(mailrecord.TypeEmailSubmission, record)
		if err != nil {
			return err
		}
		// The account now has queued work: tag it onto the queue worklist in
		// the same commit, so a scanning worker (this process's or another's)
		// can never miss it.
		if err := u.SetAccountTag(submissionQueueTag); err != nil {
			return err
		}

		var threadId jmap.Id
		json.Unmarshal(email["threadId"], &threadId)
		sent := SendResult{EmailId: emailId, SubmissionId: submissionId, ThreadId: threadId, BlobId: blobID}
		if opts.After != nil {
			if err := opts.After(u, sent, now); err != nil {
				return err
			}
		}
		*res = sent
		return nil
	}
}

// countingReader passes a message through and counts its octets, which is
// the size the Email record stores.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
