package mail

// The materialize seam: the one blob-to-Email-record gate shared by the
// blob-shaped Email producers - Email/import (RFC 8621 section 4.8), and,
// as they land, Email/copy (4.7) and Email/set create (4.6). The shape is
// two halves around the account write lease: prepare opens and parses the
// message blob OUTSIDE the lease (parsing is the expensive step and touches
// nothing in the store, so a large or hostile message never stalls other
// writers to the account), and commit validates the JMAP metadata and
// inserts the record UNDER it. Delivery deliberately does not use this
// gate: its parse-first fan-out pipeline stays independently optimizable
// on the same low-level helpers (parseMessage, insertEmail).

import (
	"context"
	"encoding/json"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// materializer turns a message blob in an account into an Email record in
// the same account.
type materializer struct {
	db           *objectdb.DB
	store        blob.Store
	maxMailboxes *int64
}

// pendingEmail is one message blob parsed and ready to commit as an Email
// record: everything derivable outside the account lease.
type pendingEmail struct {
	blobId jmap.Id
	msg    *parsed
	size   uint64
}

// prepare opens and parses blobId in acct, outside the account lease. The
// open goes through the checked blob path: an id that is invalid, unknown
// to the account, or unreferenced and uploaded by someone else (RFC 8620
// section 6.1) reads as blob.ErrNotFound, which flows back for the caller
// to map into its own SetError vocabulary. Blobs are already size-bounded
// by the upload path, so no extra cap is applied here.
func (m materializer) prepare(ctx context.Context, acct, blobId jmap.Id, ident *auth.Identity) (*pendingEmail, error) {
	rc, size, err := runtime.OpenBlob(ctx, m.db, m.store, acct, blobId, ident)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// An Email record needs exactly what delivery stores: the headers and
	// the two content-derived fast fields (section 4.1.4). Only the preview
	// is captured; no attachment is decoded.
	c := newCapture()
	c.preview = true
	msg, err := parseMessage(rc, c)
	if err != nil {
		return nil, err
	}
	return &pendingEmail{blobId: blobId, msg: msg, size: uint64(size)}, nil
}

// emailCreated is the created report of a materialized Email: the
// server-set properties the specs require the response to carry for
// Email/import (section 4.8) and Email/copy (section 4.7) - id, blobId,
// threadId, size.
type emailCreated struct {
	Id       jmap.Id `json:"id"`
	BlobId   jmap.Id `json:"blobId"`
	ThreadId jmap.Id `json:"threadId"`
	Size     uint64  `json:"size"`
}

// commit validates the metadata and inserts the Email under the account
// lease, returning the created report. mailboxIds and keywords follow the
// same rules as Email/set (section 4.1.1): the pure validators check them
// without touching counters, which insertEmail maintains; nil keywords
// default to the empty map, and validateKeywords lowercases in place.
func (m materializer) commit(u *objectdb.Update, pe *pendingEmail, mailboxIds, keywords json.RawMessage, receivedAt time.Time) (*emailCreated, *jmap.SetError, error) {
	rec := objectdb.Object{"mailboxIds": mailboxIds}
	if keywords != nil {
		rec["keywords"] = keywords
	} else {
		rec["keywords"] = json.RawMessage(`{}`)
	}
	if serr, err := validateMailboxIds(u, rec, m.maxMailboxes); serr != nil || err != nil {
		return nil, serr, err
	}
	if serr, err := validateKeywords(rec); serr != nil || err != nil {
		return nil, serr, err
	}
	meta := emailMeta{
		BlobID:     pe.blobId,
		MailboxIds: rec["mailboxIds"],
		Keywords:   rec["keywords"],
		Size:       pe.size,
		ReceivedAt: receivedAt,
	}
	id, err := insertEmail(u, pe.msg, meta)
	if err != nil {
		return nil, nil, err
	}
	stored, err := u.Get(TypeEmail, id)
	if err != nil {
		return nil, nil, err
	}
	var tid jmap.Id
	json.Unmarshal(stored["threadId"], &tid)
	return &emailCreated{Id: id, BlobId: pe.blobId, ThreadId: tid, Size: pe.size}, nil, nil
}
