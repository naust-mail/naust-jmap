package mail

// The shared Email mutation engine: assigning a Thread and inserting a
// record, and keeping the four Mailbox counters (RFC 8621 section 2.1)
// correct on every create, metadata change, and destroy. Delivery,
// Email/import, and Email/copy all reach an Email into the store through
// insertEmail; Email/set reaches the metadata changes through the Set
// hooks. Counters are stored incremental state: each change applies a
// delta rather than recounting the account.

import (
	"encoding/json"
	"errors"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// insertEmail assigns the Thread and stores the Email record, updating
// the Thread membership and every affected Mailbox counter in the same
// commit. It is the one path every message-consuming caller shares.
func insertEmail(u *objectdb.Update, p *parsed, meta emailMeta) (jmap.Id, error) {
	tid, err := assignThread(u, p.msg.Headers)
	if err != nil {
		return "", err
	}
	meta.ThreadID = tid
	id, err := u.Create(TypeEmail, buildEmailRecord(p, meta))
	if err != nil {
		return "", err
	}
	// A new Email was added to the store: advance the EmailDelivery push
	// state (RFC 8621 section 1.5). This runs only here, on add - the
	// Email/set metadata and destroy hooks do not bump it, so a read/flag/
	// delete never moves EmailDelivery, matching the 1.5 contract.
	if err := u.BumpState(TypeEmailDelivery); err != nil {
		return "", err
	}
	stored, err := u.Get(TypeEmail, id)
	if err != nil {
		return "", err
	}
	if err := adjustCounters(u, nil, stored); err != nil {
		return "", err
	}
	return id, nil
}

// emailDestroy is the Email/set destroy hook: it removes the Email from
// its Thread and rolls back its counter contribution before the runtime
// removes the record. It never rejects (Emails have no destroy
// precondition of their own).
func emailDestroy(u *objectdb.Update, id jmap.Id, _ map[string]json.RawMessage) (*jmap.SetError, error) {
	old, err := u.Get(TypeEmail, id)
	if err != nil {
		return nil, err
	}
	if err := adjustCounters(u, old, nil); err != nil {
		return nil, err
	}
	if err := removeEmailFromThread(u, old, id); err != nil {
		return nil, err
	}
	return nil, nil
}

// ctrDelta accumulates pending changes to one Mailbox's four counters.
type ctrDelta struct {
	totalEmails, unreadEmails, totalThreads, unreadThreads int64
}

func (d ctrDelta) zero() bool { return d == ctrDelta{} }

// adjustCounters applies the counter effect of one Email transitioning
// from old to new (old nil = create, new nil = destroy) across every
// Mailbox the change can touch, in the same commit. Per-Email counters
// (totalEmails, unreadEmails) move by the Email's own mailbox membership
// and read state; per-Thread counters (totalThreads, unreadThreads) are
// recomputed for the Thread from the other Emails plus this one's before
// and after state, so the trash-aware unreadThreads rules hold.
func adjustCounters(u *objectdb.Update, old, new objectdb.Object) error {
	deltas := map[jmap.Id]*ctrDelta{}
	at := func(mb jmap.Id) *ctrDelta {
		d := deltas[mb]
		if d == nil {
			d = &ctrDelta{}
			deltas[mb] = d
		}
		return d
	}

	// Per-Email counters: subtract the old membership, add the new.
	applyEmail := func(obj objectdb.Object, sign int64) {
		if obj == nil {
			return
		}
		unread := int64(0)
		if isUnread(obj) {
			unread = 1
		}
		for mb := range mailboxIdsOf(obj) {
			d := at(mb)
			d.totalEmails += sign
			d.unreadEmails += sign * unread
		}
	}
	applyEmail(old, -1)
	applyEmail(new, +1)

	// Per-Thread counters: recompute the Thread's per-Mailbox
	// contribution before and after this Email's change.
	tid := threadIdOf(new)
	if tid == "" {
		tid = threadIdOf(old)
	}
	if tid != "" {
		trash, err := trashMailboxId(u)
		if err != nil {
			return err
		}
		var eid jmap.Id
		if new != nil {
			json.Unmarshal(new["id"], &eid)
		} else {
			json.Unmarshal(old["id"], &eid)
		}
		base, err := threadEmails(u, tid)
		if err != nil {
			return err
		}
		baseViews := threadMemberViews(base)
		var oldView, newView *threadMemberView
		if old != nil {
			v := newThreadMemberView(old)
			oldView = &v
		}
		if new != nil {
			v := newThreadMemberView(new)
			newView = &v
		}
		before := substituteView(baseViews, eid, oldView)
		after := substituteView(baseViews, eid, newView)
		for mb := range affectedMailboxes(before, after) {
			d := at(mb)
			d.totalThreads += b2i(threadInMailbox(after, mb)) - b2i(threadInMailbox(before, mb))
			d.unreadThreads += b2i(threadUnread(after, mb, trash)) - b2i(threadUnread(before, mb, trash))
		}
	}

	return applyDeltas(u, deltas)
}

// applyDeltas folds each non-zero Mailbox delta into the stored counters
// in the same commit. A Mailbox that no longer exists (destroyed in this
// commit) is skipped; its counters are moot.
func applyDeltas(u *objectdb.Update, deltas map[jmap.Id]*ctrDelta) error {
	for mb, d := range deltas {
		if d.zero() {
			continue
		}
		obj, err := u.Get(TypeMailbox, mb)
		if errors.Is(err, objectdb.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		next := cloneObject(obj)
		addCounter(next, "totalEmails", d.totalEmails)
		addCounter(next, "unreadEmails", d.unreadEmails)
		addCounter(next, "totalThreads", d.totalThreads)
		addCounter(next, "unreadThreads", d.unreadThreads)
		var id jmap.Id
		json.Unmarshal(next["id"], &id)
		if err := u.Put(TypeMailbox, id, next); err != nil {
			return err
		}
	}
	return nil
}

// threadEmails returns the Thread's Emails as this Update sees them,
// keyed by id (staged-aware, so earlier changes in this commit show).
func threadEmails(u *objectdb.Update, tid jmap.Id) (map[jmap.Id]objectdb.Object, error) {
	ids, err := u.IdsWhereEqual(TypeEmail, "threadId", mustJSON(tid))
	if err != nil {
		return nil, err
	}
	return u.GetMany(TypeEmail, ids)
}

// threadMemberView is one Thread member's mailbox membership and unread
// state, decoded once from its Email record. Without this, adjustCounters
// re-decodes the same Email's mailboxIds and keywords JSON on every
// Mailbox it is checked against - threadInMailbox and threadUnread each
// look at both the before and after picture, so a Thread with N members
// and M affected Mailboxes redecoded up to 4*M*N times for a single
// insert. threadMemberViews below decodes each member exactly once
// instead.
type threadMemberView struct {
	mailboxes map[jmap.Id]bool
	unread    bool
}

func newThreadMemberView(obj objectdb.Object) threadMemberView {
	return threadMemberView{mailboxes: mailboxIdsOf(obj), unread: isUnread(obj)}
}

// threadMemberViews decodes every member of a threadEmails result once,
// up front, for substituteView to build the before/after pictures from
// without any further JSON decoding.
func threadMemberViews(view map[jmap.Id]objectdb.Object) map[jmap.Id]threadMemberView {
	out := make(map[jmap.Id]threadMemberView, len(view))
	for id, obj := range view {
		out[id] = newThreadMemberView(obj)
	}
	return out
}

// substituteView copies base with eid forced to v (removed when v is
// nil): the before/after picture of the Thread for the changing Email.
func substituteView(base map[jmap.Id]threadMemberView, eid jmap.Id, v *threadMemberView) map[jmap.Id]threadMemberView {
	out := make(map[jmap.Id]threadMemberView, len(base)+1)
	for id, mv := range base {
		if id == eid {
			continue
		}
		out[id] = mv
	}
	if v != nil {
		out[eid] = *v
	}
	return out
}

// affectedMailboxes is the set of Mailboxes any Email in either picture
// belongs to: the only Mailboxes whose Thread counters can move.
func affectedMailboxes(before, after map[jmap.Id]threadMemberView) map[jmap.Id]bool {
	out := map[jmap.Id]bool{}
	for _, view := range []map[jmap.Id]threadMemberView{before, after} {
		for _, mv := range view {
			for mb := range mv.mailboxes {
				out[mb] = true
			}
		}
	}
	return out
}

// threadInMailbox reports whether the Thread counts toward totalThreads
// for mb: at least one Email in it is in mb (section 2.1, no trash
// adjustment).
func threadInMailbox(view map[jmap.Id]threadMemberView, mb jmap.Id) bool {
	for _, mv := range view {
		if mv.mailboxes[mb] {
			return true
		}
	}
	return false
}

// threadUnread reports whether the Thread counts toward unreadThreads for
// mb (the quality, trash-aware definition, section 2.1): among the Emails
// considered for mb, at least one is in mb and at least one is unread.
// The unread Email need not be the one in mb. Trash handling: for the
// trash Mailbox only its own Emails are considered; for any other Mailbox
// Emails that are only in the trash are ignored.
func threadUnread(view map[jmap.Id]threadMemberView, mb, trash jmap.Id) bool {
	inMailbox, anyUnread := false, false
	for _, mv := range view {
		if !consideredForUnread(mv, mb, trash) {
			continue
		}
		if mv.mailboxes[mb] {
			inMailbox = true
		}
		if mv.unread {
			anyUnread = true
		}
	}
	return inMailbox && anyUnread
}

// consideredForUnread applies the trash rules to decide whether mv
// counts when computing unreadThreads for mb. With no trash Mailbox the
// rules are inert and every Email is considered (the count degrades to
// the simple definition naturally).
func consideredForUnread(mv threadMemberView, mb, trash jmap.Id) bool {
	if trash != "" && mb == trash {
		// Trash Mailbox: ignore Emails that are not in the trash (rule 2).
		return mv.mailboxes[trash]
	}
	if trash != "" && len(mv.mailboxes) == 1 && mv.mailboxes[trash] {
		// Other Mailbox: ignore Emails only in the trash (rule 1).
		return false
	}
	return true
}

// removeEmailFromThread destroys or touches the Email's Thread as it
// leaves: the Thread is destroyed when this was its last Email, otherwise
// touched so Thread/changes reports the departed member.
func removeEmailFromThread(u *objectdb.Update, old objectdb.Object, id jmap.Id) error {
	var tid jmap.Id
	if err := json.Unmarshal(old["threadId"], &tid); err != nil {
		return err
	}
	ids, err := u.IdsWhereEqual(TypeEmail, "threadId", mustJSON(tid))
	if err != nil {
		return err
	}
	for _, oid := range ids {
		if oid != id {
			return touchThread(u, tid)
		}
	}
	return u.Destroy(TypeThread, tid)
}

// mailboxRemoveEmails carries out the onDestroyRemoveEmails cascade (RFC
// 8621 section 2.5): every Email in the Mailbox being destroyed is
// removed from it, and any Email left in no Mailbox at all is destroyed.
// Counters and Thread membership are maintained per Email, in the same
// commit, so the Mailboxes the Emails also belonged to stay correct.
func mailboxRemoveEmails(u *objectdb.Update, mbID jmap.Id) error {
	ids, err := u.IdsWhereMember(TypeEmail, "mailboxIds", string(mbID))
	if err != nil {
		return err
	}
	for _, eid := range ids {
		old, err := u.Get(TypeEmail, eid)
		if err != nil {
			return err
		}
		mbs := mailboxIdsOf(old)
		delete(mbs, mbID)
		if len(mbs) == 0 {
			if err := adjustCounters(u, old, nil); err != nil {
				return err
			}
			if err := removeEmailFromThread(u, old, eid); err != nil {
				return err
			}
			if err := u.Destroy(TypeEmail, eid); err != nil {
				return err
			}
			continue
		}
		next := cloneObject(old)
		next["mailboxIds"] = mailboxIdsJSON(mbs)
		if err := adjustCounters(u, old, next); err != nil {
			return err
		}
		if err := u.Put(TypeEmail, eid, next); err != nil {
			return err
		}
	}
	return nil
}

// cloneObject is a shallow copy of an object's property map, safe to
// mutate without disturbing the staged or committed original.
func cloneObject(obj objectdb.Object) objectdb.Object {
	next := make(objectdb.Object, len(obj))
	for k, v := range obj {
		next[k] = v
	}
	return next
}

// mailboxIdsJSON encodes a Mailbox id set as the Id[Boolean] object.
func mailboxIdsJSON(set map[jmap.Id]bool) json.RawMessage {
	m := make(map[string]bool, len(set))
	for id := range set {
		m[string(id)] = true
	}
	return mustJSON(m)
}

// trashMailboxId is the account's trash Mailbox id, or "" if it has none
// (role is unique per account, so there is at most one).
func trashMailboxId(u *objectdb.Update) (jmap.Id, error) {
	ids, err := u.IdsWhereEqual(TypeMailbox, "role", json.RawMessage(`"trash"`))
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", nil
	}
	return ids[0], nil
}

// isUnread reports whether an Email has neither the $seen nor the $draft
// keyword (RFC 8621 section 2.1 unreadEmails definition).
func isUnread(obj objectdb.Object) bool {
	kw := objectKeys(obj["keywords"])
	return !kw["$seen"] && !kw["$draft"]
}

// mailboxIdsOf is the set of Mailbox ids an Email belongs to.
func mailboxIdsOf(obj objectdb.Object) map[jmap.Id]bool {
	keys := objectKeys(obj["mailboxIds"])
	out := make(map[jmap.Id]bool, len(keys))
	for k := range keys {
		out[jmap.Id(k)] = true
	}
	return out
}

// objectKeys decodes a KindObject value to the set of its keys.
func objectKeys(raw json.RawMessage) map[string]bool {
	var m map[string]json.RawMessage
	json.Unmarshal(raw, &m)
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

// threadIdOf decodes an Email's threadId, "" when the object is nil.
func threadIdOf(obj objectdb.Object) jmap.Id {
	if obj == nil {
		return ""
	}
	var tid jmap.Id
	json.Unmarshal(obj["threadId"], &tid)
	return tid
}

// addCounter adds delta to an UnsignedInt counter property, clamping at
// zero defensively (a correct delta never drives one negative).
func addCounter(obj objectdb.Object, name string, delta int64) {
	if delta == 0 {
		return
	}
	var n int64
	json.Unmarshal(obj[name], &n)
	n += delta
	if n < 0 {
		n = 0
	}
	obj[name] = mustJSON(n)
}

// b2i is 1 for true, 0 for false.
func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
