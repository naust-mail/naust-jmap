package emailstore

// Thread assignment (RFC 8621 section 3): a flat, date-ordered list of the
// Emails that belong together. Every Email belongs to exactly one Thread.
// Assignment follows the spec's suggested algorithm (a shared message-id
// AND an equal base subject); Threads are never merged. The Thread
// descriptor, its /get computed resolver, and registration stay in the
// root package (root/thread.go); this file is the write-side join logic
// InsertEmail and BuildEmailRecord use.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/private/rawjson"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// ThreadSizeCap bounds how many Emails may share one threadId. It is a resource
// bound, not a spec limit (RFC 8621 section 3 does not mandate the join
// algorithm): resolving a Thread's emailIds scans an index entry per member and
// returns every member id, so an unbounded thread (an attacker sending many
// messages with the same Message-ID and base subject builds one) turns a single
// Thread/get into an unbounded read. Once a thread reaches this many members a
// new member starts a fresh threadId instead of joining. It is set well above
// any real conversation so it never splits legitimate mail, only a same-key
// flood. It is a var, not a const, only so a test can exercise the split
// boundary at a small size.
var ThreadSizeCap = 1024

// assignThread returns the threadId for a newly arriving message
// (section 3). It joins the Thread of the lowest-id existing Email that
// both shares a message-id and has the same base subject; failing that,
// it creates a fresh Thread. Threads are never merged: message-ids that
// span several Threads join only the first, and the rest stay split
// (threadId is immutable). Joining touches the Thread so
// Thread/changes reports the new member.
//
// The join condition (a shared message-id AND an equal base subject) is
// answered from the threadKeys set index: each member is one message-id
// hashed with the base subject, so every id in the index already satisfies
// both conditions and no candidate record is loaded to compare subjects.
// Only the lowest matching id per key is read, not the whole matching set, so
// a message-id shared by a flood of messages costs one index lookup rather
// than a scan that grows with the flood. Only the winning Email is loaded, for
// its threadId.
func assignThread(u *objectdb.Update, headers []message.HeaderField) (jmap.Id, error) {
	base := BaseSubject(emailSubject(headers))
	var best jmap.Id
	found := false
	for _, m := range emailMsgids(headers) {
		id, ok, err := u.LowestMemberId(record.TypeEmail, "threadKeys", threadKey(m, base))
		if err != nil {
			return "", err
		}
		if ok && (!found || id < best) {
			best, found = id, true
		}
	}
	if !found {
		return u.Create(record.TypeThread, objectdb.Object{})
	}
	obj, err := u.Get(record.TypeEmail, best)
	if err != nil {
		return "", err
	}
	s, ok := rawjson.String(obj["threadId"])
	if !ok {
		return "", fmt.Errorf("email record has no valid threadId")
	}
	tid := jmap.Id(s)
	// Thread-size cap. Resolving a Thread's emailIds scans an index entry
	// per member, so an unbounded thread (an attacker sending many messages
	// with the same Message-ID and base subject - the join keys - builds one)
	// turns a single Thread/get into an unbounded read. Once a thread reaches
	// ThreadSizeCap members, a new member starts a fresh threadId instead of
	// joining. The member count comes from the Thread's stored aggregate - one
	// record read, no index scan. The split is one-way: the overflow message
	// never rejoins the full thread. RFC 8621 section 3 states the thread-join
	// algorithm "is not mandated", so a server may cap thread size and split
	// the overflow this way.
	thr, err := u.Get(record.TypeThread, tid)
	if err != nil {
		return "", err
	}
	if statOf(thr).Total >= int64(ThreadSizeCap) {
		return u.Create(record.TypeThread, objectdb.Object{})
	}
	// The identity Put announces the membership change: the computed
	// emailIds gains a member, so Thread/changes must report the Thread
	// updated, and an ordinary Put is what marks the record loud against
	// the counter aggregate's bookkeeping write. The record is already
	// in hand from the cap check.
	if err := u.Put(record.TypeThread, tid, thr); err != nil {
		return "", err
	}
	return tid, nil
}

// threadKeyMembers is the threadKeys set-index value for a message: one
// member per message-id it carries (Message-ID/In-Reply-To/References),
// each hashed with the message's base subject. Two Emails share a member
// exactly when they share a message-id and have an equal base subject -
// the section 3 join condition - so assignThread needs one membership
// lookup per referenced id and no record loads.
func threadKeyMembers(headers []message.HeaderField) []string {
	base := BaseSubject(emailSubject(headers))
	ids := emailMsgids(headers)
	out := make([]string, 0, len(ids))
	for _, m := range ids {
		out = append(out, threadKey(m, base))
	}
	return out
}

// threadKey hashes a (message-id, base subject) pair into one set-index
// member. Hashing bounds the member size (subjects run to the header-value
// cap) and keeps every member a fixed width; the NUL separator is safe
// because neither a parsed message-id nor a sanitized subject contains NUL.
func threadKey(msgid, base string) string {
	sum := sha256.Sum256([]byte(msgid + "\x00" + base))
	return hex.EncodeToString(sum[:])
}

// emailMsgids is the ordered, de-duplicated union of the message-ids in
// an Email's Message-ID, In-Reply-To, and References headers (section 3
// condition 1). It uses the same parser as the stored msgid properties,
// so a lookup key always matches a stored, set-indexed value exactly.
func emailMsgids(headers []message.HeaderField) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range headers {
		switch strings.ToLower(h.Name) {
		case "message-id", "in-reply-to", "references":
			for _, id := range message.MessageIDsForm(h.Value) {
				if !seen[id] {
					seen[id] = true
					out = append(out, id)
				}
			}
		}
	}
	return out
}

// emailSubject is the text form of an Email's last Subject header, or the
// empty string if it has none - the same value stored as the subject
// property, so threading and storage agree.
func emailSubject(headers []message.HeaderField) string {
	last := ""
	found := false
	for _, h := range headers {
		if strings.EqualFold(h.Name, "Subject") {
			last = h.Value
			found = true
		}
	}
	if !found {
		return ""
	}
	return message.TextForm(last)
}
