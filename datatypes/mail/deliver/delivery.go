package deliver

// Message delivery: the transport-agnostic path by which mail arrives from
// outside and becomes an Email in the store. RFC 8621 has no delivery
// method - delivery is below the JMAP protocol - so this is a native socket,
// not a spec surface: an adapter (LMTP behind an MTA, or the HTTP ingest
// endpoint) hands an envelope and the raw message to Deliver, which returns
// one verdict per recipient. New Emails reach the store through the same
// insertEmail path Email/import uses (section 4.8 defaults for receivedAt
// and keywords), so threading, the Mailbox counters, and the EmailDelivery
// push state (section 1.5) are all maintained identically.
//
// Ordering is deliberate and hardens the unauthenticated surface: recipients
// are resolved first (an unknown recipient is rejected without the body ever
// being read), the body is read under a size cap, and MIME parsing runs
// under a global concurrency limit and BEFORE the per-account write lease -
// so a large or hostile message cannot stall other deliveries to the same
// account by parsing while holding its lock.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
	"github.com/naust-mail/naust-jmap/datatypes/mail/report"
	"github.com/naust-mail/naust-jmap/datatypes/mail/submit"
)

// Event is the record of delivering one message to one recipient.
// It is the single delivery data structure: the synchronous per-recipient
// verdict an adapter needs to answer on the wire is just its Outcome, and a
// Sink can persist the same value for audit or metrics. One event
// is produced per envelope recipient.
type Event struct {
	MailFrom   string       // envelope reverse-path; "" is the null sender <>
	Recipient  string       // the envelope forward-path this event reports
	Outcome    mail.Outcome // the verdict (see Outcome)
	Reason     string       // short human/log reason, for a bounce or a log line
	Account    jmap.Id      // resolved account, set when a recipient resolves
	EmailId    jmap.Id      // the created Email, set only when mail.Accepted (empty for a swallowed report, see Config.ReportIngestion)
	BlobId     jmap.Id      // content-addressed blob id of the raw message
	Size       int64        // octets of the raw message
	ReceivedAt time.Time    // server receive time (the Email's receivedAt)
	MessageId  string       // the message's Message-ID header, for correlation
}

// Envelope is the SMTP-level envelope (RFC 5321): the reverse-path sender
// and the forward-path recipients, carried out of band from the message
// headers by the transport.
type Envelope struct {
	MailFrom   string
	Recipients []string

	// Trace describes the transport hop for the Received stamp (RFC 5321
	// section 4.4), filled by the ingest adapter. LocalName is this server's
	// name (the BY clause) and gates the stamp: when it is empty - a caller
	// handing Deliver a message directly, with no hop to describe - only the
	// Return-Path line is prefixed. HeloName is the peer's LHLO/HELO claim
	// (untrusted; sanitized before use), PeerAddr its observed network
	// address, and Protocol the IANA-registered WITH value ("" omits WITH).
	HeloName  string
	PeerAddr  string
	Protocol  string
	LocalName string
}

// Resolver maps an envelope recipient to the local account that should
// receive it. It is deployment-specific (which addresses are local, and how
// they map to accounts), so it is a host-provided socket, like the auth
// socket - the delivery core never bakes in an addressing scheme. Returning
// ok=false rejects the recipient (no such mailbox) before the body is read.
type Resolver interface {
	Resolve(ctx context.Context, recipient string) (account jmap.Id, ok bool)
}

// Sink observes delivery outcomes. The default sink discards them;
// a host wanting durable delivery history, metrics, or bounce generation
// plugs its own. The structure (Event) is fixed so such a consumer
// retrofits nothing; only where the events go is left open.
type Sink interface {
	Record(ctx context.Context, events []Event)
}

type nopSink struct{}

func (nopSink) Record(context.Context, []Event) {}

// There is no cap on concurrent deliveries here. A delivery streams: the
// message is never held, so a parse in flight costs a buffer and not a message,
// and what an ingest must bound is therefore how many CONNECTIONS it is serving
// - which is the adapter's business (ServeLMTP, HTTPIngest), not the pipeline's.
// A cap here would also be the wrong shape: it would be held across a network
// read, so a few slow senders could hold every slot and stall delivery for
// everyone.

// defaultMaxMessageSize is the raw-message ceiling, in octets, DefaultConfig
// carries. It mirrors the JMAP maxSizeUpload default (core runtime,
// 50_000_000 octets) so a message delivered over LMTP and a blob imported via
// Email/import share one effective ceiling - an imported blob was uploaded
// through that same-capped endpoint. It mirrors a typical MTA
// message_size_limit too. An embedder that raises the session's maxSizeUpload
// should raise this to match.
const defaultMaxMessageSize = 50_000_000

// errNoInbox reports an account with no inbox role Mailbox: delivery cannot
// place the message, so the recipient tempfails (the MTA holds and retries
// while an operator fixes the account) rather than bouncing real mail.
var errNoInbox = errors.New("mail: account has no inbox mailbox")

// Deliverer ingests messages into the store. It is safe for concurrent use.
type Deliverer struct {
	db       *objectdb.DB
	store    blob.Store
	resolver Resolver
	sink     Sink
	maxSize  int64
	// reports enables DSN/MDN ingestion (Config.ReportIngestion): a
	// delivered report that correlates with an EmailSubmission updates it.
	// Requires RegisterEmailSubmission on the same db.
	reports bool
	// msgIDFallback additionally correlates DSNs by the returned content's
	// Message-ID (Config.MessageIDCorrelation).
	msgIDFallback bool
	// vacationQ, when set (Config.VacationQueue), enables the RFC 3834
	// auto-responder and names the queue its replies are submitted through.
	vacationQ *submit.Queue
}

// Config is a Deliverer's construction-time configuration. Values are
// used verbatim - start from DefaultConfig and override.
type Config struct {
	// MaxMessageSize caps the raw message size a Deliverer will accept,
	// in octets. Applied verbatim: a zero cap rejects every message
	// (logged once at construction). DefaultConfig sets the JMAP
	// maxSizeUpload default, 50_000_000.
	MaxMessageSize int64

	// Sink receives delivery outcomes. Nil discards them.
	Sink Sink

	// ReportIngestion makes delivery recognize inbound delivery status
	// notifications (RFC 3464) and message disposition notifications
	// (RFC 8098) and apply them to the EmailSubmission they report on:
	// deliveryStatus is updated and the report becomes fetchable through
	// dsnBlobIds/mdnBlobIds (RFC 8621 section 7). Only messages with the
	// null envelope sender are considered (both formats require it),
	// correlation is by the ENVID this server stamps on relay (the
	// submission id) or, for MDNs, the Original-Message-ID. Uncorrelated
	// reports are ordinary mail. Requires RegisterEmailSubmission on the
	// same db.
	ReportIngestion bool

	// MessageIDCorrelation additionally correlates DSNs whose ENVID was
	// lost in transit by the returned content's Message-ID. A report
	// matched this way is recorded and shown but can never finalize
	// deliveryStatus - a Message-ID is quotable by anyone who saw the
	// message, so it proves less than the envelope ENVID does (RFC 3464
	// section 4.1 warns DSNs are forgeable; the strong key stays the one
	// this server itself stamped).
	MessageIDCorrelation bool

	// VacationQueue, when set, enables the delivery-side RFC 3834
	// auto-responder, wired to the submission queue the replies are sent
	// through (the value submit.Register returned;
	// mail.RegisterVacationResponse must also be registered on the same
	// db, it owns the configuration the responder reads). One field,
	// because the responder IS the coupling of delivery to the send
	// queue - without a queue there is nowhere spec-compliant to put a
	// reply. New registers this responder's suppression ledger type when
	// the field is set.
	VacationQueue *submit.Queue
}

// DefaultConfig returns this package's default delivery configuration.
func DefaultConfig() Config {
	return Config{MaxMessageSize: defaultMaxMessageSize}
}

// New builds a Deliverer. db is where Emails land, store holds the
// raw-message blobs, and resolver maps recipients to accounts. The only
// failure mode is Config.VacationQueue: enabling the auto-responder
// registers its suppression ledger type on db, which can fail like any
// other RegisterType call.
func New(db *objectdb.DB, store blob.Store, resolver Resolver, cfg Config) (*Deliverer, error) {
	d := &Deliverer{
		db:            db,
		store:         store,
		resolver:      resolver,
		sink:          cfg.Sink,
		maxSize:       cfg.MaxMessageSize,
		reports:       cfg.ReportIngestion,
		msgIDFallback: cfg.MessageIDCorrelation,
		vacationQ:     cfg.VacationQueue,
	}
	if d.sink == nil {
		d.sink = nopSink{}
	}
	if d.maxSize <= 0 {
		slog.Warn("naust-jmap: deliver: Config.MaxMessageSize is not positive; every message will be rejected")
	}
	if d.vacationQ != nil {
		if err := db.RegisterType(vacationNotifiedType()); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// MaxMessageSize is the raw-message cap in octets, for an adapter that wants
// to advertise SIZE or reject early.
func (d *Deliverer) MaxMessageSize() int64 { return d.maxSize }

// Deliver ingests one message for the envelope's recipients and returns one
// Event per recipient, in the order given. It never returns an
// error: every failure mode is a per-recipient Outcome, so one bad recipient
// or a transient store fault cannot fail delivery to the others.
//
// A panic anywhere below this seam (parse, threading, insert) is recovered
// here so a hostile or malformed message cannot crash the process that
// co-hosts the JMAP server. This is the SHARED boundary: every ingest adapter
// (LMTP, HTTP, any future one) inherits crash-only isolation without repeating
// it. The verdict slice is owned here and starts at the safe default
// (mail.TempFailed is the zero value); deliver only ever upgrades an entry once a
// verdict is earned, so the recover keeps every verdict already decided and
// leaves the rest transient rather than rebuilding them. RFC 5321 has no
// "panic"; a local processing failure is a 4yz transient outcome (the MTA
// retries), which LMTP maps to 451.
func (d *Deliverer) Deliver(ctx context.Context, env Envelope, r io.Reader) (events []Event) {
	events, respond := d.deliverDeferred(ctx, env, r)
	if respond != nil {
		respond(ctx)
	}
	return events
}

// respondTimeout bounds the deferred auto-response work an ingest
// adapter runs after answering its peer, so a stalled backend loses the
// courtesy (RFC 3834 replies are best-effort) instead of pinning the
// session or handler.
const respondTimeout = 30 * time.Second

// deliverDeferred is Deliver with the auto-response work handed back
// instead of run: every verdict is settled and recorded when it returns,
// and respond (nil when there is none) carries the RFC 3834 replies, so
// an ingest adapter can answer its peer before any responder work runs.
// respond isolates its own failures - it can be run on any context,
// after any reply, and affect nothing about the delivery.
func (d *Deliverer) deliverDeferred(ctx context.Context, env Envelope, r io.Reader) (events []Event, respond func(context.Context)) {
	events = make([]Event, len(env.Recipients))
	for i, rcpt := range env.Recipients {
		events[i] = Event{MailFrom: env.MailFrom, Recipient: rcpt}
	}
	// The sink is fed at this boundary, not inside the pipeline, so it sees
	// every delivery's verdicts exactly once - a recovered panic included
	// (the deferred recover below runs before this earlier defer).
	defer func() { d.sink.Record(ctx, events) }()
	defer func() {
		if p := recover(); p != nil {
			slog.Error("naust-jmap delivery: recovered panic", "panic", p)
		}
	}()
	respond = d.deliver(ctx, env, r, events)
	return events, respond
}

// deliver is the delivery pipeline proper; deliverDeferred wraps it in the
// panic boundary above and owns the events slice, which deliver fills in
// place: each entry already carries its MailFrom and Recipient and the safe
// default verdict (mail.TempFailed). The returned respond closure is the
// pending auto-response work, nil when there is none.
func (d *Deliverer) deliver(ctx context.Context, env Envelope, r io.Reader, events []Event) (respond func(context.Context)) {
	// Resolve recipients first: an unknown recipient is rejected without
	// the body ever being read (no wasted parse, no way to make the server
	// buffer a message for an address it will refuse).
	type target struct {
		idx     int
		account jmap.Id
	}
	var targets []target
	for i, rcpt := range env.Recipients {
		acct, ok := d.resolver.Resolve(ctx, rcpt)
		if !ok {
			events[i].Outcome = mail.Rejected
			events[i].Reason = "no such recipient"
			continue
		}
		events[i].Account = acct
		targets = append(targets, target{idx: i, account: acct})
	}
	if len(targets) == 0 {
		return
	}

	// failAll gives every accepted recipient the same verdict: used when a
	// whole-message condition (too large, busy) sinks them together.
	failAll := func(o mail.Outcome, reason string) {
		for _, t := range targets {
			events[t.idx].Outcome = o
			events[t.idx].Reason = reason
		}
	}

	// The message is read once, and its octets go to the blob store and the
	// parser together: neither the raw message nor any decoded part of it is ever
	// held. So an ingest costs the server a buffer rather than a message, and the
	// size limit is enforced on the octets as they pass rather than on a buffer
	// that has already been filled with them.
	//
	// It is stored in the first accepted recipient's account; the others are
	// copies of it, streamed from there (below), which is the same work an
	// authenticated Blob/copy does.
	//
	// Delivery needs only what the stored Email record holds: the headers, and
	// the two content-derived fast fields (RFC 8621 section 4.1.4). So the capture
	// asks for the preview and nothing else - no per-part identity, no body values
	// - and the parser decodes only the leading octets of the message's text
	// parts. An attachment, however hostile, is never decoded on this
	// unauthenticated path.
	bw, err := d.store.Create(ctx, targets[0].account)
	if err != nil {
		failAll(mail.TempFailed, "temporary server error")
		slog.Error("naust-jmap delivery: blob create", "err", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = bw.Abort() // a rejected, failed, or panicking delivery stores nothing
		}
	}()

	// The trace prefix (Return-Path + Received, RFC 5321 section 4.4) is
	// streamed ahead of the message octets, so the blob store and the parser
	// both see the stamped message and neither is ever handed the unstamped
	// one. The prefix is server-built and small; the size cap governs the
	// network octets, which the cappedReader still meters.
	now := time.Now()
	prefix := tracePrefix(env, now)
	capped := &cappedReader{r: r, max: d.maxSize}
	c := parse.NewCapture()
	c.Preview = true
	// Reports are considered only from the null envelope sender, which both
	// report formats mandate (RFC 3464 section 2, RFC 8098 section 3), so
	// nothing else pays the capture and no non-report can correlate.
	c.Reports = d.reports && env.MailFrom == ""
	msg, err := parse.ParseMessage(io.TeeReader(io.MultiReader(strings.NewReader(prefix), capped), bw), c)
	switch {
	case errors.Is(err, errTooLarge):
		failAll(mail.Rejected, "message too large")
		return
	case err != nil:
		failAll(mail.TempFailed, "read error")
		slog.Error("naust-jmap delivery: read error", "err", err)
		return
	}
	size := int64(len(prefix)) + capped.read
	msgID := parse.MessageID(msg.Msg.Headers)
	var rep *report.Inbound
	if c.Reports {
		rep = extractReport(msg)
	}

	// Record the blob, publish its content, and create the Email in the first
	// recipient's account under one hold of its account lease: the record is
	// written before the content and the lease is held across both, so a crash
	// cannot strand published bytes and a concurrent blob sweep cannot delete
	// the content mid-finalize - and the Email commits in the same hold rather
	// than queueing for the lease a second time. The other recipients copy the
	// blob from here below, each finalized and delivered the same way in its
	// own account. An empty blobId back means the finalize itself failed and
	// nothing was published; a non-empty one with an error means only the
	// Email commit failed, which sinks the first recipient alone.
	blobID := bw.ID()
	var firstEmail jmap.Id
	finalized, _, firstErr := d.db.FinalizeBlobUploadThenUpdate(ctx, targets[0].account, bw,
		events[targets[0].idx].Recipient, now, d.inboxInsert(blobID, size, msg, now, rep, &firstEmail))
	if finalized == "" {
		failAll(mail.TempFailed, "temporary server error")
		slog.Error("naust-jmap delivery: blob finalize", "err", firstErr)
		return
	}
	committed = true

	for i, t := range targets {
		ev := &events[t.idx]
		ev.BlobId = blobID
		ev.Size = size
		ev.ReceivedAt = now
		ev.MessageId = msgID
		id, err := firstEmail, firstErr
		if i > 0 {
			id, err = d.copyAndDeliver(ctx, targets[0].account, t.account, blobID, ev.Recipient, size, msg, now, rep)
		}
		switch {
		case err == nil:
			ev.Outcome, ev.EmailId = mail.Accepted, id
		case errors.Is(err, errNoInbox):
			ev.Outcome, ev.Reason = mail.TempFailed, "no inbox mailbox"
		default:
			ev.Outcome, ev.Reason = mail.TempFailed, "temporary server error"
			// The untrusted recipient goes in its own structured field rather
			// than interpolated into the message, so no control character it
			// might carry can forge or escape a log line (defense in depth; the
			// LMTP ingress already rejects control-char addresses at parse
			// time).
			slog.Error("naust-jmap delivery", "recipient", ev.Recipient, "err", err)
		}
	}

	// Auto-responses run after every verdict is settled AND answered: a
	// reply is a consequence of a delivery, never a participant in it
	// (RFC 8621 section 8 delivers first, responds per RFC 3834 after),
	// so the work is handed back as a closure the adapter runs once its
	// peer has its replies. A swallowed report has no EmailId and gets no
	// reply - an ingested DSN/MDN is automatic mail anyway, refused by
	// the Auto-Submitted/null-sender gates.
	if d.vacationQ == nil {
		return nil
	}
	var due []target
	for _, t := range targets {
		if ev := events[t.idx]; ev.Outcome == mail.Accepted && ev.EmailId != "" {
			due = append(due, t)
		}
	}
	if len(due) == 0 {
		return nil
	}
	return func(ctx context.Context) {
		// The closure runs outside deliverDeferred's panic boundary;
		// a courtesy may fail any way it likes without taking the
		// adapter's session down.
		defer func() {
			if p := recover(); p != nil {
				slog.Error("naust-jmap vacation: recovered panic", "panic", p)
			}
		}()
		for _, t := range due {
			d.maybeVacationReply(ctx, t.account, events[t.idx].Recipient, env, msg, now)
		}
	}
}

// copyAndDeliver gives one further recipient's account its own copy of the
// message and creates its Email, the copy streamed from the account the
// message was stored in. The copy's finalize (record before publish, so it
// neither strands content nor races the sweep) and the Email commit run under
// one hold of the target account's lease, same as the first recipient's. A
// blobId is the content address (RFC 8620 section 6.1), so every copy of the
// message has the same one.
func (d *Deliverer) copyAndDeliver(ctx context.Context, from, to jmap.Id, blobID jmap.Id, recipient string, size int64, msg *parse.Parsed, now time.Time, rep *report.Inbound) (jmap.Id, error) {
	rc, _, err := d.store.Open(ctx, from, blobID)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	w, err := d.store.Create(ctx, to)
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		if !committed {
			_ = w.Abort()
		}
	}()
	if _, err := io.Copy(w, rc); err != nil {
		return "", err
	}
	var id jmap.Id
	finalized, _, err := d.db.FinalizeBlobUploadThenUpdate(ctx, to, w, recipient, now,
		d.inboxInsert(blobID, size, msg, now, rep, &id))
	committed = finalized != ""
	return id, err
}

// errTooLarge reports a message longer than the ingest size limit. It surfaces
// through the parser, which reports a failure to read its input, so the size
// limit does not need a separate pass over the message to enforce.
var errTooLarge = errors.New("mail: message exceeds the size limit")

// cappedReader passes a message through and stops it at the size limit. It reads
// one octet past the limit, which is how a message that is exactly at it is told
// from one that is over it.
type cappedReader struct {
	r    io.Reader
	max  int64
	read int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.read > c.max {
		return 0, errTooLarge
	}
	if room := c.max - c.read + 1; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.max {
		return n, errTooLarge
	}
	return n, err
}

// inboxInsert is the update half of one account's delivery: it finds the
// inbox and creates the Email record in it, writing the created id through
// id. The blob is already recorded in the account by the finalize half of the
// same lease hold, so the blobId passes the referential upload-record check
// later Email/set operations apply.
//
// When the message was recognized as a report (rep non-nil), ingestion runs
// first, in the same lease hold: the submission update, the report records,
// and the inbox Email all commit together or not at all. A matched report
// that advanced nothing is swallowed - the hold commits without an Email and
// id stays empty (see ingestReport).
func (d *Deliverer) inboxInsert(blobID jmap.Id, size int64, msg *parse.Parsed, now time.Time, rep *report.Inbound, id *jmap.Id) func(u *objectdb.Update) error {
	return func(u *objectdb.Update) error {
		if rep != nil {
			matched, deliver, err := submit.IngestReport(u, rep, blobID, now, submit.IngestOptions{MessageIDFallback: d.msgIDFallback})
			if err != nil {
				return err
			}
			if matched && !deliver {
				return nil
			}
		}
		inbox, err := record.RoleMailboxId(u, "inbox")
		if err != nil {
			return err
		}
		if inbox == "" {
			return errNoInbox
		}
		*id, err = emailstore.InsertEmail(u, msg, emailstore.EmailMeta{
			BlobID:     blobID,
			MailboxIds: emailstore.MailboxIdsJSON(map[jmap.Id]bool{inbox: true}),
			Keywords:   json.RawMessage(`{}`),
			Size:       uint64(size),
			ReceivedAt: now,
		})
		return err
	}
}
