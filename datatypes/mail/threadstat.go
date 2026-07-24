package mail

// The Thread's stored aggregate: the "stat" internal property on the
// Thread record. The per-Thread Mailbox counters (RFC 8621 section 2.1)
// ask thread-granular questions - does this Thread have a member in
// Mailbox M, does it count as unread for M - whose answers depend on
// every member, not just the Email being changed. Rather than loading
// all members on every insert, flag flip, and move to re-derive them
// (O(thread) record reads per change), the Thread record carries counts
// from which those answers follow directly, maintained by the same
// commit that changes a member. The counts are chosen so the section
// 2.1 trash rules stay a read-time decision: nothing stored depends on
// which Mailbox is the trash, so a role change never invalidates them.

import (
	"encoding/json"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// statProperty names the Thread record property holding the aggregate.
// It is declared Internal, so it never crosses the protocol surface:
// the client-visible Thread stays {id, emailIds}.
const statProperty = "stat"

// threadStat aggregates a Thread's members for counter maintenance.
type threadStat struct {
	// Total is the member count (assignThread's size cap reads it, and
	// zero after a departure means the Thread is done).
	Total int64 `json:"total"`
	// Unread is how many members are unread - neither $seen nor $draft,
	// the section 2.1 definition.
	Unread int64 `json:"unread,omitempty"`
	// Mailboxes holds the per-Mailbox counts, keyed by Mailbox id;
	// zeroed entries are pruned as members leave.
	Mailboxes map[jmap.Id]*threadMbStat `json:"mailboxes,omitempty"`
	// Trash is the trash Mailbox id ("" for none) under which this
	// Thread's contributions were last folded into the stored Mailbox
	// counters. The section 2.1 trash rules make unreadThreads depend on
	// WHICH Mailbox holds the trash role, so when that changes, every
	// Thread's folded contribution is out of date; recording the rules
	// each Thread was counted under lets each one be corrected
	// independently - by the next change that touches it, or by
	// MigrateThreadCounters for Threads nothing touches - instead of one
	// unbounded recount inside the commit that moved the role (role is a
	// client-settable property, so that recount would be arbitrarily
	// large work on demand).
	Trash jmap.Id `json:"trash,omitempty"`
}

// threadMbStat is one Mailbox's slice of a Thread.
type threadMbStat struct {
	// Total is how many members are in this Mailbox.
	Total int64 `json:"total"`
	// Unread is how many members in this Mailbox are unread.
	Unread int64 `json:"unread,omitempty"`
	// OnlyUnread is how many unread members are in this Mailbox and no
	// other. It exists for section 2.1 trash rule 1: an unread Email
	// that is only in the trash must not make its Thread unread
	// anywhere else, and "unread members not solely in the trash" is
	// Unread minus the trash Mailbox's OnlyUnread.
	OnlyUnread int64 `json:"onlyUnread,omitempty"`
}

// statOf decodes a Thread record's aggregate. A Thread without one (as
// assignThread creates it) is all zeroes.
func statOf(thr objectdb.Object) *threadStat {
	st := &threadStat{}
	if raw := thr[statProperty]; raw != nil {
		json.Unmarshal(raw, st)
	}
	if st.Mailboxes == nil {
		st.Mailboxes = map[jmap.Id]*threadMbStat{}
	}
	return st
}

// memberView is one Email's counter-relevant projection: its Mailbox
// membership and unread state, decoded once.
type memberView struct {
	mailboxes map[jmap.Id]bool
	unread    bool
}

// viewOf projects an Email record, nil for a nil object (the absent
// side of a create or destroy).
func viewOf(obj objectdb.Object) *memberView {
	if obj == nil {
		return nil
	}
	return &memberView{mailboxes: mailboxIdsOf(obj), unread: isUnread(obj)}
}

// viewsEqual reports whether two projections are interchangeable for
// counting - when they are, a change cannot move any Thread counter and
// the aggregate need not be touched.
func viewsEqual(a, b *memberView) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.unread != b.unread || len(a.mailboxes) != len(b.mailboxes) {
		return false
	}
	for mb := range a.mailboxes {
		if !b.mailboxes[mb] {
			return false
		}
	}
	return true
}

// apply folds one member projection into the counts, sign +1 for an
// arriving state and -1 for a departing one, pruning zeroed entries.
func (st *threadStat) apply(v *memberView, sign int64) {
	if v == nil {
		return
	}
	st.Total += sign
	if v.unread {
		st.Unread += sign
	}
	for mb := range v.mailboxes {
		m := st.Mailboxes[mb]
		if m == nil {
			m = &threadMbStat{}
			st.Mailboxes[mb] = m
		}
		m.Total += sign
		if v.unread {
			m.Unread += sign
			if len(v.mailboxes) == 1 {
				m.OnlyUnread += sign
			}
		}
		if m.Total <= 0 && m.Unread <= 0 && m.OnlyUnread <= 0 {
			delete(st.Mailboxes, mb)
		}
	}
}

// inMailbox answers section 2.1 totalThreads for one Mailbox: the
// Thread has at least one member in it. No trash adjustment applies.
func (st *threadStat) inMailbox(mb jmap.Id) bool {
	m := st.Mailboxes[mb]
	return m != nil && m.Total > 0
}

// unreadIn answers section 2.1 unreadThreads for one Mailbox. It
// implements the section's "quality implementation" formula (not the
// "simplest solution" it also permits): at least one member in mb and
// at least one member unread, explicitly not necessarily the same
// member, under the trash rules. For the trash Mailbox only its own
// members are considered (rule 2); for any other Mailbox, members only
// in the trash are ignored (rule 1) - and a member in mb is never only
// in the trash, so membership itself needs no adjustment. With no
// trash Mailbox (trash == "") the rules are inert.
func (st *threadStat) unreadIn(mb, trash jmap.Id) bool {
	m := st.Mailboxes[mb]
	if m == nil || m.Total <= 0 {
		return false
	}
	if trash != "" && mb == trash {
		return m.Unread > 0
	}
	unread := st.Unread
	if trash != "" {
		if tm := st.Mailboxes[trash]; tm != nil {
			unread -= tm.OnlyUnread
		}
	}
	return unread > 0
}
