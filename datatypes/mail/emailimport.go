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

// emailImport handles Email/import: the materialize seam plus import's own
// argument decoding, receivedAt resolution, and response shape. core bounds
// the batch size.
type emailImport struct {
	mat  materializer
	core jmap.CoreCapabilities
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

// importResponse is the Email/import response (section 4.8); the created
// values are the seam's emailCreated report, which carries exactly the
// section 4.8 properties. created and notCreated are Id[...]|null: an
// empty (nil) map marshals to null.
type importResponse struct {
	AccountId  jmap.Id                    `json:"accountId"`
	OldState   string                     `json:"oldState"`
	NewState   string                     `json:"newState"`
	Created    map[jmap.Id]emailCreated   `json:"created"`
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
	// open its blob and parse the message (the materialize seam's prepare
	// half; see materialize.go for why parsing stays off the lease).
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
	states, err := h.mat.db.Update(ctx, a.AccountId, func(u *objectdb.Update) error {
		state, err := h.mat.db.TypeState(ctx, a.AccountId, TypeEmail)
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
			// The request-wide creation-id map covers every newly created
			// record (RFC 8620 section 5.3), so a later "#cid" in the same
			// request resolves to the imported Email.
			call.CreatedIds[p.cid] = created.Id
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
	pe       *pendingEmail
	received time.Time
	serr     *jmap.SetError
}

// preflight opens and parses one EmailImport's blob without the account
// lease (the seam's prepare half). A bad EmailImport (missing/unknown/
// inaccessible blobId, unparseable receivedAt) yields a SetError; a real
// store failure aborts the whole call.
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
	pe, err := h.mat.prepare(ctx, acct, p.obj.BlobId, call.Identity)
	if errors.Is(err, blob.ErrNotFound) {
		p.serr = invalidProp("blobId", "blob not found")
		return p, nil
	}
	if err != nil {
		return p, err
	}
	p.pe = pe
	received, serr := importReceivedAt(p.obj.ReceivedAt, pe.msg.msg)
	if serr != nil {
		p.serr = serr
		return p, nil
	}
	p.received = received
	return p, nil
}

// commitOne validates a preflighted EmailImport and inserts it under the
// lease (the seam's commit half).
func (h emailImport) commitOne(u *objectdb.Update, p preparedImport) (*emailCreated, *jmap.SetError, error) {
	return h.mat.commit(u, p.pe, p.obj.MailboxIds, p.obj.Keywords, p.received)
}

// setCreated / setNotCreated lazily build the response maps (Id[...]|null:
// an empty map marshals to null, section 4.8).
func (r *importResponse) setCreated(cid jmap.Id, c emailCreated) {
	if r.Created == nil {
		r.Created = make(map[jmap.Id]emailCreated)
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
