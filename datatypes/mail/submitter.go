package mail

// The Submitter socket: outbound relay of a finished message to a
// smarthost. It is the outbound dual of the inbound Resolver/Deliverer
// pipeline and deliberately a separate, one-method surface: the sending
// worker owns queueing, retry, and per-recipient bookkeeping; a
// Submitter only performs one transmission attempt and reports what the
// smarthost said. The reference implementation is SMTPRelay
// (smtprelay.go); hosts with other needs (OAuth submission services,
// pipes into a local MTA, test fakes) implement the interface.

import (
	"context"
	"io"
)

// SubmissionRecipient is one envelope recipient of an outbound message:
// the RCPT TO address plus any rcpt-parameters (RFC 5321 section 4.1.2)
// to send with it. Parameter values are in DECODED form - the RFC 8621
// section 7 Envelope stores them with any xtext encoding removed - and
// a nil value means the parameter takes no value. Applying xtext where
// the wire requires it (ORCPT, RFC 3461 section 4.2) is the Submitter's
// job.
type SubmissionRecipient struct {
	Email      string
	Parameters map[string]*string
}

// SubmissionEnvelope is the SMTP envelope for one transmission attempt:
// the MAIL FROM address with its mail-parameters (decoded, as above -
// ENVID per RFC 3461 section 4.4 is xtext-encoded by the Submitter),
// the recipients still to be attempted, and the size in bytes of the
// message as stored (an upper bound for the wire form: the caller
// strips Bcc before handing over the stream, which only shrinks it -
// usable for SIZE declarations, RFC 1870). FUTURERELEASE hold
// parameters never appear here: the create pipeline consumes and strips
// them, so the envelope is exactly what should be relayed.
type SubmissionEnvelope struct {
	MailFrom       string
	MailParameters map[string]*string
	Recipients     []SubmissionRecipient
	Size           int64
}

// RecipientResult is the fate of one recipient in one transmission
// attempt. Outcome reuses the delivery classification (Accepted -> 2xx,
// Rejected -> permanent 5xx, TempFailed -> transient 4xx); Reply is the
// SMTP reply to record as the recipient's smtpReply, flattened to one
// line per RFC 8621 section 7 (the RCPT TO reply, unless the message
// was rejected at the DATA stage, in which case the DATA reply).
type RecipientResult struct {
	Recipient string
	Outcome   Outcome
	Reply     string
}

// Submitter performs one transmission attempt to a smarthost. A normal
// return carries one result per envelope recipient - mixed outcomes are
// expected (some accepted, some rejected, some to retry). A non-nil
// error means the attempt ended abnormally (connection, TLS,
// authentication failure); results MAY still accompany it for the
// recipients whose fate is known, and the caller applies them exactly
// as on a normal return - an Accepted result reports an irrevocable
// relay (a 250 after end-of-data, RFC 5321 section 4.2.5) and that
// recipient is never sent again. A fan-out implementation that splits
// recipients across transactions reports the completed transactions'
// results alongside the error from the failed one. Recipients with no
// result - with or without an error - are treated as temporarily
// failed and retried, so a single-transaction implementation whose
// abnormal end accepts nobody correctly returns (nil, err). Submit
// must honor ctx cancellation and never retain msg after returning;
// msg is a single-use stream of the RFC 5322 message with Bcc already
// stripped by the caller (a server duty per RFC 8621 section 7.5,
// never the Submitter's).
type Submitter interface {
	Submit(ctx context.Context, env SubmissionEnvelope, msg io.Reader) ([]RecipientResult, error)
}
