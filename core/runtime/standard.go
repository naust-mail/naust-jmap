package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// RegisterStandardType registers a datatype descriptor with the object
// database and derives its Foo/get, Foo/changes, Foo/set, Foo/copy,
// Foo/query, and Foo/queryChanges methods (RFC 8620 sections 5.1-5.6).
// This is the runtime's central promise:
// a plugin declares a type; the protocol machinery is generated. The
// capability must additionally be advertised in the session via
// Server.RegisterCapability.
func RegisterStandardType(p *Processor, db *objectdb.DB, t *descriptor.Type, core jmap.CoreCapabilities) error {
	return RegisterStandardTypeExt(p, db, t, core, nil)
}

// RegisterStandardTypeExt is RegisterStandardType with per-type
// extension hooks (see Extensions) for datatypes whose RFC extends the
// standard methods, as RFC 8621 does. A nil ext derives the plain
// RFC 8620 methods.
func RegisterStandardTypeExt(p *Processor, db *objectdb.DB, t *descriptor.Type, core jmap.CoreCapabilities, ext *Extensions) error {
	if ext != nil {
		if err := ext.validate(t); err != nil {
			return err
		}
	}
	if err := db.RegisterType(t); err != nil {
		return err
	}
	st := &stdType{db: db, t: t, core: core, ext: ext}
	p.Register(t.Name+"/get", t.Capability, st.get)
	p.Register(t.Name+"/changes", t.Capability, st.changes)
	p.Register(t.Name+"/set", t.Capability, st.set)
	p.Register(t.Name+"/copy", t.Capability, st.copy)
	p.Register(t.Name+"/query", t.Capability, st.query)
	p.Register(t.Name+"/queryChanges", t.Capability, st.queryChanges)
	return nil
}

type stdType struct {
	db   *objectdb.DB
	t    *descriptor.Type
	core jmap.CoreCapabilities
	ext  *Extensions // nil = no extensions
}

// decodeArgs strictly decodes method arguments; unknown or mistyped
// arguments are invalidArguments (section 3.6.2).
func decodeArgs(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func fail(callID, errType, description string) []jmap.Invocation {
	return []jmap.Invocation{jmap.ErrorInvocation(callID, jmap.MethodError{Type: errType, Description: description})}
}

// checkAccount validates the accountId argument against the caller's
// access (sections 3.6.2 accountNotFound / accountReadOnly).
func checkAccount(call *Call, acct jmap.Id, needWrite bool) (errType, description string) {
	if acct == "" {
		return jmap.ErrInvalidArguments, "accountId is required"
	}
	if call.Identity == nil {
		return jmap.ErrAccountNotFound, ""
	}
	access, ok := call.Identity.Accounts[acct]
	if !ok {
		return jmap.ErrAccountNotFound, ""
	}
	if needWrite && access.ReadOnly {
		return jmap.ErrAccountReadOnly, ""
	}
	return "", ""
}

func reply(name, callID string, args any) []jmap.Invocation {
	raw, err := json.Marshal(args)
	if err != nil {
		return fail(callID, jmap.ErrServerFail, err.Error())
	}
	return []jmap.Invocation{{Name: name, Args: raw, CallID: callID}}
}

// ---- Foo/get (section 5.1) ----

type getArgs struct {
	AccountId  jmap.Id    `json:"accountId"`
	Ids        *[]jmap.Id `json:"ids"`
	Properties *[]string  `json:"properties"`
}

type getResponse struct {
	AccountId jmap.Id           `json:"accountId"`
	State     string            `json:"state"`
	List      []objectdb.Object `json:"list"`
	NotFound  []jmap.Id         `json:"notFound"`
}

func (st *stdType) get(ctx context.Context, call *Call) []jmap.Invocation {
	respName := st.t.Name + "/get"
	var a getArgs
	extra, err := st.decodeWithExtras("get", call.Args, &a)
	if err != nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := checkAccount(call, a.AccountId, false); errType != "" {
		return fail(call.CallID, errType, desc)
	}

	// properties: omitted or null means all, unless the type overrides
	// that default with a fixed list (RFC 8621 section 4.2 does for
	// Email); the id property is always returned; an invalid property
	// MUST reject the call with invalidArguments (5.1). Names that are
	// not stored properties may be computed ones (Extensions.Computed).
	reqProps := a.Properties
	if reqProps == nil && st.ext != nil && st.ext.DefaultGetProperties != nil {
		reqProps = &st.ext.DefaultGetProperties
	}
	var props map[string]bool
	var computed []string
	if reqProps != nil {
		props = map[string]bool{"id": true}
		for _, name := range *reqProps {
			if _, declared := st.t.Properties[name]; declared || name == "id" {
				props[name] = true
				continue
			}
			if st.ext != nil && st.ext.Computed != nil && st.ext.Computed.Accepts(name) {
				if !props[name] {
					computed = append(computed, name)
				}
				props[name] = true
				continue
			}
			return fail(call.CallID, jmap.ErrInvalidArguments, fmt.Sprintf("unknown property %q", name))
		}
	}

	maxGet := int(st.core.MaxObjectsInGet)
	var ids []jmap.Id
	if a.Ids == nil {
		// ids null: all records, subject to maxObjectsInGet (5.1).
		all, err := st.db.AllIds(ctx, a.AccountId, st.t.Name, maxGet)
		if err != nil {
			return fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		if len(all) > maxGet {
			return fail(call.CallID, jmap.ErrRequestTooLarge, "")
		}
		ids = all
	} else {
		if len(*a.Ids) > maxGet {
			return fail(call.CallID, jmap.ErrRequestTooLarge, "")
		}
		// An id included more than once appears only once in the response
		// (5.1).
		seen := make(map[jmap.Id]bool, len(*a.Ids))
		for _, id := range *a.Ids {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}

	resp := getResponse{
		AccountId: a.AccountId,
		List:      make([]objectdb.Object, 0, len(ids)),
		NotFound:  make([]jmap.Id, 0),
	}
	for _, id := range ids {
		obj, err := st.db.Get(ctx, a.AccountId, st.t.Name, id)
		if errors.Is(err, objectdb.ErrNotFound) {
			resp.NotFound = append(resp.NotFound, id)
			continue
		}
		if err != nil {
			return fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		var resolved map[string]json.RawMessage
		if len(computed) > 0 {
			resolved, err = st.ext.Computed.Resolve(ctx, a.AccountId, obj, computed, extra)
			if err != nil {
				return fail(call.CallID, jmap.ErrServerFail, err.Error())
			}
		}
		if props != nil {
			filtered := make(objectdb.Object, len(props))
			for name := range props {
				if v, has := obj[name]; has {
					filtered[name] = v
				}
			}
			for _, name := range computed {
				if v, has := resolved[name]; has {
					filtered[name] = v
				}
			}
			obj = filtered
		}
		resp.List = append(resp.List, obj)
	}
	state, err := st.db.TypeState(ctx, a.AccountId, st.t.Name)
	if err != nil {
		return fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	resp.State = state
	return reply(respName, call.CallID, resp)
}

// ---- Foo/changes (section 5.2) ----

type changesArgs struct {
	AccountId  jmap.Id `json:"accountId"`
	SinceState *string `json:"sinceState"`
	MaxChanges *int64  `json:"maxChanges"`
}

type changesResponse struct {
	AccountId      jmap.Id   `json:"accountId"`
	OldState       string    `json:"oldState"`
	NewState       string    `json:"newState"`
	HasMoreChanges bool      `json:"hasMoreChanges"`
	Created        []jmap.Id `json:"created"`
	Updated        []jmap.Id `json:"updated"`
	Destroyed      []jmap.Id `json:"destroyed"`
}

func (st *stdType) changes(ctx context.Context, call *Call) []jmap.Invocation {
	var a changesArgs
	extra, err := st.decodeWithExtras("changes", call.Args, &a)
	if err != nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := checkAccount(call, a.AccountId, false); errType != "" {
		return fail(call.CallID, errType, desc)
	}
	if a.SinceState == nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, "sinceState is required")
	}
	max := 0
	if a.MaxChanges != nil {
		// maxChanges MUST be a positive integer greater than 0 (5.2).
		if *a.MaxChanges <= 0 || !jmap.ValidUnsignedInt(*a.MaxChanges) {
			return fail(call.CallID, jmap.ErrInvalidArguments, "maxChanges must be a positive integer")
		}
		max = int(*a.MaxChanges)
	}
	cs, err := st.db.Changes(ctx, a.AccountId, st.t.Name, *a.SinceState, max)
	if errors.Is(err, objectdb.ErrCannotCalculateChanges) {
		return fail(call.CallID, jmap.ErrCannotCalculateChanges, "")
	}
	if err != nil {
		return fail(call.CallID, jmap.ErrServerFail, err.Error())
	}
	resp := changesResponse{
		AccountId:      a.AccountId,
		OldState:       cs.OldState,
		NewState:       cs.NewState,
		HasMoreChanges: cs.HasMore,
		Created:        emptyIfNil(cs.Created),
		Updated:        emptyIfNil(cs.Updated),
		Destroyed:      emptyIfNil(cs.Destroyed),
	}
	if st.ext != nil && st.ext.ExtraResponse != nil && st.ext.ExtraResponse.Changes != nil {
		view := &ChangesView{
			OldState:       resp.OldState,
			NewState:       resp.NewState,
			HasMoreChanges: resp.HasMoreChanges,
			Created:        resp.Created,
			Updated:        resp.Updated,
			Destroyed:      resp.Destroyed,
			UpdatedProps:   cs.UpdatedProps,
		}
		fields, err := st.ext.ExtraResponse.Changes(ctx, a.AccountId, view, extra)
		if err != nil {
			return fail(call.CallID, jmap.ErrServerFail, err.Error())
		}
		return replyExtra(st.t.Name+"/changes", call.CallID, resp, fields)
	}
	return reply(st.t.Name+"/changes", call.CallID, resp)
}

func emptyIfNil(ids []jmap.Id) []jmap.Id {
	if ids == nil {
		return []jmap.Id{}
	}
	return ids
}

// ---- Foo/set (section 5.3) ----

type setArgs struct {
	AccountId jmap.Id                     `json:"accountId"`
	IfInState *string                     `json:"ifInState"`
	Create    map[jmap.Id]json.RawMessage `json:"create"`
	Update    map[jmap.Id]json.RawMessage `json:"update"`
	Destroy   []jmap.Id                   `json:"destroy"`
}

type setResponse struct {
	AccountId    jmap.Id                     `json:"accountId"`
	OldState     string                      `json:"oldState"`
	NewState     string                      `json:"newState"`
	Created      map[jmap.Id]objectdb.Object `json:"created,omitzero"`
	Updated      map[jmap.Id]objectdb.Object `json:"updated,omitzero"`
	Destroyed    []jmap.Id                   `json:"destroyed,omitzero"`
	NotCreated   map[jmap.Id]*jmap.SetError  `json:"notCreated,omitzero"`
	NotUpdated   map[jmap.Id]*jmap.SetError  `json:"notUpdated,omitzero"`
	NotDestroyed map[jmap.Id]*jmap.SetError  `json:"notDestroyed,omitzero"`
}

var errStateMismatch = errors.New("runtime: ifInState does not match")

func (st *stdType) set(ctx context.Context, call *Call) []jmap.Invocation {
	var a setArgs
	extra, err := st.decodeWithExtras("set", call.Args, &a)
	if err != nil {
		return fail(call.CallID, jmap.ErrInvalidArguments, err.Error())
	}
	if errType, desc := checkAccount(call, a.AccountId, true); errType != "" {
		return fail(call.CallID, errType, desc)
	}
	if int64(len(a.Create)+len(a.Update)+len(a.Destroy)) > st.core.MaxObjectsInSet {
		return fail(call.CallID, jmap.ErrRequestTooLarge, "")
	}

	resp := setResponse{AccountId: a.AccountId}
	// One commit for the whole method call: each record is accepted or
	// rejected individually (rejections land in notCreated/notUpdated/
	// notDestroyed and processing continues, 5.3), but everything accepted
	// becomes visible atomically and under one new state.
	states, err := st.db.Update(ctx, a.AccountId, func(u *objectdb.Update) error {
		state, err := st.db.TypeState(ctx, a.AccountId, st.t.Name)
		if err != nil {
			return err
		}
		resp.OldState = state
		if a.IfInState != nil && *a.IfInState != state {
			return errStateMismatch
		}
		if err := st.processCreates(u, call, a.Create, extra, &resp); err != nil {
			return err
		}
		if err := st.processUpdates(u, call, a.Update, extra, &resp); err != nil {
			return err
		}
		return st.processDestroys(u, a.Destroy, call.CreatedIds, extra, &resp)
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
	return reply(st.t.Name+"/set", call.CallID, resp)
}

// processCreates validates and stages the create map. Creates are
// dependency-ordered so a create referencing another record's creation
// id ("#cid" in an Id-kind property) runs after that record is created,
// whatever the map order (5.3: creates MUST happen before their creation
// ids are referenced within a single call).
func (st *stdType) processCreates(u *objectdb.Update, call *Call, creates map[jmap.Id]json.RawMessage, extra map[string]json.RawMessage, resp *setResponse) error {
	if len(creates) == 0 {
		return nil
	}
	type pendingCreate struct {
		obj  objectdb.Object
		sent map[string]bool
	}
	pending := make(map[jmap.Id]*pendingCreate)

	for _, cid := range sortedIds(mapKeys(creates)) {
		var obj objectdb.Object
		if err := json.Unmarshal(creates[cid], &obj); err != nil {
			resp.notCreated(cid, &jmap.SetError{Type: jmap.SetErrInvalidProperties, Description: "create value is not an object"})
			continue
		}
		var bad []string
		// The client MUST omit server-set properties, including id (5.3).
		if _, has := obj["id"]; has {
			bad = append(bad, "id")
		}
		for name := range obj {
			if name == "id" {
				continue
			}
			p, declared := st.t.Properties[name]
			if !declared || p.ServerSet {
				bad = append(bad, name)
			}
		}
		if len(bad) > 0 {
			resp.notCreated(cid, invalidProperties(bad))
			continue
		}
		sent := make(map[string]bool, len(obj))
		for name := range obj {
			sent[name] = true
		}
		// Omitted properties with a declared default get it; server-set
		// properties get their default as the server-assigned value.
		for name, p := range st.t.Properties {
			if p.Default == nil {
				continue
			}
			if _, has := obj[name]; !has {
				obj[name] = p.Default
			}
		}
		pending[cid] = &pendingCreate{obj: obj, sent: sent}
	}

	for len(pending) > 0 {
		progress := false
		for _, cid := range sortedIds(mapKeys(pending)) {
			pc := pending[cid]
			resolved, _ := resolveIdRefs(st.t, pc.obj, call.CreatedIds)
			if !resolved {
				continue // wait for the referenced create to land
			}
			delete(pending, cid)
			progress = true
			if bad := checkValues(st.t, pc.obj); len(bad) > 0 {
				resp.notCreated(cid, invalidProperties(bad))
				continue
			}
			bad, err := checkBlobRefs(u, st.t, pc.obj)
			if err != nil {
				return err
			}
			if len(bad) > 0 {
				resp.notCreated(cid, invalidProperties(bad))
				continue
			}
			if st.ext != nil && st.ext.Set != nil && st.ext.Set.Validate != nil {
				serr, err := st.ext.Set.Validate(u, nil, pc.obj, extra)
				if err != nil {
					return err
				}
				if serr != nil {
					resp.notCreated(cid, serr)
					continue
				}
			}
			id, err := u.Create(st.t.Name, pc.obj)
			if err != nil {
				return err
			}
			call.CreatedIds[cid] = id
			// The created response carries every property the client did
			// not send: server-set, defaulted, and the id (5.3).
			stored, err := u.Get(st.t.Name, id)
			if err != nil {
				return err
			}
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
		}
		if !progress {
			// The remaining creates reference creation ids that will never
			// resolve: a reference to an invalid record is invalidProperties
			// (5.3).
			for cid, pc := range pending {
				_, unresolved := resolveIdRefs(st.t, pc.obj, call.CreatedIds)
				resp.notCreated(cid, invalidProperties(unresolved))
			}
			return nil
		}
	}
	return nil
}

func (st *stdType) processUpdates(u *objectdb.Update, call *Call, updates map[jmap.Id]json.RawMessage, extra map[string]json.RawMessage, resp *setResponse) error {
	for _, uid := range sortedIds(mapKeys(updates)) {
		realId, ok := resolveIdArg(uid, call.CreatedIds)
		if !ok {
			resp.notUpdated(uid, &jmap.SetError{Type: jmap.SetErrNotFound})
			continue
		}
		current, err := u.Get(st.t.Name, realId)
		if errors.Is(err, objectdb.ErrNotFound) {
			resp.notUpdated(uid, &jmap.SetError{Type: jmap.SetErrNotFound})
			continue
		}
		if err != nil {
			return err
		}
		var patch map[string]json.RawMessage
		if err := json.Unmarshal(updates[uid], &patch); err != nil {
			resp.notUpdated(uid, &jmap.SetError{Type: jmap.SetErrInvalidPatch, Description: "update value is not a PatchObject"})
			continue
		}
		newObj, serr := applyPatch(st.t, current, patch, call.CreatedIds)
		if serr != nil {
			resp.notUpdated(uid, serr)
			continue
		}
		bad, err := checkBlobRefs(u, st.t, newObj)
		if err != nil {
			return err
		}
		if len(bad) > 0 {
			resp.notUpdated(uid, invalidProperties(bad))
			continue
		}
		if st.ext != nil && st.ext.Set != nil && st.ext.Set.Validate != nil {
			serr, err := st.ext.Set.Validate(u, current, newObj, extra)
			if err != nil {
				return err
			}
			if serr != nil {
				resp.notUpdated(uid, serr)
				continue
			}
		}
		if err := u.Put(st.t.Name, realId, newObj); err != nil {
			return err
		}
		if resp.Updated == nil {
			resp.Updated = make(map[jmap.Id]objectdb.Object)
		}
		// The value is any change not explicitly requested, or null; no
		// M0 property changes as a side effect of an update (5.3).
		resp.Updated[realId] = nil
	}
	return nil
}

func (st *stdType) processDestroys(u *objectdb.Update, destroy []jmap.Id, createdIds map[jmap.Id]jmap.Id, extra map[string]json.RawMessage, resp *setResponse) error {
	for _, did := range destroy {
		realId, ok := resolveIdArg(did, createdIds)
		if !ok {
			resp.notDestroyed(did, &jmap.SetError{Type: jmap.SetErrNotFound})
			continue
		}
		if st.ext != nil && st.ext.Set != nil && st.ext.Set.Destroy != nil {
			// The hook sees only records that exist; a missing one gets
			// the plain notFound below.
			if _, err := u.Get(st.t.Name, realId); err == nil {
				serr, err := st.ext.Set.Destroy(u, realId, extra)
				if err != nil {
					return err
				}
				if serr != nil {
					resp.notDestroyed(did, serr)
					continue
				}
			} else if !errors.Is(err, objectdb.ErrNotFound) {
				return err
			}
		}
		err := u.Destroy(st.t.Name, realId)
		if errors.Is(err, objectdb.ErrNotFound) {
			resp.notDestroyed(did, &jmap.SetError{Type: jmap.SetErrNotFound})
			continue
		}
		if err != nil {
			return err
		}
		resp.Destroyed = append(resp.Destroyed, realId)
	}
	return nil
}

// applyPatch validates and applies a PatchObject (5.3): paths are JSON
// Pointers with an implicit leading "/"; violations of the pointer
// restrictions are invalidPatch; property-level violations (unknown,
// wrong kind, changing id/server-set/immutable values) are
// invalidProperties listing every offending property.
func applyPatch(t *descriptor.Type, current objectdb.Object, patch map[string]json.RawMessage, createdIds map[jmap.Id]jmap.Id) (objectdb.Object, *jmap.SetError) {
	paths := make([]string, 0, len(patch))
	for path := range patch {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	// No patch pointer may be the prefix of another (5.3).
	segments := make([][]string, len(paths))
	for i, path := range paths {
		parts := strings.Split(path, "/")
		for j, s := range parts {
			// RFC 6901 unescaping: ~1 first, then ~0.
			parts[j] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
		}
		segments[i] = parts
	}
	for i := range segments {
		for j := i + 1; j < len(segments); j++ {
			if segmentPrefix(segments[i], segments[j]) || segmentPrefix(segments[j], segments[i]) {
				return nil, &jmap.SetError{
					Type:        jmap.SetErrInvalidPatch,
					Description: fmt.Sprintf("pointer %q overlaps pointer %q", paths[i], paths[j]),
				}
			}
		}
	}

	newObj := make(objectdb.Object, len(current))
	for name, v := range current {
		newObj[name] = v
	}
	var invalid []string
	for i, path := range paths {
		if len(segments[i]) > 1 {
			// Every M0 property kind is a scalar, so all parts prior to the
			// last can never exist as objects on the record; when compound
			// kinds arrive this becomes real pointer evaluation (5.3).
			return nil, &jmap.SetError{
				Type:        jmap.SetErrInvalidPatch,
				Description: fmt.Sprintf("pointer %q references inside a non-object property", path),
			}
		}
		name := segments[i][0]
		val := patch[path]
		if name == "id" {
			// Left to the unchanged-value check below.
			if isNull(val) {
				delete(newObj, "id")
			} else {
				newObj["id"] = val
			}
			continue
		}
		p, declared := t.Properties[name]
		if !declared {
			invalid = append(invalid, name)
			continue
		}
		if isNull(val) {
			// null: default if declared, else remove (5.3).
			if p.Default != nil {
				newObj[name] = p.Default
			} else {
				delete(newObj, name)
			}
			continue
		}
		if p.Kind == descriptor.KindId {
			resolved, ok := resolveIdValue(val, createdIds)
			if !ok {
				invalid = append(invalid, name)
				continue
			}
			val = resolved
		}
		if err := p.CheckValue(val); err != nil {
			invalid = append(invalid, name)
			continue
		}
		newObj[name] = val
	}

	// id, server-set, and immutable properties may appear in a patch only
	// with a value identical to the current one (5.3): compare outcomes.
	fixed := []string{"id"}
	for name, p := range t.Properties {
		if p.ServerSet || p.Immutable {
			fixed = append(fixed, name)
		}
	}
	for _, name := range fixed {
		if !jsonEqual(newObj[name], current[name]) {
			invalid = append(invalid, name)
		}
	}
	if len(invalid) > 0 {
		return nil, invalidProperties(invalid)
	}
	return newObj, nil
}

func segmentPrefix(a, b []string) bool {
	if len(a) >= len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// resolveIdRefs substitutes "#creationId" references in Id-kind
// properties in place. It reports false with the offending property
// names while any reference is still unresolved.
func resolveIdRefs(t *descriptor.Type, obj objectdb.Object, createdIds map[jmap.Id]jmap.Id) (bool, []string) {
	var unresolved []string
	for name, p := range t.Properties {
		if p.Kind != descriptor.KindId {
			continue
		}
		raw, has := obj[name]
		if !has {
			continue
		}
		resolved, ok := resolveIdValue(raw, createdIds)
		if !ok {
			unresolved = append(unresolved, name)
			continue
		}
		obj[name] = resolved
	}
	sort.Strings(unresolved)
	return len(unresolved) == 0, unresolved
}

// resolveIdValue maps a raw JSON Id value through the request-wide
// creation-id map when it carries the "#" prefix (5.3). Values without
// the prefix pass through untouched.
func resolveIdValue(raw json.RawMessage, createdIds map[jmap.Id]jmap.Id) (json.RawMessage, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || !strings.HasPrefix(s, "#") {
		return raw, true // not a reference; kind validation decides
	}
	real, ok := createdIds[jmap.Id(s[1:])]
	if !ok {
		return nil, false
	}
	out, err := json.Marshal(real)
	if err != nil {
		return nil, false
	}
	return out, true
}

// resolveIdArg resolves a "#creationId" used as an update key or
// destroy id.
func resolveIdArg(id jmap.Id, createdIds map[jmap.Id]jmap.Id) (jmap.Id, bool) {
	if !strings.HasPrefix(string(id), "#") {
		return id, true
	}
	real, ok := createdIds[id[1:]]
	return real, ok
}

// checkValues validates every property's kind, returning the offending
// names (the mechanical part of invalidProperties, 5.3).
func checkValues(t *descriptor.Type, obj objectdb.Object) []string {
	var bad []string
	for name, raw := range obj {
		if name == "id" {
			continue
		}
		p, declared := t.Properties[name]
		if !declared {
			bad = append(bad, name)
			continue
		}
		if err := p.CheckValue(raw); err != nil {
			bad = append(bad, name)
		}
	}
	sort.Strings(bad)
	return bad
}

// checkBlobRefs verifies every BlobRef property of the final object
// references a blob that exists in the account, returning the offending
// property names. A dangling blobId is invalidProperties (section 5.3:
// "There is a reference to another record (foreign key), and the given
// id does not correspond to a valid record"). Any blobId that exists
// within the account may be used (section 6). Values that fail the kind
// check are skipped; checkValues already reported them.
func checkBlobRefs(u *objectdb.Update, t *descriptor.Type, obj objectdb.Object) ([]string, error) {
	var bad []string
	for name, p := range t.Properties {
		if !p.BlobRef {
			continue
		}
		raw, has := obj[name]
		if !has {
			continue
		}
		var id jmap.Id
		if json.Unmarshal(raw, &id) != nil {
			continue
		}
		ok, err := u.BlobExists(id)
		if err != nil {
			return nil, err
		}
		if !ok {
			bad = append(bad, name)
		}
	}
	sort.Strings(bad)
	return bad, nil
}

func invalidProperties(names []string) *jmap.SetError {
	sort.Strings(names)
	return &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: names}
}

func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// jsonEqual compares two raw values semantically (object key order and
// whitespace do not matter). A nil raw means "absent".
func jsonEqual(a, b json.RawMessage) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func mapKeys[V any](m map[jmap.Id]V) []jmap.Id {
	out := make([]jmap.Id, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func sortedIds(ids []jmap.Id) []jmap.Id {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (r *setResponse) notCreated(id jmap.Id, e *jmap.SetError) {
	if r.NotCreated == nil {
		r.NotCreated = make(map[jmap.Id]*jmap.SetError)
	}
	r.NotCreated[id] = e
}

func (r *setResponse) notUpdated(id jmap.Id, e *jmap.SetError) {
	if r.NotUpdated == nil {
		r.NotUpdated = make(map[jmap.Id]*jmap.SetError)
	}
	r.NotUpdated[id] = e
}

func (r *setResponse) notDestroyed(id jmap.Id, e *jmap.SetError) {
	if r.NotDestroyed == nil {
		r.NotDestroyed = make(map[jmap.Id]*jmap.SetError)
	}
	r.NotDestroyed[id] = e
}
