package mail

// Email/copy (RFC 8621 section 4.7): the standard /copy (RFC 8620 section
// 5.4) with mail's restriction - only mailboxIds, keywords, and receivedAt
// may be set during the copy; the message itself cannot be modified.
// Copying an Email between accounts is an ingest into the target account:
// the message blob is copied across, re-parsed, and re-threaded there
// (Threads are per-account), with the Mailbox counters maintained. The
// generic derived record copy can do none of that, so Email/copy is a
// custom method on the materialize seam, like Email/import. The spec's
// optional duplicate forbidding is not implemented (this runtime never
// forbids duplicates), so alreadyExists is never returned. Each copy is an
// atomic unit that may succeed or fail alone.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// emailCopy handles Email/copy: the materialize seam plus the copy
// choreography. proc runs the section 5.4 implicit destroy; core bounds
// the batch size.
type emailCopy struct {
	mat  materializer
	proc *runtime.Processor
	core jmap.CoreCapabilities
}

// emailCopyArgs is the standard /copy request (RFC 8620 section 5.4).
type emailCopyArgs struct {
	FromAccountId            jmap.Id                     `json:"fromAccountId"`
	IfFromInState            *string                     `json:"ifFromInState"`
	AccountId                jmap.Id                     `json:"accountId"`
	IfInState                *string                     `json:"ifInState"`
	Create                   map[jmap.Id]json.RawMessage `json:"create"`
	OnSuccessDestroyOriginal bool                        `json:"onSuccessDestroyOriginal"`
	DestroyFromIfInState     *string                     `json:"destroyFromIfInState"`
}

// emailCopyResponse is the standard /copy response; the created values are
// the seam's emailCreated report, exactly the properties section 4.7
// requires. Created and NotCreated are Id[...]|null: explicitly null - not
// empty - when nothing was copied or nothing failed (5.4).
type emailCopyResponse struct {
	FromAccountId jmap.Id                    `json:"fromAccountId"`
	AccountId     jmap.Id                    `json:"accountId"`
	OldState      string                     `json:"oldState"`
	NewState      string                     `json:"newState"`
	Created       map[jmap.Id]emailCreated   `json:"created"`
	NotCreated    map[jmap.Id]*jmap.SetError `json:"notCreated"`
}

func (r *emailCopyResponse) setCreated(cid jmap.Id, c emailCreated) {
	if r.Created == nil {
		r.Created = make(map[jmap.Id]emailCreated)
	}
	r.Created[cid] = c
}

func (r *emailCopyResponse) setNotCreated(cid jmap.Id, e *jmap.SetError) {
	if r.NotCreated == nil {
		r.NotCreated = make(map[jmap.Id]*jmap.SetError)
	}
	r.NotCreated[cid] = e
}

// errCopyStateMismatch aborts the whole copy when ifInState does not match
// the target account's current Email state (5.4).
var errCopyStateMismatch = errors.New("mail: copy ifInState mismatch")

func (h emailCopy) handle(ctx context.Context, call *runtime.Call) []jmap.Invocation {
	var a emailCopyArgs
	if err := runtime.DecodeArgs(call.Args, &a); err != nil {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	// The from account needs read access only; an inaccessible one is the
	// method's own fromAccountNotFound error (5.4).
	if a.FromAccountId == "" {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, "fromAccountId is required")
	}
	if errType, desc := runtime.CheckAccount(call, a.FromAccountId, false); errType != "" {
		if errType == jmap.ErrAccountNotFound {
			errType = jmap.ErrFromAccountNotFound
		}
		return runtime.Fail(call.CallID, errType, desc)
	}
	if errType, desc := runtime.CheckAccount(call, a.AccountId, true); errType != "" {
		return runtime.Fail(call.CallID, errType, desc)
	}
	// accountId MUST be different to fromAccountId (5.4).
	if a.AccountId == a.FromAccountId {
		return runtime.Fail(call.CallID, jmap.ErrInvalidArguments, "accountId must differ from fromAccountId")
	}
	if int64(len(a.Create)) > h.core.MaxObjectsInSet {
		return runtime.Fail(call.CallID, jmap.ErrRequestTooLarge, "")
	}

	// Phase 1, reading (5.4): ifFromInState must match the from account's
	// Email state when the data to be copied is read - and the reads, the
	// blob copies, and the expensive parses all happen here, before the
	// target account's write lease is taken (the seam's prepare half).
	fromState, err := h.mat.db.TypeState(ctx, a.FromAccountId, TypeEmail)
	if err != nil {
		return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	if a.IfFromInState != nil && *a.IfFromInState != fromState {
		return runtime.Fail(call.CallID, jmap.ErrStateMismatch, "")
	}
	cids := sortedCreationIds(a.Create)
	preps := make([]preparedCopy, len(cids))
	for i, cid := range cids {
		p, err := h.preflight(ctx, call, &a, cid, a.Create[cid])
		if err != nil {
			return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		preps[i] = p
	}

	resp := emailCopyResponse{FromAccountId: a.FromAccountId, AccountId: a.AccountId}
	// Source ids of the successful copies, for the implicit destroy.
	var copiedFrom []jmap.Id
	states, err := h.mat.db.Update(ctx, a.AccountId, func(u *objectdb.Update) error {
		state, err := h.mat.db.TypeState(ctx, a.AccountId, TypeEmail)
		if err != nil {
			return err
		}
		resp.OldState = state
		if a.IfInState != nil && *a.IfInState != state {
			return errCopyStateMismatch
		}
		for _, p := range preps {
			if p.serr != nil {
				resp.setNotCreated(p.cid, p.serr)
				continue
			}
			ec, serr, err := h.mat.commit(u, p.pe, p.mailboxIds, p.keywords, p.received)
			if err != nil {
				return err
			}
			if serr != nil {
				resp.setNotCreated(p.cid, serr)
				continue
			}
			call.CreatedIds[p.cid] = ec.Id
			resp.setCreated(p.cid, *ec)
			copiedFrom = append(copiedFrom, p.fromId)
		}
		return nil
	})
	if errors.Is(err, errCopyStateMismatch) {
		return runtime.Fail(call.CallID, jmap.ErrStateMismatch, "")
	}
	if err != nil {
		return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	resp.NewState = states[TypeEmail]
	if resp.NewState == "" {
		resp.NewState = resp.OldState
	}
	out := runtime.Reply("Email/copy", call.CallID, resp)

	// onSuccessDestroyOriginal: after emitting the Email/copy response and
	// before processing the next method, the server MUST make a single
	// Email/set call destroying the original of each successfully copied
	// record; its output joins the responses under the same call id (5.4).
	// destroyFromIfInState is passed through as its ifInState, so the
	// destroy can abort with stateMismatch while the copy stands. The
	// Email/set destroy path unwinds the from account's Thread membership
	// and Mailbox counters.
	if a.OnSuccessDestroyOriginal && len(copiedFrom) > 0 {
		// Two creates copying the same original destroy it once.
		seen := make(map[jmap.Id]bool, len(copiedFrom))
		destroy := make([]jmap.Id, 0, len(copiedFrom))
		for _, id := range copiedFrom {
			if !seen[id] {
				seen[id] = true
				destroy = append(destroy, id)
			}
		}
		out = append(out, h.proc.ImplicitSet(ctx, TypeEmail, struct {
			AccountId jmap.Id   `json:"accountId"`
			IfInState *string   `json:"ifInState"`
			Destroy   []jmap.Id `json:"destroy"`
		}{a.FromAccountId, a.DestroyFromIfInState, destroy}, call)...)
	}
	return out
}

// preparedCopy is one copy request after preflight: either a SetError to
// record under notCreated, or a parsed message in the target account ready
// to insert under its lease, with the resolved metadata to apply.
type preparedCopy struct {
	cid        jmap.Id
	fromId     jmap.Id
	pe         *pendingEmail
	mailboxIds json.RawMessage
	keywords   json.RawMessage
	received   time.Time
	serr       *jmap.SetError
}

// preflight resolves one copy request without the target account's lease:
// validate the object shape against 4.7's restricted property set, read
// the original in the from account, copy its blob into the target account
// (content addressing keeps the blobId), and parse it there. A bad request
// yields a SetError; a real store failure aborts the whole call - in
// particular, the blob behind an existing Email record is referenced and
// so cannot have been swept, meaning a failure to read it is a store
// fault, never a client error.
func (h emailCopy) preflight(ctx context.Context, call *runtime.Call, a *emailCopyArgs, cid jmap.Id, raw json.RawMessage) (preparedCopy, error) {
	p := preparedCopy{cid: cid}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		p.serr = &jmap.SetError{Type: jmap.SetErrInvalidProperties, Description: "create value is not an object"}
		return p, nil
	}
	// Only mailboxIds, keywords, and receivedAt may be set during the
	// copy (4.7); id names the original (5.4).
	var bad []string
	for name := range obj {
		switch name {
		case "id", "mailboxIds", "keywords", "receivedAt":
		default:
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		p.serr = &jmap.SetError{
			Type: jmap.SetErrInvalidProperties, Properties: bad,
			Description: "only mailboxIds, keywords, and receivedAt may be set during a copy",
		}
		return p, nil
	}
	// The object MUST contain an id: the Email in the from account to be
	// copied (5.4). A "#creationId" from earlier in the request resolves
	// as usual (3.7).
	rawId, has := obj["id"]
	if !has {
		p.serr = invalidProp("id", "id is required")
		return p, nil
	}
	var fromId jmap.Id
	if err := json.Unmarshal(rawId, &fromId); err != nil || fromId == "" {
		p.serr = invalidProp("id", "not an id")
		return p, nil
	}
	fromId, ok := runtime.ResolveIdArg(fromId, call.CreatedIds)
	if !ok {
		p.serr = invalidProp("id", "unknown creation id")
		return p, nil
	}
	original, err := h.mat.db.Get(ctx, a.FromAccountId, TypeEmail, fromId)
	if errors.Is(err, objectdb.ErrNotFound) {
		p.serr = &jmap.SetError{Type: jmap.SetErrNotFound}
		return p, nil
	}
	if err != nil {
		return p, err
	}
	p.fromId = fromId

	// Any property not supplied comes from the original (5.4). Inherited
	// mailboxIds name from-account Mailboxes, which almost never exist in
	// the target account - commit's validation rejects the unknown ids, so
	// in practice a cross-account copy names its target Mailboxes
	// explicitly.
	p.mailboxIds = obj["mailboxIds"]
	if p.mailboxIds == nil {
		p.mailboxIds = original["mailboxIds"]
	}
	p.keywords = obj["keywords"]
	if p.keywords == nil {
		p.keywords = original["keywords"]
	}
	if rawRA, has := obj["receivedAt"]; has {
		var s string
		if err := json.Unmarshal(rawRA, &s); err != nil {
			p.serr = invalidProp("receivedAt", "not a valid UTCDate")
			return p, nil
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			p.serr = invalidProp("receivedAt", "not a valid UTCDate")
			return p, nil
		}
		p.received = t
	} else {
		var s string
		json.Unmarshal(original["receivedAt"], &s)
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return p, errors.New("mail: stored receivedAt is not RFC 3339: " + s)
		}
		p.received = t
	}

	// The copy is an ingest: bring the message bytes into the target
	// account, then parse them there through the seam. The blobId is
	// server-set on the original record, so it is trusted; the copy's
	// upload record is what lets the seam's checked open in the target
	// account succeed.
	var blobId jmap.Id
	json.Unmarshal(original["blobId"], &blobId)
	newBlob, err := runtime.CopyBlob(ctx, h.mat.db, h.mat.store, a.FromAccountId, a.AccountId, blobId, call.Identity)
	if err != nil {
		return p, err
	}
	pe, err := h.mat.prepare(ctx, a.AccountId, newBlob, call.Identity)
	if err != nil {
		return p, err
	}
	p.pe = pe
	return p, nil
}
