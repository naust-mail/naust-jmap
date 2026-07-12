package mail

// Mailbox (RFC 8621 section 2): the stored descriptor plus the type
// semantics the descriptor cannot express - name/parent/role
// invariants on /set, the FilterCondition language and tree
// arrangements on /query, updatedProperties on /changes, and the
// computed myRights.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// TypeMailbox is the Mailbox type name.
const TypeMailbox = "Mailbox"

// mailboxCounters are the server-set count properties in the canonical
// order used for Mailbox/changes updatedProperties.
var mailboxCounters = [...]string{"totalEmails", "unreadEmails", "totalThreads", "unreadThreads"}

// MailboxType returns the Mailbox descriptor. The count properties are
// server-set and maintained by the Email side in the same commit that
// changes them; until Email lands they stay 0.
func MailboxType() *descriptor.Type {
	null := json.RawMessage("null")
	counter := descriptor.Property{Kind: descriptor.KindUnsignedInt, ServerSet: true, Default: json.RawMessage("0")}
	return &descriptor.Type{
		Name:       TypeMailbox,
		Capability: CapabilityURI,
		Properties: map[string]descriptor.Property{
			"name":          {Kind: descriptor.KindString},
			"parentId":      {Kind: descriptor.KindId, Nullable: true, Indexed: true, Default: null},
			"role":          {Kind: descriptor.KindString, Nullable: true, Indexed: true, Default: null},
			"sortOrder":     {Kind: descriptor.KindUnsignedInt, Default: json.RawMessage("0")},
			"totalEmails":   counter,
			"unreadEmails":  counter,
			"totalThreads":  counter,
			"unreadThreads": counter,
			"isSubscribed":  {Kind: descriptor.KindBool, Default: json.RawMessage("true")},
		},
	}
}

// RegisterMailbox registers the Mailbox type and its RFC 8621 method
// extensions. The embedder must also register CapabilityURI on the
// server: session value an empty object, account value an
// AccountCapability.
func RegisterMailbox(p *runtime.Processor, db *objectdb.DB, core jmap.CoreCapabilities) error {
	ext := &runtime.Extensions{
		DefaultGetProperties: []string{
			"name", "parentId", "role", "sortOrder", "totalEmails",
			"unreadEmails", "totalThreads", "unreadThreads", "myRights",
			"isSubscribed",
		},
		Computed: mailboxComputed{},
		ExtraArgs: map[string]runtime.MethodArgs{
			"set":   {Names: []string{"onDestroyRemoveEmails"}, Check: checkBoolExtras},
			"query": {Names: []string{"sortAsTree", "filterAsTree"}, Check: checkBoolExtras},
		},
		ExtraResponse: &runtime.ResponseExtras{Changes: mailboxChangesExtra},
		Set: &runtime.SetHooks{
			Validate: mailboxValidate,
			Destroy:  mailboxDestroy,
		},
		Query: &runtime.QueryHooks{
			Filter:  mailboxFilter{},
			Arrange: mailboxArrange(db),
		},
	}
	return runtime.RegisterStandardTypeExt(p, db, MailboxType(), core, ext)
}

// isNullRaw reports whether raw is the literal JSON null.
func isNullRaw(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// decodeString decodes a raw JSON string; ok is false for null, a
// missing value, or a non-string. Values are compared decoded, never
// as raw bytes: JSON escaping admits many encodings of one string.
func decodeString(raw json.RawMessage) (string, bool) {
	if raw == nil || isNullRaw(raw) {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// checkBoolExtras requires every supplied extra argument to be a JSON
// boolean. null is guarded explicitly: json.Unmarshal treats it as a
// no-op success for every Go type.
func checkBoolExtras(extra map[string]json.RawMessage) error {
	for name, raw := range extra {
		var b bool
		if isNullRaw(raw) || json.Unmarshal(raw, &b) != nil {
			return fmt.Errorf("%s must be a Boolean", name)
		}
	}
	return nil
}

func invalidProp(name, desc string) *jmap.SetError {
	return &jmap.SetError{Type: jmap.SetErrInvalidProperties, Properties: []string{name}, Description: desc}
}

// myRightsValue is the MailboxRights object: every account here is the
// mailbox owner, so all rights are granted on every mailbox.
var myRightsValue = json.RawMessage(`{"mayReadItems":true,"mayAddItems":true,"mayRemoveItems":true,"maySetSeen":true,"maySetKeywords":true,"mayCreateChild":true,"mayRename":true,"mayDelete":true,"maySubmit":true}`)

type mailboxComputed struct{}

func (mailboxComputed) Accepts(name string) bool { return name == "myRights" }

func (mailboxComputed) Resolve(_ context.Context, _ jmap.Id, _ objectdb.Object, names []string, _ map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, 1)
	for _, name := range names {
		if name == "myRights" {
			out["myRights"] = myRightsValue
		}
	}
	return out, nil
}

// mailboxValidate enforces the section 2 invariants on every create
// and update. The mechanical kind/immutable/server-set checks have
// already passed; new is the record as it would be stored.
func mailboxValidate(u *objectdb.Update, old, new objectdb.Object, extra map[string]json.RawMessage) (*jmap.SetError, error) {
	// name has no default, so a patch to null removes it; it is
	// required for the life of the record.
	name, hasName := decodeString(new["name"])
	if !hasName {
		return invalidProp("name", "name is required"), nil
	}
	if serr := checkMailboxName(name); serr != nil {
		return serr, nil
	}

	// On update the stored record carries its id; on create the record
	// is not yet staged, so there is no self to exclude below.
	var selfId jmap.Id
	if old != nil {
		json.Unmarshal(new["id"], &selfId)
	}

	parentRaw := new["parentId"]
	if parentRaw == nil {
		parentRaw = json.RawMessage("null")
	}
	above, serr, err := mailboxDepthAbove(u, selfId, parentRaw)
	if serr != nil || err != nil {
		return serr, err
	}
	below := 0
	if old != nil {
		// A create cannot have children yet; a reparent moves a whole
		// subtree, so its height counts against the depth limit too.
		below, err = mailboxSubtreeHeight(u, selfId, maxMailboxDepth)
		if err != nil {
			return nil, err
		}
	}
	if above+1+below > maxMailboxDepth {
		return invalidProp("parentId", "mailbox tree would be too deep"), nil
	}

	// Sibling names are unique per parent. IdsWhereEqual sees records
	// staged earlier in this same /set call, so same-call duplicate
	// creates are caught as well.
	siblings, err := u.IdsWhereEqual(TypeMailbox, "parentId", parentRaw)
	if err != nil {
		return nil, err
	}
	for _, sib := range siblings {
		if sib == selfId {
			continue
		}
		obj, err := u.Get(TypeMailbox, sib)
		if err != nil {
			return nil, err
		}
		if sibName, ok := decodeString(obj["name"]); ok && sibName == name {
			return invalidProp("name", "a sibling mailbox already has this name"), nil
		}
	}

	if roleRaw := new["role"]; !isNullRaw(roleRaw) && roleRaw != nil {
		role, _ := decodeString(roleRaw)
		if !mailboxRoles[role] {
			return invalidProp("role", "not a registered IMAP mailbox name attribute"), nil
		}
		others, err := u.IdsWhereEqual(TypeMailbox, "role", roleRaw)
		if err != nil {
			return nil, err
		}
		for _, other := range others {
			if other != selfId {
				return invalidProp("role", "another mailbox already has this role"), nil
			}
		}
	}

	// The descriptor kind admits the full UnsignedInt range; the spec
	// bounds sortOrder to below 2^31.
	var sortOrder int64
	json.Unmarshal(new["sortOrder"], &sortOrder)
	if sortOrder >= 1<<31 {
		return invalidProp("sortOrder", "sortOrder must be below 2^31"), nil
	}
	return nil, nil
}

// checkMailboxName enforces the name shape: a Net-Unicode (RFC 5198)
// string of at least 1 character - NFC-normalized, no control
// characters - within maxSizeMailboxName UTF-8 octets.
func checkMailboxName(name string) *jmap.SetError {
	if name == "" {
		return invalidProp("name", "name must not be empty")
	}
	if len(name) > maxSizeMailboxName {
		return invalidProp("name", "name exceeds maxSizeMailboxName")
	}
	for _, r := range name {
		if r < 0x20 || (r >= 0x7F && r <= 0x9F) {
			return invalidProp("name", "name must not contain control characters")
		}
	}
	if !norm.NFC.IsNormalString(name) {
		return invalidProp("name", "name must be normalization form NFC")
	}
	return nil
}

// mailboxDepthAbove walks up from parentRaw counting ancestors,
// rejecting a missing parent and a walk that reaches selfId (a cycle).
// The step bound terminates a corrupt pre-existing cycle that does not
// pass through self.
func mailboxDepthAbove(u *objectdb.Update, selfId jmap.Id, parentRaw json.RawMessage) (int, *jmap.SetError, error) {
	depth := 0
	cur := parentRaw
	for cur != nil && !isNullRaw(cur) {
		var pid jmap.Id
		json.Unmarshal(cur, &pid)
		if selfId != "" && pid == selfId {
			return 0, invalidProp("parentId", "mailbox cannot be its own ancestor"), nil
		}
		obj, err := u.Get(TypeMailbox, pid)
		if errors.Is(err, objectdb.ErrNotFound) {
			return 0, invalidProp("parentId", "parent mailbox does not exist"), nil
		}
		if err != nil {
			return 0, nil, err
		}
		depth++
		if depth > maxMailboxDepth {
			return 0, invalidProp("parentId", "mailbox tree would be too deep"), nil
		}
		cur = obj["parentId"]
	}
	return depth, nil, nil
}

// mailboxSubtreeHeight returns the number of levels below id (0 for a
// leaf). budget bounds recursion against a corrupt cycle; exhausting
// it returns a height that cannot pass the depth check.
func mailboxSubtreeHeight(u *objectdb.Update, id jmap.Id, budget int) (int, error) {
	if budget <= 0 {
		return maxMailboxDepth, nil
	}
	idRaw, err := json.Marshal(id)
	if err != nil {
		return 0, err
	}
	children, err := u.IdsWhereEqual(TypeMailbox, "parentId", idRaw)
	if err != nil {
		return 0, err
	}
	height := 0
	for _, child := range children {
		h, err := mailboxSubtreeHeight(u, child, budget-1)
		if err != nil {
			return 0, err
		}
		if h+1 > height {
			height = h + 1
		}
	}
	return height, nil
}

// mailboxDestroy enforces the section 2.5 destroy preconditions. The
// email cascade for onDestroyRemoveEmails lands with the Email type;
// the counters cannot be non-zero before it exists.
func mailboxDestroy(u *objectdb.Update, id jmap.Id, extra map[string]json.RawMessage) (*jmap.SetError, error) {
	idRaw, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	children, err := u.IdsWhereEqual(TypeMailbox, "parentId", idRaw)
	if err != nil {
		return nil, err
	}
	if len(children) > 0 {
		return &jmap.SetError{Type: "mailboxHasChild", Description: "mailbox has child mailboxes"}, nil
	}
	obj, err := u.Get(TypeMailbox, id)
	if err != nil {
		return nil, err
	}
	var total int64
	json.Unmarshal(obj["totalEmails"], &total)
	if total > 0 {
		if string(extra["onDestroyRemoveEmails"]) != "true" {
			return &jmap.SetError{Type: "mailboxHasEmail", Description: "mailbox still contains emails"}, nil
		}
		return nil, errors.New("mail: onDestroyRemoveEmails cascade is not implemented yet")
	}
	return nil, nil
}

// mailboxChangesExtra adds updatedProperties (section 2.2): non-null
// only when every change in the window was an update touching nothing
// but the count properties, so a client can refetch just those.
func mailboxChangesExtra(_ context.Context, _ jmap.Id, view *runtime.ChangesView, _ map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{"updatedProperties": json.RawMessage("null")}
	if len(view.Updated) == 0 || len(view.Created) > 0 || len(view.Destroyed) > 0 || view.UpdatedProps == nil {
		return out, nil
	}
	changed := make(map[string]bool, len(view.UpdatedProps))
	for _, p := range view.UpdatedProps {
		changed[p] = true
	}
	props := make([]string, 0, len(mailboxCounters))
	for _, c := range mailboxCounters {
		if changed[c] {
			props = append(props, c)
			delete(changed, c)
		}
	}
	if len(changed) > 0 {
		// A non-count property may have changed; null tells the client
		// to refetch everything.
		return out, nil
	}
	raw, err := json.Marshal(props)
	if err != nil {
		return nil, err
	}
	out["updatedProperties"] = raw
	return out, nil
}

// mailboxFilter is the Mailbox FilterCondition language (section 2.3).
type mailboxFilter struct{}

func (mailboxFilter) ValidateCondition(name string, value json.RawMessage) error {
	switch name {
	case "parentId":
		if isNullRaw(value) {
			return nil
		}
		var id jmap.Id
		if json.Unmarshal(value, &id) != nil || !id.Valid() {
			return fmt.Errorf("parentId must be an Id or null")
		}
	case "name":
		if _, ok := decodeString(value); !ok {
			return fmt.Errorf("name must be a String")
		}
	case "role":
		if isNullRaw(value) {
			return nil
		}
		if _, ok := decodeString(value); !ok {
			return fmt.Errorf("role must be a String or null")
		}
	case "hasAnyRole", "isSubscribed":
		var b bool
		if isNullRaw(value) || json.Unmarshal(value, &b) != nil {
			return fmt.Errorf("%s must be a Boolean", name)
		}
	default:
		return runtime.UnsupportedFilterError{Description: fmt.Sprintf("cannot filter on %q", name)}
	}
	return nil
}

func (mailboxFilter) MatchCondition(obj objectdb.Object, name string, value json.RawMessage) bool {
	switch name {
	case "parentId", "role":
		stored := obj[name]
		if isNullRaw(value) {
			return stored == nil || isNullRaw(stored)
		}
		want, _ := decodeString(value)
		got, ok := decodeString(stored)
		return ok && got == want
	case "name":
		want, _ := decodeString(value)
		got, _ := decodeString(obj["name"])
		return strings.Contains(got, want)
	case "hasAnyRole":
		var want bool
		json.Unmarshal(value, &want)
		role := obj["role"]
		return (role != nil && !isNullRaw(role)) == want
	case "isSubscribed":
		var want, got bool
		json.Unmarshal(value, &want)
		json.Unmarshal(obj["isSubscribed"], &got)
		return got == want
	}
	return false
}

// mailboxArrange implements the sortAsTree and filterAsTree /query
// arguments (section 2.3), running on the full matched set before
// windowing.
func mailboxArrange(db *objectdb.DB) func(context.Context, jmap.Id, []runtime.QueryRecord, func(a, b objectdb.Object) int, map[string]json.RawMessage) ([]jmap.Id, error) {
	return func(ctx context.Context, acct jmap.Id, matched []runtime.QueryRecord, compare func(a, b objectdb.Object) int, extra map[string]json.RawMessage) ([]jmap.Id, error) {
		records := matched
		if string(extra["filterAsTree"]) == "true" {
			records = mailboxFilterAsTree(records)
		}
		if string(extra["sortAsTree"]) == "true" {
			var err error
			records, err = mailboxSortAsTree(ctx, db, acct, records, compare)
			if err != nil {
				return nil, err
			}
		}
		ids := make([]jmap.Id, len(records))
		for i, r := range records {
			ids[i] = r.Id
		}
		return ids, nil
	}
}

// mailboxFilterAsTree keeps a mailbox only if every ancestor also
// matched the filter, preserving the incoming order.
func mailboxFilterAsTree(matched []runtime.QueryRecord) []runtime.QueryRecord {
	byId := make(map[jmap.Id]objectdb.Object, len(matched))
	for _, r := range matched {
		byId[r.Id] = r.Obj
	}
	out := make([]runtime.QueryRecord, 0, len(matched))
	for _, r := range matched {
		obj, keep := r.Obj, true
		for steps := 0; ; steps++ {
			parent := obj["parentId"]
			if parent == nil || isNullRaw(parent) {
				break
			}
			// The step bound terminates on a corrupt parent cycle.
			if steps > len(matched) {
				keep = false
				break
			}
			var pid jmap.Id
			json.Unmarshal(parent, &pid)
			parentObj, ok := byId[pid]
			if !ok {
				keep = false
				break
			}
			obj = parentObj
		}
		if keep {
			out = append(out, r)
		}
	}
	return out
}

// mailboxSortAsTree orders the matched records as a depth-first walk
// of the full mailbox tree with siblings in the standard sort order.
// A matched mailbox with no tree position (a corrupt parent cycle
// makes it unreachable from the roots) comes last, in id order.
func mailboxSortAsTree(ctx context.Context, db *objectdb.DB, acct jmap.Id, matched []runtime.QueryRecord, compare func(a, b objectdb.Object) int) ([]runtime.QueryRecord, error) {
	all, err := db.AllIds(ctx, acct, TypeMailbox, 0)
	if err != nil {
		return nil, err
	}
	type node struct {
		id  jmap.Id
		obj objectdb.Object
	}
	// "" keys the roots (null parentId).
	children := make(map[jmap.Id][]node)
	for _, id := range all {
		obj, err := db.Get(ctx, acct, TypeMailbox, id)
		if err != nil {
			return nil, err
		}
		var parent jmap.Id
		if p := obj["parentId"]; p != nil && !isNullRaw(p) {
			json.Unmarshal(p, &parent)
		}
		children[parent] = append(children[parent], node{id: id, obj: obj})
	}
	for _, siblings := range children {
		sort.SliceStable(siblings, func(i, j int) bool {
			return compare(siblings[i].obj, siblings[j].obj) < 0
		})
	}
	order := make(map[jmap.Id]int, len(all))
	var walk func(parent jmap.Id)
	walk = func(parent jmap.Id) {
		for _, n := range children[parent] {
			if _, seen := order[n.id]; seen {
				continue
			}
			order[n.id] = len(order)
			walk(n.id)
		}
	}
	walk("")
	out := append([]runtime.QueryRecord(nil), matched...)
	sort.SliceStable(out, func(i, j int) bool {
		oi, iok := order[out[i].Id]
		oj, jok := order[out[j].Id]
		if iok != jok {
			return iok
		}
		if !iok {
			return out[i].Id < out[j].Id
		}
		return oi < oj
	})
	return out, nil
}
