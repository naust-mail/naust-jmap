package mail

// Thread (RFC 8621 section 3): a flat, date-ordered list of the Emails
// that belong together. Every Email belongs to exactly one Thread. The
// Thread record itself stores nothing but its id; emailIds is computed
// from the Emails indexed by threadId. Assignment follows the spec's
// suggested algorithm (a shared message-id AND an equal base subject);
// Threads are never merged.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/message"
)

// TypeThread is the Thread type name.
const TypeThread = "Thread"

// ThreadType returns the Thread descriptor: only the implicit id is
// stored. emailIds is computed on /get from the Email threadId index.
func ThreadType() *descriptor.Type {
	return &descriptor.Type{
		Name:       TypeThread,
		Capability: CapabilityURI,
		Properties: map[string]descriptor.Property{},
	}
}

// RegisterThread registers the Thread type. It must be registered before
// Email, whose delivery/import path creates Thread records.
func RegisterThread(p *runtime.Processor, db *objectdb.DB, core jmap.CoreCapabilities) error {
	ext := &runtime.Extensions{
		// Thread supports only Thread/get and Thread/changes (RFC 8621
		// section 3); it has no set, copy, or query.
		Methods:              []string{"get", "changes"},
		DefaultGetProperties: []string{"emailIds"},
		Computed:             threadComputed{db: db},
	}
	return runtime.RegisterStandardTypeExt(p, db, ThreadType(), core, ext)
}

// threadComputed resolves emailIds: the Thread's Emails sorted by
// receivedAt oldest first, id as the stable tiebreak (section 3).
type threadComputed struct{ db *objectdb.DB }

func (threadComputed) Accepts(name string) bool { return name == "emailIds" }

func (tc threadComputed) Resolve(ctx context.Context, acct jmap.Id, stored objectdb.Object, names []string, _ map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, 1)
	for _, name := range names {
		if name != "emailIds" {
			continue
		}
		var tid jmap.Id
		if err := json.Unmarshal(stored["id"], &tid); err != nil {
			return nil, err
		}
		ids, err := tc.db.IdsWhereEqual(ctx, acct, TypeEmail, "threadId", mustJSON(tid))
		if err != nil {
			return nil, err
		}
		type entry struct {
			id   jmap.Id
			recv time.Time
		}
		entries := make([]entry, 0, len(ids))
		for _, id := range ids {
			obj, err := tc.db.Get(ctx, acct, TypeEmail, id)
			if err != nil {
				return nil, err
			}
			var recv string
			json.Unmarshal(obj["receivedAt"], &recv)
			t, _ := time.Parse(time.RFC3339, recv)
			entries = append(entries, entry{id: id, recv: t})
		}
		sort.Slice(entries, func(i, j int) bool {
			if !entries[i].recv.Equal(entries[j].recv) {
				return entries[i].recv.Before(entries[j].recv)
			}
			return entries[i].id < entries[j].id
		})
		ordered := make([]jmap.Id, len(entries))
		for i, e := range entries {
			ordered[i] = e.id
		}
		out["emailIds"] = mustJSON(ordered)
	}
	return out, nil
}

// threadSizeCap bounds how many Emails may share one threadId. It is a resource
// bound, not a spec limit (RFC 8621 section 3 does not mandate the join
// algorithm): adjustCounters rescans a thread's members on every insert, so an
// unbounded thread (an attacker sending many messages with the same Message-ID
// and base subject builds one) makes insertion O(N^2) overall. Once a thread
// reaches this many members a new member starts a fresh threadId instead of
// joining. It is set well above any real conversation so it never splits
// legitimate mail, only a same-key flood. It is a var, not a const, only so a
// test can exercise the split boundary at a small size.
var threadSizeCap = 1024

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
	base := baseSubject(emailSubject(headers))
	var best jmap.Id
	found := false
	for _, m := range emailMsgids(headers) {
		id, ok, err := u.LowestMemberId(TypeEmail, "threadKeys", threadKey(m, base))
		if err != nil {
			return "", err
		}
		if ok && (!found || id < best) {
			best, found = id, true
		}
	}
	if !found {
		return u.Create(TypeThread, objectdb.Object{})
	}
	obj, err := u.Get(TypeEmail, best)
	if err != nil {
		return "", err
	}
	var tid jmap.Id
	if err := json.Unmarshal(obj["threadId"], &tid); err != nil {
		return "", err
	}
	// Thread-size cap. adjustCounters rescans a thread's members on every
	// insert, so an unbounded thread (an attacker sending many messages with
	// the same Message-ID and base subject - the join keys - builds one) makes
	// insertion O(thread) per message and O(N^2) overall. Once a thread reaches
	// threadSizeCap members, a new member starts a fresh threadId instead of
	// joining, bounding every per-insert thread scan to the cap. The split is
	// one-way: the overflow message never rejoins the full thread. RFC 8621
	// section 3 states the thread-join algorithm "is not mandated", so a server
	// may cap thread size and split the overflow this way.
	members, err := u.IdsWhereEqual(TypeEmail, "threadId", mustJSON(tid))
	if err != nil {
		return "", err
	}
	if len(members) >= threadSizeCap {
		return u.Create(TypeThread, objectdb.Object{})
	}
	if err := touchThread(u, tid); err != nil {
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
	base := baseSubject(emailSubject(headers))
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

// touchThread re-stages the Thread record so a membership change (a new
// or departed Email) surfaces as an update in Thread/changes; the record
// carries no data of its own to change.
func touchThread(u *objectdb.Update, tid jmap.Id) error {
	obj, err := u.Get(TypeThread, tid)
	if err != nil {
		return err
	}
	return u.Put(TypeThread, tid, obj)
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

// storedSubject decodes a stored Email's subject property (a String or
// null) to its text value.
func storedSubject(obj objectdb.Object) string {
	var s string
	if raw := obj["subject"]; raw != nil && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}
