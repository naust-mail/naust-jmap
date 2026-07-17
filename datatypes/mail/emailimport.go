package mail

// Email/import (RFC 8621 section 4.8): add already-uploaded RFC 5322 blobs
// to the store as Email records. Import is a custom method - creating an
// Email is an ingest of a message, not the generic property-bag create the
// derived /set offers (which Email forbids) - so it shares the one ingest
// path, insertEmail, with delivery. Each Email is an atomic unit that may
// succeed or fail on its own; a bad blobId/mailboxIds/keywords is an
// invalidProperties SetError, not a whole-call failure.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// emailImport handles Email/import. db is where records land, store holds
// the uploaded message blobs, core bounds the batch size, and maxMailboxes
// is the account's tooManyMailboxes limit.
type emailImport struct {
	db           *objectdb.DB
	store        blob.Store
	core         jmap.CoreCapabilities
	maxMailboxes *int64
}

// importArgs is the Email/import request (section 4.8).
type importArgs struct {
	AccountId jmap.Id                     `json:"accountId"`
	IfInState *string                     `json:"ifInState"`
	Emails    map[jmap.Id]json.RawMessage `json:"emails"`
}

// emailImportObj is one EmailImport (section 4.8): the blob to import plus
// the metadata to apply to the new Email.
type emailImportObj struct {
	BlobId     jmap.Id         `json:"blobId"`
	MailboxIds json.RawMessage `json:"mailboxIds"`
	Keywords   json.RawMessage `json:"keywords"`
	ReceivedAt *string         `json:"receivedAt"`
}

// importCreated is the created-Email report (section 4.8): id, blobId,
// threadId, size.
type importCreated struct {
	Id       jmap.Id `json:"id"`
	BlobId   jmap.Id `json:"blobId"`
	ThreadId jmap.Id `json:"threadId"`
	Size     uint64  `json:"size"`
}

// importResponse is the Email/import response (section 4.8). created and
// notCreated are Id[...]|null: an empty (nil) map marshals to null.
type importResponse struct {
	AccountId  jmap.Id                    `json:"accountId"`
	OldState   string                     `json:"oldState"`
	NewState   string                     `json:"newState"`
	Created    map[jmap.Id]importCreated  `json:"created"`
	NotCreated map[jmap.Id]*jmap.SetError `json:"notCreated"`
}

// errImportStateMismatch aborts the whole import when ifInState does not
// match the account's current Email state (section 4.8).
var errImportStateMismatch = errors.New("mail: import ifInState mismatch")

func (h emailImport) handle(ctx context.Context, call *runtime.Call) []jmap.Invocation {
	var a importArgs
	if err := runtime.DecodeArgs(call.Args, &a); err != nil {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := runtime.CheckAccount(call, a.AccountId, true); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	if int64(len(a.Emails)) > h.core.MaxObjectsInSet {
		return runtime.Fail(call.CallID, jmap.ErrRequestTooLarge, "")
	}

	// Preflight every EmailImport BEFORE taking the account write lease:
	// open its blob and parse the message. MIME parsing is the expensive
	// step and touches nothing in the store, so keeping it off the per-
	// account write lock means a large or hostile message no longer stalls
	// other writers to the account while it parses. Blobs are already size-
	// bounded by the upload path, so no extra cap is applied here.
	cids := sortedCreationIds(a.Emails)
	preps := make([]preparedImport, len(cids))
	for i, cid := range cids {
		p, err := h.preflight(ctx, call, a.AccountId, cid, a.Emails[cid])
		if err != nil {
			return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		preps[i] = p
	}

	resp := importResponse{AccountId: a.AccountId}
	states, err := h.db.Update(ctx, a.AccountId, func(u *objectdb.Update) error {
		state, err := h.db.TypeState(ctx, a.AccountId, TypeEmail)
		if err != nil {
			return err
		}
		resp.OldState = state
		if a.IfInState != nil && *a.IfInState != state {
			return errImportStateMismatch
		}
		// Preflight order is the stable sorted-cid order, so the outcome does
		// not depend on map iteration.
		for _, p := range preps {
			if p.serr != nil {
				resp.setNotCreated(p.cid, p.serr)
				continue
			}
			created, serr, err := h.commitOne(u, p)
			if err != nil {
				return err
			}
			if serr != nil {
				resp.setNotCreated(p.cid, serr)
				continue
			}
			resp.setCreated(p.cid, *created)
		}
		return nil
	})
	if errors.Is(err, errImportStateMismatch) {
		return runtime.Fail(call.CallID, jmap.ErrStateMismatch, "")
	}
	if err != nil {
		return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	resp.NewState = states[TypeEmail]
	if resp.NewState == "" {
		resp.NewState = resp.OldState
	}
	return runtime.Reply("Email/import", call.CallID, resp)
}

// preparedImport is one EmailImport after preflight: either a SetError to
// record under notCreated, or a parsed message ready to insert under the
// lease.
type preparedImport struct {
	cid      jmap.Id
	obj      emailImportObj
	msg      *parsed
	size     uint64
	received time.Time
	serr     *jmap.SetError
}

// preflight opens and parses one EmailImport's blob without the account
// lease. A bad EmailImport (missing/unknown/inaccessible blobId, unparseable
// receivedAt) yields a SetError; a real store failure aborts the whole call.
func (h emailImport) preflight(ctx context.Context, call *runtime.Call, acct, cid jmap.Id, raw json.RawMessage) (preparedImport, error) {
	p := preparedImport{cid: cid}
	if err := json.Unmarshal(raw, &p.obj); err != nil {
		p.serr = invalidProp("blobId", "EmailImport is not an object")
		return p, nil
	}
	if p.obj.BlobId == "" {
		p.serr = invalidProp("blobId", "blobId is required")
		return p, nil
	}
	// The blobId is client-supplied, so the open goes through the checked
	// path: an id that is invalid, unknown to the account, or unreferenced
	// and uploaded by someone else (RFC 8620 section 6.1) reads as not
	// found, indistinguishable from an absent blob.
	rc, size, err := runtime.OpenBlob(ctx, h.db, h.store, acct, p.obj.BlobId, call.Identity)
	if errors.Is(err, blob.ErrNotFound) {
		p.serr = invalidProp("blobId", "blob not found")
		return p, nil
	}
	if err != nil {
		return p, err
	}
	defer rc.Close()
	// Import stores an Email record, so it needs exactly what delivery does: the
	// headers and the two content-derived fast fields (section 4.1.4). Only the
	// preview is captured; no attachment is decoded.
	c := newCapture()
	c.preview = true
	p.msg, err = parseMessage(rc, c)
	if err != nil {
		return p, err
	}
	p.size = uint64(size)
	received, serr := importReceivedAt(p.obj.ReceivedAt, p.msg.msg)
	if serr != nil {
		p.serr = serr
		return p, nil
	}
	p.received = received
	return p, nil
}

// commitOne validates a preflighted EmailImport and inserts it under the
// lease. mailboxIds and keywords follow the same rules as Email/set (section
// 4.1.1): the pure validators check them without touching counters, which
// insertEmail maintains. validateKeywords lowercases in place.
func (h emailImport) commitOne(u *objectdb.Update, p preparedImport) (*importCreated, *jmap.SetError, error) {
	rec := objectdb.Object{"mailboxIds": p.obj.MailboxIds}
	if p.obj.Keywords != nil {
		rec["keywords"] = p.obj.Keywords
	} else {
		rec["keywords"] = json.RawMessage(`{}`)
	}
	if serr, err := validateMailboxIds(u, rec, h.maxMailboxes); serr != nil || err != nil {
		return nil, serr, err
	}
	if serr, err := validateKeywords(rec); serr != nil || err != nil {
		return nil, serr, err
	}
	meta := emailMeta{
		BlobID:     p.obj.BlobId,
		MailboxIds: rec["mailboxIds"],
		Keywords:   rec["keywords"],
		Size:       p.size,
		ReceivedAt: p.received,
	}
	id, err := insertEmail(u, p.msg, meta)
	if err != nil {
		return nil, nil, err
	}
	stored, err := u.Get(TypeEmail, id)
	if err != nil {
		return nil, nil, err
	}
	var tid jmap.Id
	json.Unmarshal(stored["threadId"], &tid)
	return &importCreated{Id: id, BlobId: p.obj.BlobId, ThreadId: tid, Size: meta.Size}, nil, nil
}

// setCreated / setNotCreated lazily build the response maps (Id[...]|null:
// an empty map marshals to null, section 4.8).
func (r *importResponse) setCreated(cid jmap.Id, c importCreated) {
	if r.Created == nil {
		r.Created = make(map[jmap.Id]importCreated)
	}
	r.Created[cid] = c
}

func (r *importResponse) setNotCreated(cid jmap.Id, e *jmap.SetError) {
	if r.NotCreated == nil {
		r.NotCreated = make(map[jmap.Id]*jmap.SetError)
	}
	r.NotCreated[cid] = e
}

// importReceivedAt resolves the receivedAt to store (section 4.8): the
// client value if given and valid, else the date of the most recent
// Received header, else the time of import on the server.
func importReceivedAt(client *string, m *message.Message) (time.Time, *jmap.SetError) {
	if client != nil {
		t, err := time.Parse(time.RFC3339, *client)
		if err != nil {
			return time.Time{}, invalidProp("receivedAt", "not a valid UTCDate")
		}
		return t, nil
	}
	if recv := m.HeaderInstances("Received"); len(recv) > 0 {
		// The most recent Received header is the topmost; its date is the
		// text after the last ";".
		if i := strings.LastIndex(recv[0], ";"); i >= 0 {
			if t := message.DateForm(recv[0][i+1:]); t != nil {
				return *t, nil
			}
		}
	}
	return time.Now(), nil
}

// sortedCreationIds returns the creation ids of an import map in a stable
// order, so a batch's outcome does not depend on map iteration order.
func sortedCreationIds(m map[jmap.Id]json.RawMessage) []jmap.Id {
	ids := make([]jmap.Id, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
