package mail

// EmailSubmission (RFC 8621 section 7): the submission of an Email for
// delivery, stored as a descriptor type whose records ARE the outbound
// queue - undoStatus "pending" plus the internal nextAttemptAt index is
// the whole queue state, so a restart resumes from durable records. The
// create pipeline lives in submissioncreate.go; this file owns the
// descriptor, the update (cancel) and query semantics, and the section
// 7.5 onSuccess continuation that issues the mandatory implicit
// Email/set.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// TypeEmailSubmission is the EmailSubmission datatype name.
const TypeEmailSubmission = "EmailSubmission"

// undoStatus values (RFC 8621 section 7).
const (
	undoPending  = "pending"
	undoFinal    = "final"
	undoCanceled = "canceled"
)

// EmailSubmissionType returns the section 7 descriptor. Everything but
// undoStatus is immutable or server-set: the only client-driven update is
// the cancel transition, which submissionValidate owns (undoStatus is
// "server set on create", which the create override enforces, but must
// stay client-writable for the cancel). The internal properties are the
// queue mechanics: attempts/nextAttemptAt/claimedAt drive the sending
// worker, and blobId snapshots the message's blob at create - the spec
// says destroying the Email MUST NOT affect the submission (section 7.5),
// and the blob reference keeps GC off the bytes of a pending send.
func EmailSubmissionType() *descriptor.Type {
	emptyList := json.RawMessage(`[]`)
	return &descriptor.Type{
		Name:       TypeEmailSubmission,
		Capability: SubmissionCapabilityURI,
		Properties: map[string]descriptor.Property{
			"identityId": {Kind: descriptor.KindId, Immutable: true},
			"emailId":    {Kind: descriptor.KindId, Immutable: true},
			"threadId":   {Kind: descriptor.KindId, Immutable: true, ServerSet: true},
			// envelope is "Envelope|null" on the wire, but the stored value
			// is always concrete: the server MUST generate one when the
			// client omits it (section 7), and this implementation stores
			// what it will relay.
			"envelope":   {Kind: descriptor.KindObject, Immutable: true},
			"sendAt":     {Kind: descriptor.KindDate, Immutable: true, ServerSet: true},
			"undoStatus": {Kind: descriptor.KindString},
			// deliveryStatus is SUPPORTED (never null): per-recipient
			// status from creation on, updated by the sending worker.
			"deliveryStatus": {Kind: descriptor.KindObject, ServerSet: true},
			// DSN/MDN ingestion is not yet implemented; the lists exist
			// (and stay empty) so the schema is already shaped for it.
			"dsnBlobIds": {Kind: descriptor.KindArray, ServerSet: true, Default: emptyList},
			"mdnBlobIds": {Kind: descriptor.KindArray, ServerSet: true, Default: emptyList},
			// Queue mechanics, invisible to the client.
			"attempts": {Kind: descriptor.KindUnsignedInt, Internal: true},
			// nextAttemptAt is indexed: key order is due order, which IS
			// the queue. It is removed when the submission leaves the
			// queue (canceled or finished), so the index holds exactly the
			// pending work.
			"nextAttemptAt": {Kind: descriptor.KindDate, Internal: true, Indexed: true},
			// claimedAt holds the claim TOKEN "<rfc3339nano>|<nonce>", not a
			// bare date: the whole string is the claim's identity (a per-worker
			// nonce keeps it unique when two clocks collide on the same
			// instant), so it is a string, not KindDate. Not indexed - the due
			// index (nextAttemptAt) carries claim expiry.
			"claimedAt": {Kind: descriptor.KindString, Internal: true},
			"blobId":    {Kind: descriptor.KindId, BlobRef: true, Internal: true},
		},
	}
}

// RegisterEmailSubmission registers the EmailSubmission type with its
// derived methods (RFC 8621 sections 7.1-7.5) and returns the
// SubmissionQueue: the live queue view a SubmissionWorker consumes
// (NewSubmissionWorker). Every commit that creates submissions rings
// the queue, so sending is wired by constructing and running a worker
// over it - a host without one still queues records durably, and a
// worker attached any time later resumes them from its startup pass.
// policy gates
// sending (nil installs the deny-everything StaticSendPolicy); limits
// are enforced verbatim and MUST match the SubmissionAccountCapability
// advertised for the account. The Email and Identity types must be
// registered on the same db (a submission resolves both).
func RegisterEmailSubmission(p *runtime.Processor, db *objectdb.DB, store blob.Store, core jmap.CoreCapabilities, policy SendPolicy, limits SubmissionLimits) (*SubmissionQueue, error) {
	if policy == nil {
		policy = NewStaticSendPolicy()
	}
	q := newSubmissionQueue(db, store)
	creator := submissionCreate{db: db, store: store, policy: policy, limits: limits}
	ext := &runtime.Extensions{
		Methods: []string{"get", "changes", "set", "query", "queryChanges"},
		ExtraArgs: map[string]runtime.MethodArgs{
			"set": {
				Names: []string{"onSuccessUpdateEmail", "onSuccessDestroyEmail"},
				Check: checkOnSuccessArgs,
			},
		},
		Set: &runtime.SetHooks{
			// Updates are the cancel transition only; creation is the
			// submission pipeline (submissioncreate.go), prepared outside
			// the account lease like every other producer. Destroy is
			// plain record removal: it MUST NOT affect the deliveries it
			// represents (section 7.5), and the worker tolerates a
			// claimed record vanishing.
			Validate:      submissionValidate,
			PrepareCreate: creator.prepare,
			CommitCreate:  creator.commit,
			AfterSet:      submissionAfterSet(db, p, q),
		},
		Query: &runtime.QueryHooks{
			Filter: submissionFilter{},
			Sort:   submissionSort{},
		},
	}
	if err := runtime.RegisterStandardTypeExt(p, db, EmailSubmissionType(), core, ext); err != nil {
		return nil, err
	}
	return q, nil
}

// submissionValidate owns the update path: the only legal client change
// is undoStatus "pending" to "canceled" (section 7.5; every other
// property is immutable or server-set, which the descriptor enforces). A
// successful cancel takes the record out of the queue and finalizes its
// deliveryStatus; the changed status is echoed through the updated
// response as a server side effect. Creates never reach this hook - the
// create override owns them.
func submissionValidate(_ *objectdb.Update, old, new objectdb.Object, _ map[string]json.RawMessage) (*jmap.SetError, error) {
	var oldStatus, newStatus string
	json.Unmarshal(old["undoStatus"], &oldStatus)
	json.Unmarshal(new["undoStatus"], &newStatus)
	if newStatus == oldStatus {
		return nil, nil // no-op, including a redundant re-cancel
	}
	if newStatus != undoCanceled {
		return invalidProp("undoStatus", "may only be set to canceled"), nil
	}
	if oldStatus != undoPending {
		return &jmap.SetError{Type: "cannotUnsend", Description: "the message has already been sent"}, nil
	}
	// A claimed submission is in an active transmit (the worker stamps
	// claimedAt before relaying); once bytes may be on the wire the send
	// cannot be recalled.
	if _, claimed := old["claimedAt"]; claimed {
		return &jmap.SetError{Type: "cannotUnsend", Description: "the message is being sent"}, nil
	}
	delete(new, "nextAttemptAt")
	ds, serr := cancelDeliveryStatus(new["deliveryStatus"])
	if serr != nil {
		return serr, nil
	}
	new["deliveryStatus"] = ds
	return nil, nil
}

// deliveryStatusObj is one recipient's DeliveryStatus (section 7).
type deliveryStatusObj struct {
	SmtpReply string `json:"smtpReply"`
	Delivered string `json:"delivered"`
	Displayed string `json:"displayed"`
}

// cancelDeliveryStatus finalizes every recipient of a canceled
// submission: it will never be delivered, and the smtpReply takes the
// section 7 synthetic form since no SMTP exchange happened.
func cancelDeliveryStatus(raw json.RawMessage) (json.RawMessage, *jmap.SetError) {
	var ds map[string]deliveryStatusObj
	if err := json.Unmarshal(raw, &ds); err != nil {
		return nil, &jmap.SetError{Type: jmap.SetErrInvalidProperties, Description: "stored deliveryStatus is corrupt"}
	}
	for rcpt, st := range ds {
		st.SmtpReply = "554 5.0.0 sending canceled at user request"
		st.Delivered = "no"
		ds[rcpt] = st
	}
	return mustJSON(ds), nil
}

// checkOnSuccessArgs shape-checks the section 7.5 extra /set arguments up
// front, so a malformed argument rejects the call as invalidArguments
// before anything runs.
func checkOnSuccessArgs(extra map[string]json.RawMessage) error {
	if raw, has := extra["onSuccessUpdateEmail"]; has && !isNullRaw(raw) {
		var m map[jmap.Id]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("onSuccessUpdateEmail must be an Id[PatchObject] map or null")
		}
	}
	if raw, has := extra["onSuccessDestroyEmail"]; has && !isNullRaw(raw) {
		var ids []jmap.Id
		if err := json.Unmarshal(raw, &ids); err != nil {
			return fmt.Errorf("onSuccessDestroyEmail must be an Id list or null")
		}
	}
	return nil
}

// submissionAfterSet is the section 7.5 continuation: after the
// EmailSubmission/set has been processed, a single implicit Email/set
// MUST be made to perform the changes requested in onSuccessUpdateEmail/
// onSuccessDestroyEmail, its response following the set's under the same
// call id. An entry applies only if its submission's create/update/
// destroy succeeded in this call; entries whose item failed (or that
// reference nothing in the call) request no change, so with nothing
// applicable no implicit call is made. It also rings the queue when
// the call created submissions - here, after the commit, so the bell
// never announces work that could still roll back; the worker's sweep
// reads what and when from the records themselves.
func submissionAfterSet(db *objectdb.DB, p *runtime.Processor, q *SubmissionQueue) func(context.Context, *runtime.Call, *runtime.SetOutcome, map[string]json.RawMessage) []jmap.Invocation {
	return func(ctx context.Context, call *runtime.Call, outcome *runtime.SetOutcome, extra map[string]json.RawMessage) []jmap.Invocation {
		if len(outcome.Created) > 0 {
			q.ring()
		}
		var updates map[jmap.Id]json.RawMessage
		json.Unmarshal(extra["onSuccessUpdateEmail"], &updates)
		var destroys []jmap.Id
		json.Unmarshal(extra["onSuccessDestroyEmail"], &destroys)
		if len(updates) == 0 && len(destroys) == 0 {
			return nil
		}

		succeeded := make(map[jmap.Id]bool)
		for _, id := range outcome.Created {
			succeeded[id] = true
		}
		for _, id := range outcome.Updated {
			succeeded[id] = true
		}
		for id := range outcome.Destroyed {
			succeeded[id] = true
		}
		// emailIdOf resolves a submission reference ("#creationId" or real
		// id) to its Email's id, when that submission succeeded in this
		// call. A destroyed submission's record is gone, so its emailId
		// comes from the outcome's snapshot.
		emailIdOf := func(ref jmap.Id) (jmap.Id, bool, error) {
			id, ok := runtime.ResolveIdArg(ref, call.CreatedIds)
			if !ok || !succeeded[id] {
				return "", false, nil
			}
			var rec objectdb.Object
			if destroyed, has := outcome.Destroyed[id]; has {
				rec = destroyed
			} else {
				var err error
				rec, err = db.Get(ctx, outcome.AccountId, TypeEmailSubmission, id)
				if err != nil {
					return "", false, err
				}
			}
			var emailId jmap.Id
			if json.Unmarshal(rec["emailId"], &emailId) != nil || emailId == "" {
				return "", false, nil
			}
			return emailId, true, nil
		}

		emailUpdates := make(map[jmap.Id]json.RawMessage)
		refs := make([]jmap.Id, 0, len(updates))
		for ref := range updates {
			refs = append(refs, ref)
		}
		sort.Slice(refs, func(i, j int) bool { return refs[i] < refs[j] })
		for _, ref := range refs {
			emailId, ok, err := emailIdOf(ref)
			if err != nil {
				return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
			}
			if ok {
				emailUpdates[emailId] = updates[ref]
			}
		}
		var emailDestroys []jmap.Id
		for _, ref := range destroys {
			emailId, ok, err := emailIdOf(ref)
			if err != nil {
				return runtime.Fail(call.CallID, jmap.ErrServerFail, err.Error())
			}
			if ok {
				emailDestroys = append(emailDestroys, emailId)
			}
		}
		if len(emailUpdates) == 0 && len(emailDestroys) == 0 {
			return nil
		}
		args := map[string]any{"accountId": outcome.AccountId}
		if len(emailUpdates) > 0 {
			args["update"] = emailUpdates
		}
		if len(emailDestroys) > 0 {
			args["destroy"] = emailDestroys
		}
		return p.ImplicitSet(ctx, TypeEmail, args, call)
	}
}

// submissionFilter is the EmailSubmission FilterCondition language
// (section 7.3). Every condition reads the record in hand, so no I/O.
type submissionFilter struct{}

func (submissionFilter) ValidateCondition(name string, value json.RawMessage) error {
	switch name {
	case "identityIds", "emailIds", "threadIds":
		var ids []jmap.Id
		if json.Unmarshal(value, &ids) != nil || ids == nil {
			return fmt.Errorf("%s must be an Id list", name)
		}
	case "undoStatus":
		if _, ok := decodeString(value); !ok {
			return fmt.Errorf("undoStatus must be a String")
		}
	case "before", "after":
		s, ok := decodeString(value)
		if !ok || !jmap.ValidUTCDate(s) {
			return fmt.Errorf("%s must be a UTCDate", name)
		}
	default:
		return runtime.UnsupportedFilterError{Description: fmt.Sprintf("cannot filter on %q", name)}
	}
	return nil
}

func (submissionFilter) MatchCondition(_ context.Context, _ jmap.Id, obj objectdb.Object, name string, value json.RawMessage) (bool, error) {
	switch name {
	case "identityIds", "emailIds", "threadIds":
		prop := map[string]string{
			"identityIds": "identityId", "emailIds": "emailId", "threadIds": "threadId",
		}[name]
		got, _ := decodeString(obj[prop])
		var ids []string
		json.Unmarshal(value, &ids)
		for _, id := range ids {
			if id == got {
				return true, nil
			}
		}
		return false, nil
	case "undoStatus":
		want, _ := decodeString(value)
		got, ok := decodeString(obj["undoStatus"])
		return ok && got == want, nil
	case "before", "after":
		bound, err := parseUTCDateValue(value)
		if err != nil {
			return false, err
		}
		sendAt, err := parseUTCDateValue(obj["sendAt"])
		if err != nil {
			return false, err
		}
		if name == "before" {
			return sendAt.Before(bound), nil
		}
		return !sendAt.Before(bound), nil // same as or after (section 7.3)
	}
	return false, nil
}

// parseUTCDateValue decodes a JSON UTCDate string to a time.
func parseUTCDateValue(raw json.RawMessage) (time.Time, error) {
	s, ok := decodeString(raw)
	if !ok {
		return time.Time{}, fmt.Errorf("not a UTCDate value: %s", raw)
	}
	return time.Parse(time.RFC3339, s)
}

// submissionSort implements the section 7.3 sort properties: emailId,
// threadId, and the send date. The spec's list names the date property
// "sentAt" although the type's property is "sendAt" (an internal
// inconsistency in RFC 8621 section 7.3 - EmailSubmission has no sentAt);
// both names are accepted and mean sendAt.
type submissionSort struct{}

var submissionSortProps = map[string]string{
	"emailId": "emailId", "threadId": "threadId",
	"sendAt": "sendAt", "sentAt": "sendAt",
}

func (submissionSort) ParseSort(_ context.Context, _ jmap.Id, sortArg []json.RawMessage) (func(a, b objectdb.Object) int, string, string) {
	if len(sortArg) == 0 {
		return nil, "", ""
	}
	type cmp struct {
		prop       string
		descending bool
	}
	cmps := make([]cmp, 0, len(sortArg))
	for _, raw := range sortArg {
		var c struct {
			Property    string `json:"property"`
			IsAscending *bool  `json:"isAscending"`
		}
		if err := json.Unmarshal(raw, &c); err != nil || c.Property == "" {
			return nil, jmap.ErrInvalidArguments, "each Comparator needs a property"
		}
		prop, ok := submissionSortProps[c.Property]
		if !ok {
			return nil, jmap.ErrUnsupportedSort, fmt.Sprintf("cannot sort on %q", c.Property)
		}
		cmps = append(cmps, cmp{prop: prop, descending: c.IsAscending != nil && !*c.IsAscending})
	}
	// All three properties are strings whose stored encodings order
	// correctly bytewise (ids, and RFC 3339 UTC dates of the fixed form
	// the create pipeline writes).
	return func(a, b objectdb.Object) int {
		for _, c := range cmps {
			av, _ := decodeString(a[c.prop])
			bv, _ := decodeString(b[c.prop])
			var r int
			switch {
			case av < bv:
				r = -1
			case av > bv:
				r = 1
			}
			if c.descending {
				r = -r
			}
			if r != 0 {
				return r
			}
		}
		return 0
	}, "", ""
}
