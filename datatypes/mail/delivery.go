package mail

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
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// Outcome is the disposition of a delivery to one recipient. It maps
// directly to an SMTP/LMTP reply class: Accepted -> 2xx, Rejected -> 5xx
// (permanent, the sender should bounce), TempFailed -> 4xx (transient, the
// sender should retry). TempFailed is the zero value on purpose: a recipient
// whose verdict has not yet been reached reads as a transient failure - the
// safe default, since an interrupted delivery is then retried rather than
// falsely reported as delivered.
type Outcome int

const (
	TempFailed Outcome = iota
	Rejected
	Accepted
)

// String is the lowercase name of an Outcome, for logs and the HTTP ingest
// response.
func (o Outcome) String() string {
	switch o {
	case Accepted:
		return "accepted"
	case Rejected:
		return "rejected"
	case TempFailed:
		return "tempfailed"
	default:
		return "unknown"
	}
}

// DeliveryEvent is the record of delivering one message to one recipient.
// It is the single delivery data structure: the synchronous per-recipient
// verdict an adapter needs to answer on the wire is just its Outcome, and a
// DeliverySink can persist the same value for audit or metrics. One event
// is produced per envelope recipient.
type DeliveryEvent struct {
	MailFrom   string    // envelope reverse-path; "" is the null sender <>
	Recipient  string    // the envelope forward-path this event reports
	Outcome    Outcome   // the verdict (see Outcome)
	Reason     string    // short human/log reason, for a bounce or a log line
	Account    jmap.Id   // resolved account, set when a recipient resolves
	EmailId    jmap.Id   // the created Email, set only when Accepted
	BlobId     jmap.Id   // content-addressed blob id of the raw message
	Size       int64     // octets of the raw message
	ReceivedAt time.Time // server receive time (the Email's receivedAt)
	MessageId  string    // the message's Message-ID header, for correlation
}

// Envelope is the SMTP-level envelope (RFC 5321): the reverse-path sender
// and the forward-path recipients, carried out of band from the message
// headers by the transport.
type Envelope struct {
	MailFrom   string
	Recipients []string
}

// Resolver maps an envelope recipient to the local account that should
// receive it. It is deployment-specific (which addresses are local, and how
// they map to accounts), so it is a host-provided socket, like the auth
// socket - the delivery core never bakes in an addressing scheme. Returning
// ok=false rejects the recipient (no such mailbox) before the body is read.
type Resolver interface {
	Resolve(ctx context.Context, recipient string) (account jmap.Id, ok bool)
}

// DeliverySink observes delivery outcomes. The default sink discards them;
// a host wanting durable delivery history, metrics, or bounce generation
// plugs its own. The structure (DeliveryEvent) is fixed so such a consumer
// retrofits nothing; only where the events go is left open.
type DeliverySink interface {
	Record(ctx context.Context, events []DeliveryEvent)
}

type nopSink struct{}

func (nopSink) Record(context.Context, []DeliveryEvent) {}

// There is no cap on concurrent deliveries here. A delivery streams: the
// message is never held, so a parse in flight costs a buffer and not a message,
// and what an ingest must bound is therefore how many CONNECTIONS it is serving
// - which is the adapter's business (ServeLMTP, HTTPIngest), not the pipeline's.
// A cap here would also be the wrong shape: it would be held across a network
// read, so a few slow senders could hold every slot and stall delivery for
// everyone.

// defaultMaxMessageSize is the raw-message ceiling, in octets, a Deliverer
// accepts when the embedder passes no WithMaxMessageSize. It mirrors the JMAP
// maxSizeUpload default (core runtime, 50_000_000 octets) so a message
// delivered over LMTP and a blob imported via Email/import share one effective
// ceiling - an imported blob was uploaded through that same-capped endpoint. It
// mirrors a typical MTA message_size_limit too. An embedder that raises the
// session's maxSizeUpload should raise this to match with WithMaxMessageSize.
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
	sink     DeliverySink
	maxSize  int64
}

// DelivererOption configures a Deliverer.
type DelivererOption func(*Deliverer)

// WithMaxMessageSize caps the raw message size a Deliverer will accept.
func WithMaxMessageSize(n int64) DelivererOption {
	return func(d *Deliverer) { d.maxSize = n }
}

// WithDeliverySink installs a sink for delivery outcomes (default: discard).
func WithDeliverySink(s DeliverySink) DelivererOption {
	return func(d *Deliverer) { d.sink = s }
}

// NewDeliverer builds a Deliverer. db is where Emails land, store holds the
// raw-message blobs, and resolver maps recipients to accounts.
func NewDeliverer(db *objectdb.DB, store blob.Store, resolver Resolver, opts ...DelivererOption) *Deliverer {
	d := &Deliverer{
		db:       db,
		store:    store,
		resolver: resolver,
		sink:     nopSink{},
		maxSize:  defaultMaxMessageSize,
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// MaxMessageSize is the raw-message cap in octets, for an adapter that wants
// to advertise SIZE or reject early.
func (d *Deliverer) MaxMessageSize() int64 { return d.maxSize }

// Deliver ingests one message for the envelope's recipients and returns one
// DeliveryEvent per recipient, in the order given. It never returns an
// error: every failure mode is a per-recipient Outcome, so one bad recipient
// or a transient store fault cannot fail delivery to the others.
//
// A panic anywhere below this seam (parse, threading, insert) is recovered
// here so a hostile or malformed message cannot crash the process that
// co-hosts the JMAP server. This is the SHARED boundary: every ingest adapter
// (LMTP, HTTP, any future one) inherits crash-only isolation without repeating
// it. The verdict slice is owned here and starts at the safe default
// (TempFailed is the zero value); deliver only ever upgrades an entry once a
// verdict is earned, so the recover keeps every verdict already decided and
// leaves the rest transient rather than rebuilding them. RFC 5321 has no
// "panic"; a local processing failure is a 4yz transient outcome (the MTA
// retries), which LMTP maps to 451.
func (d *Deliverer) Deliver(ctx context.Context, env Envelope, r io.Reader) (events []DeliveryEvent) {
	events = make([]DeliveryEvent, len(env.Recipients))
	for i, rcpt := range env.Recipients {
		events[i] = DeliveryEvent{MailFrom: env.MailFrom, Recipient: rcpt}
	}
	// The sink is fed at this boundary, not inside the pipeline, so it sees
	// every delivery's verdicts exactly once - a recovered panic included
	// (the deferred recover below runs before this earlier defer).
	defer func() { d.sink.Record(ctx, events) }()
	defer func() {
		if p := recover(); p != nil {
			log.Printf("naust-jmap delivery: recovered panic: %v", p)
		}
	}()
	d.deliver(ctx, env, r, events)
	return events
}

// deliver is the delivery pipeline proper; Deliver wraps it in the panic
// boundary above and owns the events slice, which deliver fills in place: each
// entry already carries its MailFrom and Recipient and the safe default
// verdict (TempFailed).
func (d *Deliverer) deliver(ctx context.Context, env Envelope, r io.Reader, events []DeliveryEvent) {
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
			events[i].Outcome = Rejected
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
	failAll := func(o Outcome, reason string) {
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
		failAll(TempFailed, "temporary server error")
		log.Printf("naust-jmap delivery: blob create: %v", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = bw.Abort() // a rejected, failed, or panicking delivery stores nothing
		}
	}()

	capped := &cappedReader{r: r, max: d.maxSize}
	c := newCapture()
	c.preview = true
	msg, err := parseMessage(io.TeeReader(capped, bw), c)
	switch {
	case errors.Is(err, errTooLarge):
		failAll(Rejected, "message too large")
		return
	case err != nil:
		failAll(TempFailed, "read error")
		log.Printf("naust-jmap delivery: read error: %v", err)
		return
	}
	size := capped.read
	now := time.Now()
	msgID := messageIDHeader(msg.msg.Headers)

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
		events[targets[0].idx].Recipient, now, d.inboxInsert(blobID, size, msg, now, &firstEmail))
	if finalized == "" {
		failAll(TempFailed, "temporary server error")
		log.Printf("naust-jmap delivery: blob finalize: %v", firstErr)
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
			id, err = d.copyAndDeliver(ctx, targets[0].account, t.account, blobID, ev.Recipient, size, msg, now)
		}
		switch {
		case err == nil:
			ev.Outcome, ev.EmailId = Accepted, id
		case errors.Is(err, errNoInbox):
			ev.Outcome, ev.Reason = TempFailed, "no inbox mailbox"
		default:
			ev.Outcome, ev.Reason = TempFailed, "temporary server error"
			// Quote the untrusted recipient so no control character it might
			// carry can forge or escape a log line (defense in depth; the LMTP
			// ingress already rejects control-char addresses at parse time).
			log.Printf("naust-jmap delivery: %s: %v", strconv.Quote(ev.Recipient), err)
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
func (d *Deliverer) copyAndDeliver(ctx context.Context, from, to jmap.Id, blobID jmap.Id, recipient string, size int64, msg *parsed, now time.Time) (jmap.Id, error) {
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
		d.inboxInsert(blobID, size, msg, now, &id))
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
func (d *Deliverer) inboxInsert(blobID jmap.Id, size int64, msg *parsed, now time.Time, id *jmap.Id) func(u *objectdb.Update) error {
	return func(u *objectdb.Update) error {
		inbox, err := inboxMailboxId(u)
		if err != nil {
			return err
		}
		if inbox == "" {
			return errNoInbox
		}
		*id, err = insertEmail(u, msg, emailMeta{
			BlobID:     blobID,
			MailboxIds: mailboxIdsJSON(map[jmap.Id]bool{inbox: true}),
			Keywords:   json.RawMessage(`{}`),
			Size:       uint64(size),
			ReceivedAt: now,
		})
		return err
	}
}

// inboxMailboxId is the account's inbox Mailbox id as this Update sees it,
// or "" if it has none (the inbox role is unique per account, so at most
// one). Mirrors trashMailboxId.
func inboxMailboxId(u *objectdb.Update) (jmap.Id, error) {
	ids, err := u.IdsWhereEqual(TypeMailbox, "role", json.RawMessage(`"inbox"`))
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

// messageIDHeader is the first Message-ID of the message, or "" - purely for
// correlating a DeliveryEvent with external logs.
func messageIDHeader(headers []message.HeaderField) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, "Message-ID") {
			if ids := message.MessageIDsForm(h.Value); len(ids) > 0 {
				return ids[0]
			}
		}
	}
	return ""
}
