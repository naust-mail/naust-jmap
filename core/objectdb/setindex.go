package objectdb

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
)

// The set index is the composite counterpart of the scalar property
// index: a SetIndexed KindObject or KindArray property has one index
// entry per member, so "which records contain member X" is a range
// scan. It shares the "x" index keyspace with scalar indexes - a member
// takes the place of a scalar value - which is safe because a property
// is Indexed or SetIndexed, never both, so their key sets never overlap
// within one property. Unlike scalar string values, members are encoded
// byte-for-byte (no i;ascii-casemap fold): message-ids and ids are
// case-sensitive. The blob reference index (refOps) is this same
// pattern specialized to BlobRef properties.

// setMembers returns the index members of a SetIndexed property value:
// the object's keys for a KindObject, the string elements for a
// KindArray. A null or absent value has no members (json.Unmarshal maps
// "null" to a nil map/slice). Non-string array elements are an error, as
// a set index needs string members.
func setMembers(p descriptor.Property, raw json.RawMessage) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	switch p.Kind {
	case descriptor.KindObject:
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		return out, nil
	case descriptor.KindArray:
		var a []string
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, err
		}
		return a, nil
	}
	return nil, fmt.Errorf("objectdb: kind %d is not set-indexable", p.Kind)
}

// memberSet is setMembers as a lookup set for one record version.
func memberSet(p descriptor.Property, obj Object, name string) (map[string]bool, error) {
	if obj == nil {
		return nil, nil
	}
	members, err := setMembers(p, obj[name])
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(members))
	for _, m := range members {
		set[m] = true
	}
	return set, nil
}

// setIndexOps maintains the membership reverse index for SetIndexed
// properties: delete keys for departed members, set keys for arrived
// ones. Runs inside the same commit as the object write, so the index
// can never disagree with the records (like indexOps and refOps).
func setIndexOps(batch *backend.Batch, acct jmap.Id, t *descriptor.Type, id jmap.Id, old, new Object) error {
	for name, p := range t.Properties {
		if !p.SetIndexed {
			continue
		}
		oldM, err := memberSet(p, old, name)
		if err != nil {
			return err
		}
		newM, err := memberSet(p, new, name)
		if err != nil {
			return err
		}
		for m := range oldM {
			if !newM[m] {
				batch.Delete(idxKey(acct, t.Name, name, []byte(m), id))
			}
		}
		for m := range newM {
			if !oldM[m] {
				batch.Set(idxKey(acct, t.Name, name, []byte(m), id), nil)
			}
		}
	}
	return nil
}

// IdsWhereMember returns the ids of records that contain member in the
// given SetIndexed composite property, straight from the membership
// index. The property must be declared SetIndexed. Matching is exact:
// object keys and array elements are compared byte-for-byte, without the
// i;ascii-casemap folding scalar string indexes apply.
func (db *DB) IdsWhereMember(ctx context.Context, acct jmap.Id, typeName, prop, member string) ([]jmap.Id, error) {
	t := db.types[typeName]
	if t == nil {
		return nil, ErrUnknownType
	}
	p, declared := t.Properties[prop]
	if !declared || !p.SetIndexed {
		return nil, fmt.Errorf("objectdb: property %s.%s is not set-indexed", typeName, prop)
	}
	start, end := prefixRange(seg(string(acct)), seg("x"), seg(typeName), seg(prop), []byte(member))
	var ids []jmap.Id
	err := db.be.Scan(ctx, start, end, false, func(k, _ []byte) bool {
		ids = append(ids, idFromObjKey(k))
		return true
	})
	return ids, err
}

// IdsWhereMember is DB.IdsWhereMember as this Update sees it: the
// committed membership index overlaid with the staged creates, updates,
// and destroys, so a cross-record invariant enforced mid-commit (an
// email joining the thread of another email staged earlier in the same
// commit) sees the staged state the committed index cannot show.
func (u *Update) IdsWhereMember(typeName, prop, member string) ([]jmap.Id, error) {
	t := u.db.types[typeName]
	if t == nil {
		return nil, ErrUnknownType
	}
	p, declared := t.Properties[prop]
	if !declared || !p.SetIndexed {
		return nil, fmt.Errorf("objectdb: property %s.%s is not set-indexed", typeName, prop)
	}
	committed, err := u.db.IdsWhereMember(u.ctx, u.acct, typeName, prop, member)
	if err != nil {
		return nil, err
	}
	set := make(map[jmap.Id]bool, len(committed))
	for _, id := range committed {
		set[id] = true
	}
	for _, st := range u.staged {
		if st.typeName != typeName {
			continue
		}
		delete(set, st.id)
		if st.new == nil {
			continue
		}
		members, err := setMembers(p, st.new[prop])
		if err != nil {
			return nil, err
		}
		for _, m := range members {
			if m == member {
				set[st.id] = true
				break
			}
		}
	}
	ids := make([]jmap.Id, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// LowestMemberId returns the smallest id whose record contains member in the
// given SetIndexed property, without materialising every match: the id is the
// last segment of each index key, so the index is ordered by id and the first
// row of the member's range is already the smallest. It reads only that row
// (plus any staged records this commit has not yet written), so a member
// shared by a huge number of records costs one lookup, not a full scan. ok is
// false when no record contains the member. It sees the same committed-plus-
// staged overlay as IdsWhereMember.
func (u *Update) LowestMemberId(typeName, prop, member string) (jmap.Id, bool, error) {
	t := u.db.types[typeName]
	if t == nil {
		return "", false, ErrUnknownType
	}
	p, declared := t.Properties[prop]
	if !declared || !p.SetIndexed {
		return "", false, fmt.Errorf("objectdb: property %s.%s is not set-indexed", typeName, prop)
	}
	// A staged record overrides the committed index for its own id, so note
	// every staged id of this type to skip while scanning committed, and fold
	// the staged members in separately (a staged id that is no longer a member
	// is thereby excluded, one that newly is gets counted).
	staged := make(map[jmap.Id]bool)
	var stagedBest jmap.Id
	stagedFound := false
	for _, st := range u.staged {
		if st.typeName != typeName {
			continue
		}
		staged[st.id] = true
		if st.new == nil {
			continue
		}
		members, err := setMembers(p, st.new[prop])
		if err != nil {
			return "", false, err
		}
		for _, m := range members {
			if m == member {
				if !stagedFound || st.id < stagedBest {
					stagedBest, stagedFound = st.id, true
				}
				break
			}
		}
	}
	var committedBest jmap.Id
	committedFound := false
	start, end := prefixRange(seg(string(u.acct)), seg("x"), seg(typeName), seg(prop), []byte(member))
	err := u.db.be.Scan(u.ctx, start, end, false, func(k, _ []byte) bool {
		id := idFromObjKey(k)
		if staged[id] {
			return true // overridden by its staged version, handled above
		}
		committedBest, committedFound = id, true
		return false // ordered by id: the first non-staged row is the smallest
	})
	if err != nil {
		return "", false, err
	}
	switch {
	case committedFound && stagedFound:
		if committedBest < stagedBest {
			return committedBest, true, nil
		}
		return stagedBest, true, nil
	case committedFound:
		return committedBest, true, nil
	case stagedFound:
		return stagedBest, true, nil
	default:
		return "", false, nil
	}
}
