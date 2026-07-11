package runtime

// Foo/copy (RFC 8620 section 5.4): the only way to move records
// between two different accounts. Conceptually three phases - read
// from the from account, write the copies to the target account,
// optionally destroy the originals - and the phases are NOT atomic:
// the copy can succeed while the destroy fails.

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

type copyArgs struct {
	FromAccountId            jmap.Id                     `json:"fromAccountId"`
	IfFromInState            *string                     `json:"ifFromInState"`
	AccountId                jmap.Id                     `json:"accountId"`
	IfInState                *string                     `json:"ifInState"`
	Create                   map[jmap.Id]json.RawMessage `json:"create"`
	OnSuccessDestroyOriginal bool                        `json:"onSuccessDestroyOriginal"`
	DestroyFromIfInState     *string                     `json:"destroyFromIfInState"`
}

type copyResponse struct {
	FromAccountId jmap.Id `json:"fromAccountId"`
	AccountId     jmap.Id `json:"accountId"`
	OldState      string  `json:"oldState"`
	NewState      string  `json:"newState"`
	// Created and NotCreated are Id[Foo]|null and Id[SetError]|null:
	// explicitly null - not empty - when nothing was copied or nothing
	// failed (5.4), hence no omitzero.
	Created    map[jmap.Id]objectdb.Object `json:"created"`
	NotCreated map[jmap.Id]*jmap.SetError  `json:"notCreated"`
}

func (r *copyResponse) notCreated(id jmap.Id, e *jmap.SetError) {
	if r.NotCreated == nil {
		r.NotCreated = make(map[jmap.Id]*jmap.SetError)
	}
	r.NotCreated[id] = e
}

func (st *stdType) copy(ctx context.Context, call *Call) []jmap.Invocation {
	var a copyArgs
	if err := decodeArgs(call.Args, &a); err != nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	// The from account needs read access only; an inaccessible one is
	// the method's own fromAccountNotFound error (5.4).
	if a.FromAccountId == "" {
		return fail(call.CallID, jmap.ErrInvalidArguments, "fromAccountId is required")
	}
	if errType, desc := checkAccount(call, a.FromAccountId, false); errType != "" {
		if errType == jmap.ErrAccountNotFound {
			errType = jmap.ErrFromAccountNotFound
		}
		return fail(call.CallID, errType, desc)
	}
	if errType, desc := checkAccount(call, a.AccountId, true); errType != "" {
		return fail(call.CallID, errType, desc)
	}
	// accountId MUST be different to fromAccountId (5.4).
	if a.AccountId == a.FromAccountId {
		return fail(call.CallID, jmap.ErrInvalidArguments, "accountId must differ from fromAccountId")
	}
	if int64(len(a.Create)) > st.core.MaxObjectsInSet {
		return fail(call.CallID, jmap.ErrRequestTooLarge, "")
	}

	resp := copyResponse{FromAccountId: a.FromAccountId, AccountId: a.AccountId}
	// Source ids of the successful copies, for the implicit destroy.
	var copiedFrom []jmap.Id
	states, err := st.db.Update(ctx, a.AccountId, func(u *objectdb.Update) error {
		// Phase 1, reading: ifFromInState must match the from account's
		// state when reading the data to be copied (5.4). Reads take no
		// lease, so only the target account's lease is held here.
		fromState, err := st.db.TypeState(ctx, a.FromAccountId, st.t.Name)
		if err != nil {
			return err
		}
		if a.IfFromInState != nil && *a.IfFromInState != fromState {
			return errStateMismatch
		}
		state, err := st.db.TypeState(ctx, a.AccountId, st.t.Name)
		if err != nil {
			return err
		}
		resp.OldState = state
		if a.IfInState != nil && *a.IfInState != state {
			return errStateMismatch
		}
		copiedFrom, err = st.processCopies(ctx, u, call, &a, &resp)
		return err
	})
	if errors.Is(err, errStateMismatch) {
		return fail(call.CallID, jmap.ErrStateMismatch, "")
	}
	if err != nil {
		return fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	resp.NewState = states[st.t.Name]
	if resp.NewState == "" {
		resp.NewState = resp.OldState
	}
	out := reply(st.t.Name+"/copy", call.CallID, resp)

	// onSuccessDestroyOriginal: after emitting the Foo/copy response and
	// before processing the next method, the server MUST make a single
	// Foo/set call destroying the original of each successfully copied
	// record; its output joins the responses under the same call id
	// (5.4). destroyFromIfInState is passed through as its ifInState, so
	// the destroy can abort with stateMismatch while the copy stands.
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
		raw, err := json.Marshal(setArgs{
			AccountId: a.FromAccountId,
			IfInState: a.DestroyFromIfInState,
			Destroy:   destroy,
		})
		if err != nil {
			return append(out, fail(call.CallID, jmap.ErrServerFail, err.Error())...)
		}
		implicit := &Call{
			Name: st.t.Name + "/set", Args: raw, CallID: call.CallID,
			Identity: call.Identity, CreatedIds: call.CreatedIds,
		}
		out = append(out, st.set(ctx, implicit)...)
	}
	return out
}

// processCopies runs each copy as an atomic unit (5.4): read the
// original, override it with any client-supplied properties, and push
// the result through the standard create pipeline. Returns the source
// ids of the successful copies.
func (st *stdType) processCopies(ctx context.Context, u *objectdb.Update, call *Call, a *copyArgs, resp *copyResponse) ([]jmap.Id, error) {
	type pendingCopy struct {
		obj    objectdb.Object
		sent   map[string]bool
		fromId jmap.Id
	}
	pending := make(map[jmap.Id]*pendingCopy)

	for _, cid := range sortedIds(mapKeys(a.Create)) {
		var overlay objectdb.Object
		if err := json.Unmarshal(a.Create[cid], &overlay); err != nil {
			resp.notCreated(cid, &jmap.SetError{Type: jmap.SetErrInvalidProperties, Description: "create value is not an object"})
			continue
		}
		// The object MUST contain an id: the record in the from account
		// to be copied (5.4). A "#creationId" from earlier in the request
		// resolves as usual (3.7).
		rawId, has := overlay["id"]
		if !has {
			resp.notCreated(cid, invalidProperties([]string{"id"}))
			continue
		}
		delete(overlay, "id")
		var fromId jmap.Id
		resolved, ok := resolveIdValue(rawId, call.CreatedIds)
		if ok {
			ok = json.Unmarshal(resolved, &fromId) == nil && fromId != ""
		}
		if !ok {
			resp.notCreated(cid, invalidProperties([]string{"id"}))
			continue
		}
		// Overlaid properties follow the create rules: unknown and
		// server-set properties are invalid (5.3).
		var bad []string
		for name := range overlay {
			p, declared := st.t.Properties[name]
			if !declared || p.ServerSet {
				bad = append(bad, name)
			}
		}
		if len(bad) > 0 {
			resp.notCreated(cid, invalidProperties(bad))
			continue
		}
		original, err := st.db.Get(ctx, a.FromAccountId, st.t.Name, fromId)
		if errors.Is(err, objectdb.ErrNotFound) {
			resp.notCreated(cid, &jmap.SetError{Type: jmap.SetErrNotFound})
			continue
		}
		if err != nil {
			return nil, err
		}
		// Candidate: the original's values minus id and server-set
		// properties (the server re-derives those in the target account),
		// with any client-supplied properties used instead (5.4).
		obj := make(objectdb.Object, len(original))
		for name, v := range original {
			if name == "id" {
				continue
			}
			if p, declared := st.t.Properties[name]; declared && p.ServerSet {
				continue
			}
			obj[name] = v
		}
		for name, v := range overlay {
			obj[name] = v
		}
		sent := make(map[string]bool, len(obj))
		for name := range obj {
			sent[name] = true
		}
		for name, p := range st.t.Properties {
			if p.Default == nil {
				continue
			}
			if _, has := obj[name]; !has {
				obj[name] = p.Default
			}
		}
		pending[cid] = &pendingCopy{obj: obj, sent: sent, fromId: fromId}
	}

	// Same dependency ordering as /set creates: a copy whose overlaid
	// Id-kind property references another copy's creation id waits for
	// that copy to land (5.3 via 3.7).
	var copiedFrom []jmap.Id
	for len(pending) > 0 {
		progress := false
		for _, cid := range sortedIds(mapKeys(pending)) {
			pc := pending[cid]
			resolved, _ := resolveIdRefs(st.t, pc.obj, call.CreatedIds)
			if !resolved {
				continue
			}
			delete(pending, cid)
			progress = true
			if bad := checkValues(st.t, pc.obj); len(bad) > 0 {
				resp.notCreated(cid, invalidProperties(bad))
				continue
			}
			// A BlobRef property must reference a blob in the TARGET
			// account: copy blobs across first with Blob/copy (section 6.3).
			bad, err := checkBlobRefs(u, st.t, pc.obj)
			if err != nil {
				return nil, err
			}
			if len(bad) > 0 {
				resp.notCreated(cid, invalidProperties(bad))
				continue
			}
			id, err := u.Create(st.t.Name, pc.obj)
			if err != nil {
				return nil, err
			}
			call.CreatedIds[cid] = id
			stored, err := u.Get(st.t.Name, id)
			if err != nil {
				return nil, err
			}
			// The created response carries the properties set by the
			// server, the new id above all (5.4).
			out := make(objectdb.Object)
			for name, v := range stored {
				if !pc.sent[name] {
					out[name] = v
				}
			}
			if resp.Created == nil {
				resp.Created = make(map[jmap.Id]objectdb.Object)
			}
			resp.Created[cid] = out
			copiedFrom = append(copiedFrom, pc.fromId)
		}
		if !progress {
			for cid, pc := range pending {
				_, unresolved := resolveIdRefs(st.t, pc.obj, call.CreatedIds)
				resp.notCreated(cid, invalidProperties(unresolved))
			}
			break
		}
	}
	return copiedFrom, nil
}
