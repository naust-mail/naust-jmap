package emailstore

// The shared Email mutation engine: assigning a Thread and inserting a
// record, and keeping the four Mailbox counters (RFC 8621 section 2.1)
// correct on every create, metadata change, and destroy. Delivery,
// Email/import, and Email/copy all reach an Email into the store through
// InsertEmail; Email/set reaches the metadata changes through the Set
// hooks. Counters are stored incremental state: each change applies a
// delta rather than recounting the account, and the thread-granular
// counters read the Thread's stored aggregate (threadstat.go) rather
// than the Thread's member Emails, so a change costs the same however
// large its Thread is.

import (
	"encoding/json"
	"errors"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/private/rawjson"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// InsertEmail assigns the Thread and stores the Email record, updating
// the Thread membership and every affected Mailbox counter in the same
// commit. It is the one path every message-consuming caller shares.
func InsertEmail(u *objectdb.Update, p *parse.Parsed, meta EmailMeta) (jmap.Id, error) {
	tid, err := assignThread(u, p.Msg.Headers)
	if err != nil {
		return "", err
	}
	meta.ThreadID = tid
	id, err := u.Create(record.TypeEmail, BuildEmailRecord(p, meta))
	if err != nil {
		return "", err
	}
	// A new Email was added to the store: advance the EmailDelivery push
	// state (RFC 8621 section 1.5). This runs only here, on add - the
	// Email/set metadata and destroy hooks do not bump it, so a read/flag/
	// delete never moves EmailDelivery, matching the 1.5 contract.
	if err := u.BumpState(record.TypeEmailDelivery); err != nil {
		return "", err
	}
	stored, err := u.Get(record.TypeEmail, id)
	if err != nil {
		return "", err
	}
	if err := AdjustCounters(u, nil, stored); err != nil {
		return "", err
	}
	return id, nil
}

// EmailDestroy is the Email/set destroy hook: it removes the Email from
// its Thread and rolls back its counter contribution before the runtime
// removes the record. It never rejects (Emails have no destroy
// precondition of their own).
func EmailDestroy(u *objectdb.Update, id jmap.Id, _ map[string]json.RawMessage) (*jmap.SetError, error) {
	old, err := u.Get(record.TypeEmail, id)
	if err != nil {
		return nil, err
	}
	if err := AdjustCounters(u, old, nil); err != nil {
		return nil, err
	}
	if err := removeEmailFromThread(u, old); err != nil {
		return nil, err
	}
	return nil, nil
}

// ctrDelta accumulates pending changes to one Mailbox's four counters.
type ctrDelta struct {
	totalEmails, unreadEmails, totalThreads, unreadThreads int64
}

func (d ctrDelta) zero() bool { return d == ctrDelta{} }

// AdjustCounters applies the counter effect of one Email transitioning
// from old to new (old nil = create, new nil = destroy) across every
// Mailbox the change can touch, in the same commit. Per-Email counters
// (totalEmails, unreadEmails) move by the Email's own mailbox membership
// and read state. Per-Thread counters (totalThreads, unreadThreads) are
// answered from the Thread's stored aggregate (threadstat.go): the one
// changing Email's delta updates the aggregate and the counter movements
// fall out of the flag transitions, with no member Email loaded. The
// aggregate write is bookkeeping (PutInternal): it never surfaces in
// Thread/changes, because the client-visible Thread is unchanged by a
// flag or mailbox change. Membership changes, which do change the
// computed emailIds, are announced by an identity Put of the Thread at
// the insert and destroy sites.
func AdjustCounters(u *objectdb.Update, old, new objectdb.Object) error {
	deltas := map[jmap.Id]*ctrDelta{}
	at := func(mb jmap.Id) *ctrDelta {
		d := deltas[mb]
		if d == nil {
			d = &ctrDelta{}
			deltas[mb] = d
		}
		return d
	}

	// Each side's keywords and mailboxIds are decoded once, into the same
	// projection the Thread aggregate consumes below - this is the hottest
	// decode in the mutation path, so it must not run twice per object.
	oldView, newView := viewOf(old), viewOf(new)

	// Per-Email counters: subtract the old membership, add the new.
	applyEmail := func(v *memberView, sign int64) {
		if v == nil {
			return
		}
		unread := B2i(v.unread)
		for mb := range v.mailboxes {
			d := at(mb)
			d.totalEmails += sign
			d.unreadEmails += sign * unread
		}
	}
	applyEmail(oldView, -1)
	applyEmail(newView, +1)

	tid := ThreadIdOf(new)
	if tid == "" {
		tid = ThreadIdOf(old)
	}
	if tid != "" && !viewsEqual(oldView, newView) {
		trash, err := record.RoleMailboxId(u, "trash")
		if err != nil {
			return err
		}
		thr, err := u.Get(record.TypeThread, tid)
		if err != nil {
			return err
		}
		st := statOf(thr)
		// The Mailboxes whose flags can move: every Mailbox the Thread
		// currently touches (a global unread transition can flip
		// unreadThreads for all of them) plus any the change enters.
		affected := map[jmap.Id]bool{}
		for mb := range st.Mailboxes {
			affected[mb] = true
		}
		for _, v := range []*memberView{oldView, newView} {
			if v == nil {
				continue
			}
			for mb := range v.mailboxes {
				affected[mb] = true
			}
		}
		// The before flags are evaluated under the trash rules the stored
		// counters were folded with (st.Trash), the after flags under the
		// current rules: when the trash role moved since this Thread was
		// last counted, the difference carries the correction for the
		// rule change along with the member's own delta, and the stamp
		// records that this Thread is now up to date (see threadStat.Trash).
		type flagPair struct{ in, unread bool }
		before := make(map[jmap.Id]flagPair, len(affected))
		for mb := range affected {
			before[mb] = flagPair{in: st.inMailbox(mb), unread: st.unreadIn(mb, st.Trash)}
		}
		st.apply(oldView, -1)
		st.apply(newView, +1)
		st.Trash = trash
		for mb := range affected {
			d := at(mb)
			d.totalThreads += B2i(st.inMailbox(mb)) - B2i(before[mb].in)
			d.unreadThreads += B2i(st.unreadIn(mb, trash)) - B2i(before[mb].unread)
		}
		next := cloneObject(thr)
		next[StatProperty] = record.MustJSON(st)
		if err := u.PutInternal(record.TypeThread, tid, next); err != nil {
			return err
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
		obj, err := u.Get(record.TypeMailbox, mb)
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
		if err := u.Put(record.TypeMailbox, id, next); err != nil {
			return err
		}
	}
	return nil
}

// removeEmailFromThread destroys or touches the Email's Thread as it
// leaves: the Thread is destroyed when this was its last Email,
// otherwise touched so Thread/changes reports the departed member. It
// runs after AdjustCounters has applied the departure, so the stored
// aggregate already excludes this Email and a zero member count means
// the Thread is done - no member scan decides this.
func removeEmailFromThread(u *objectdb.Update, old objectdb.Object) error {
	var tid jmap.Id
	if err := json.Unmarshal(old["threadId"], &tid); err != nil {
		return err
	}
	thr, err := u.Get(record.TypeThread, tid)
	if err != nil {
		return err
	}
	if statOf(thr).Total > 0 {
		// Identity Put: the departed member changed the computed
		// emailIds, so Thread/changes must report the Thread updated -
		// an ordinary Put marks the record loud against the counter
		// aggregate's bookkeeping write, and the record is already in
		// hand.
		return u.Put(record.TypeThread, tid, thr)
	}
	return u.Destroy(record.TypeThread, tid)
}

// MailboxRemoveEmails carries out the onDestroyRemoveEmails cascade (RFC
// 8621 section 2.5): every Email in the Mailbox being destroyed is
// removed from it, and any Email left in no Mailbox at all is destroyed.
// Counters and Thread membership are maintained per Email, in the same
// commit, so the Mailboxes the Emails also belonged to stay correct.
func MailboxRemoveEmails(u *objectdb.Update, mbID jmap.Id) error {
	ids, err := u.IdsWhereMember(record.TypeEmail, "mailboxIds", string(mbID))
	if err != nil {
		return err
	}
	for _, eid := range ids {
		old, err := u.Get(record.TypeEmail, eid)
		if err != nil {
			return err
		}
		mbs := MailboxIdsOf(old)
		delete(mbs, mbID)
		if len(mbs) == 0 {
			if err := AdjustCounters(u, old, nil); err != nil {
				return err
			}
			if err := removeEmailFromThread(u, old); err != nil {
				return err
			}
			if err := u.Destroy(record.TypeEmail, eid); err != nil {
				return err
			}
			continue
		}
		next := cloneObject(old)
		next["mailboxIds"] = MailboxIdsJSON(mbs)
		if err := AdjustCounters(u, old, next); err != nil {
			return err
		}
		if err := u.Put(record.TypeEmail, eid, next); err != nil {
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

// MailboxIdsJSON encodes a Mailbox id set as the Id[Boolean] object.
func MailboxIdsJSON(set map[jmap.Id]bool) json.RawMessage {
	m := make(map[string]bool, len(set))
	for id := range set {
		m[string(id)] = true
	}
	return record.MustJSON(m)
}

// isUnread reports whether an Email has neither the $seen nor the $draft
// keyword (RFC 8621 section 2.1 unreadEmails definition).
func isUnread(obj objectdb.Object) bool {
	kw := ObjectKeys(obj["keywords"])
	return !kw["$seen"] && !kw["$draft"]
}

// MailboxIdsOf is the set of Mailbox ids an Email belongs to. Built
// directly off EachKey rather than through ObjectKeys: viewOf calls this
// for both sides of every counter-affecting change, and going through
// ObjectKeys' map[string]bool first would mean building and discarding
// a second map on every call just to retype its keys.
func MailboxIdsOf(obj objectdb.Object) map[jmap.Id]bool {
	out := make(map[jmap.Id]bool, 4)
	if rawjson.EachKey(obj["mailboxIds"], func(k string) { out[jmap.Id(k)] = true }) != nil {
		return map[jmap.Id]bool{}
	}
	return out
}

// ObjectKeys decodes a KindObject value to the set of its keys. A nil,
// null, or malformed value is the empty set, as when this decoded
// through a map with the error ignored.
func ObjectKeys(raw json.RawMessage) map[string]bool {
	out := make(map[string]bool, 4)
	if rawjson.EachKey(raw, func(k string) { out[k] = true }) != nil {
		return map[string]bool{}
	}
	return out
}

// HasKey reports whether a KindObject value has the given key, without
// building the key set ObjectKeys would - a membership test on the hot
// query path allocates nothing this way. A nil, null, or malformed
// value has no keys, matching ObjectKeys.
func HasKey(raw json.RawMessage, key string) bool {
	ok, err := rawjson.HasKey(raw, key)
	return err == nil && ok
}

// ThreadIdOf decodes an Email's threadId, "" when the object is nil.
func ThreadIdOf(obj objectdb.Object) jmap.Id {
	if obj == nil {
		return ""
	}
	s, _ := rawjson.String(obj["threadId"])
	return jmap.Id(s)
}

// addCounter adds delta to an UnsignedInt counter property, clamping at
// zero defensively (a correct delta never drives one negative).
func addCounter(obj objectdb.Object, name string, delta int64) {
	if delta == 0 {
		return
	}
	n, _ := rawjson.Int(obj[name])
	n += delta
	if n < 0 {
		n = 0
	}
	obj[name] = record.MustJSON(n)
}

// B2i is 1 for true, 0 for false.
func B2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
