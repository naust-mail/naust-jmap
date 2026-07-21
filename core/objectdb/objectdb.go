// Package objectdb is the runtime's object database: collections of
// typed records with in-commit index maintenance, a per-account change
// log, and per-type state strings, built once over any backend.Backend.
//
// Consistency contract (the matched pair): every mutation happens under
// the account's lease and commits as ONE atomic batch containing the
// object writes, index updates, the change log entry, the sequence
// bump, and the lease fencing assertion. The change log is part of the
// commit, never a downstream event - a state string can therefore never
// disagree with the data it describes.
//
// Reads take no lease. A multi-object read concurrent with a commit may
// observe a torn view across objects (single Get/Scan calls are atomic;
// groups are not). RFC 8620 section 3.10 already tells clients data may
// change between method calls; a snapshot upgrade via an optional
// backend interface can tighten this later without API change.
package objectdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/tuning"
)

// Object is one record: property name to raw JSON value. The "id"
// property is always present on stored objects.
type Object map[string]json.RawMessage

var (
	// ErrNotFound reports a missing record.
	ErrNotFound = errors.New("objectdb: record not found")
	// ErrUnknownType reports an unregistered type name.
	ErrUnknownType = errors.New("objectdb: unknown type")
	// ErrCannotCalculateChanges maps to the cannotCalculateChanges
	// method error (RFC 8620 section 5.2).
	ErrCannotCalculateChanges = errors.New("objectdb: cannot calculate changes from that state")

	errUnknownKind = errors.New("objectdb: unknown property kind")
)

// DB is the object database over one backend.
type DB struct {
	be       backend.Backend
	leases   lease.Manager
	types    map[string]*descriptor.Type
	notifier notify.Notifier
	// idScheme selects how Create assigns record ids (see tuning.IdScheme).
	idScheme tuning.IdScheme
	// now supplies the wall-clock reading the ULID id scheme stamps into
	// ids. It is a field only so a test can pin it; production uses time.Now.
	now func() time.Time
}

// Option configures a DB at construction.
type Option func(*DB)

// WithIdScheme selects the record-id scheme (default tuning.DefaultIdScheme).
func WithIdScheme(s tuning.IdScheme) Option { return func(db *DB) { db.idScheme = s } }

// WithNow overrides the clock the ULID id scheme reads. It exists for
// deterministic testing; production leaves the default, time.Now.
func WithNow(now func() time.Time) Option { return func(db *DB) { db.now = now } }

// New wraps a backend and lease manager.
func New(be backend.Backend, lm lease.Manager, opts ...Option) *DB {
	db := &DB{
		be:       be,
		leases:   lm,
		types:    make(map[string]*descriptor.Type),
		idScheme: tuning.DefaultIdScheme,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(db)
	}
	return db
}

// RegisterType adds a type descriptor. Registration is not
// concurrency-safe; register everything before serving.
func (db *DB) RegisterType(t *descriptor.Type) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if _, dup := db.types[t.Name]; dup {
		return fmt.Errorf("objectdb: type %s already registered", t.Name)
	}
	db.types[t.Name] = t
	return nil
}

// Type returns a registered descriptor, or nil.
func (db *DB) Type(name string) *descriptor.Type { return db.types[name] }

// TypeNames returns every registered type name, sorted.
func (db *DB) TypeNames() []string {
	names := make([]string, 0, len(db.types))
	for name := range db.types {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SetNotifier attaches a post-commit notifier: after every successful
// commit, the touched types and their new state strings are published
// for the account (the producer side of RFC 8620 section 7 push).
// Delivery is best-effort by the notify contract - the commit is
// already durable, and a lost notification only delays a client's
// resync. Set before serving; not concurrency-safe.
func (db *DB) SetNotifier(n notify.Notifier) { db.notifier = n }

// Notifier returns the attached post-commit notifier, nil when none is
// set. A consumer that must wake on commits (a queue worker's
// cross-process discovery) subscribes to exactly the instance commits
// publish to, so the wiring cannot diverge.
func (db *DB) Notifier() notify.Notifier { return db.notifier }

func unmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

// Get returns one record.
func (db *DB) Get(ctx context.Context, acct jmap.Id, typeName string, id jmap.Id) (Object, error) {
	if db.types[typeName] == nil {
		return nil, ErrUnknownType
	}
	raw, err := db.be.Get(ctx, objKey(acct, typeName, id))
	if errors.Is(err, backend.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var obj Object
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// GetMany returns one record per id, in the same order, nil for an id Get
// would report ErrNotFound for. On a backend implementing
// backend.MultiGetter (Postgres) this is one round trip regardless of
// len(ids) instead of one per id - loadAndMatch (query.go) and the generic
// /get method (standard.go) are the two callers this exists for, both of
// which read a batch of ids and already distinguish found from missing
// themselves. On a backend without MultiGetter it falls back to sequential
// Get calls, so it is always correct, just not always faster.
func (db *DB) GetMany(ctx context.Context, acct jmap.Id, typeName string, ids []jmap.Id) ([]Object, error) {
	if db.types[typeName] == nil {
		return nil, ErrUnknownType
	}
	if len(ids) == 0 {
		return nil, nil
	}
	mg, ok := db.be.(backend.MultiGetter)
	if !ok {
		out := make([]Object, len(ids))
		for i, id := range ids {
			obj, err := db.Get(ctx, acct, typeName, id)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			out[i] = obj
		}
		return out, nil
	}
	out := make([]Object, len(ids))
	for start := 0; start < len(ids); start += tuning.MaxMultiGetBatch {
		end := start + tuning.MaxMultiGetBatch
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		keys := make([][]byte, len(chunk))
		for i, id := range chunk {
			keys[i] = objKey(acct, typeName, id)
		}
		raws, err := mg.MultiGet(ctx, keys)
		if err != nil {
			return nil, err
		}
		for i, raw := range raws {
			if raw == nil {
				continue
			}
			var obj Object
			if err := json.Unmarshal(raw, &obj); err != nil {
				return nil, err
			}
			out[start+i] = obj
		}
	}
	return out, nil
}

// AllIds lists every record id of a type, in id order. If max > 0 and
// more than max exist, it returns max+1 ids so the caller can detect
// the overflow (RFC 8620 section 5.1: /get with ids null is subject to
// maxObjectsInGet).
func (db *DB) AllIds(ctx context.Context, acct jmap.Id, typeName string, max int) ([]jmap.Id, error) {
	if db.types[typeName] == nil {
		return nil, ErrUnknownType
	}
	start, end := prefixRange(seg(string(acct)), seg("o"), seg(typeName))
	var ids []jmap.Id
	err := db.be.Scan(ctx, start, end, false, func(k, _ []byte) bool {
		ids = append(ids, idFromObjKey(k))
		return max <= 0 || len(ids) <= max
	})
	return ids, err
}

// idFromObjKey recovers the trailing id segment of an object key.
func idFromObjKey(k []byte) jmap.Id {
	// The id is the last segment; strip the terminator and unescape.
	// Ids use the section 1.2 alphabet, so no escapes occur in practice.
	end := len(k) - 2 // drop 0x00 0x01 terminator
	start := end
	for start >= 2 && !(k[start-2] == 0x00 && k[start-1] == 0x01) {
		start--
	}
	return jmap.Id(k[start:end])
}

// TypeState returns the current state string for a type in an account
// ("0" for a type never written; RFC 8620 section 5.1 state semantics).
func (db *DB) TypeState(ctx context.Context, acct jmap.Id, typeName string) (string, error) {
	if db.types[typeName] == nil {
		return "", ErrUnknownType
	}
	raw, err := db.be.Get(ctx, typeStateKey(acct, typeName))
	if errors.Is(err, backend.ErrNotFound) {
		return "0", nil
	}
	if err != nil {
		return "", err
	}
	seq, err := backend.DecodeInt64(raw)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(seq, 10), nil
}

// Update runs fn under the account's writer lease and commits every
// staged mutation atomically. It returns the new per-type state strings
// for the types fn touched. fn returning an error commits nothing.
func (db *DB) Update(ctx context.Context, acct jmap.Id, fn func(u *Update) error) (map[string]string, error) {
	l, err := db.leases.Acquire(ctx, acct)
	if err != nil {
		return nil, err
	}
	defer l.Release()
	return db.update(ctx, acct, l, fn)
}

// update is Update's body under an already-held lease, so a caller that has
// other lease-held work to do first (FinalizeBlobUploadThenUpdate) commits in
// the same hold instead of queueing for the account a second time.
func (db *DB) update(ctx context.Context, acct jmap.Id, l lease.Lease, fn func(u *Update) error) (map[string]string, error) {
	// AllocateSequence: read the counter once under the lease; the
	// incremented value persists inside the same batch as the log entry
	// it numbers, so a sequence number exists iff its commit succeeded
	// (monotonic, never reused, survives restart).
	var current int64
	if raw, err := db.be.Get(ctx, seqKey(acct)); err == nil {
		if current, err = backend.DecodeInt64(raw); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, backend.ErrNotFound) {
		return nil, err
	}
	sequence := current + 1

	u := &Update{db: db, ctx: ctx, acct: acct, staged: make(map[string]*stagedRecord), bumped: map[string]struct{}{}, tagOps: map[string]bool{}, sequence: sequence}
	if err := fn(u); err != nil {
		return nil, err
	}
	if len(u.staged) == 0 && len(u.bumped) == 0 && len(u.tagOps) == 0 {
		return map[string]string{}, nil
	}

	batch, touched, err := u.buildBatch(sequence)
	if err != nil {
		return nil, err
	}
	batch.Set(seqKey(acct), backend.EncodeInt64(sequence))
	l.Fence(batch)
	if err := db.be.WriteBatch(ctx, batch); err != nil {
		return nil, err
	}
	states := make(map[string]string, len(touched))
	for _, t := range touched {
		states[t] = strconv.FormatInt(sequence, 10)
	}
	if db.notifier != nil && len(states) > 0 {
		db.notifier.Publish(ctx, acct, jmap.TypeState(states))
	}
	return states, nil
}

// Update stages mutations for one atomic commit. Mutations to several
// types in one Update are the cross-type hook mechanism: a plugin that
// must adjust counters on a second type does it in the same commit.
type Update struct {
	db   *DB
	ctx  context.Context
	acct jmap.Id
	// staged is keyed by type/id; each entry knows the pre-image (for
	// index maintenance) and the final disposition.
	staged map[string]*stagedRecord
	// bumped is the set of types whose state string this commit advances
	// without staging any record - see BumpState.
	bumped map[string]struct{}
	// tagOps stages account-tag writes: tag name to set (true) or clear
	// (false) - see SetAccountTag.
	tagOps map[string]bool
	// sequence is this commit's per-account sequence number, read once at
	// the start of Update. createSeq counts records created in this commit.
	// The Sequence id scheme (tuning.SchemeSequence) derives ids from the
	// pair so they sort by in-account creation order without any extra read.
	sequence  int64
	createSeq int64
}

type stagedRecord struct {
	typeName string
	id       jmap.Id
	old      Object // nil = record did not exist before this Update
	new      Object // nil = destroyed
	created  bool
}

func stagedKey(typeName string, id jmap.Id) string { return typeName + "\x00" + string(id) }

// Context returns the context the Update runs under, for hooks that call
// out to sockets (a policy check inside a set hook) while the lease is
// held.
func (u *Update) Context() context.Context { return u.ctx }

// Account returns the account this Update mutates. Hooks receive the
// Update but not the method arguments, so this is how a hook learns whose
// account it is validating.
func (u *Update) Account() jmap.Id { return u.acct }

// Get reads a record as this Update sees it: staged changes first, then
// committed state. Safe because the lease excludes other writers.
func (u *Update) Get(typeName string, id jmap.Id) (Object, error) {
	if st, ok := u.staged[stagedKey(typeName, id)]; ok {
		if st.new == nil {
			return nil, ErrNotFound
		}
		return st.new, nil
	}
	return u.db.Get(u.ctx, u.acct, typeName, id)
}

// GetMany reads several records as this Update sees it: staged changes
// first, then one batched read (DB.GetMany) for whatever ids are not
// staged - the same read-your-own-writes semantics as Get, without a
// backend round trip per id. Not-found ids (staged as destroyed, or absent
// from the store) are simply missing from the returned map.
func (u *Update) GetMany(typeName string, ids []jmap.Id) (map[jmap.Id]Object, error) {
	out := make(map[jmap.Id]Object, len(ids))
	remaining := make([]jmap.Id, 0, len(ids))
	for _, id := range ids {
		if st, ok := u.staged[stagedKey(typeName, id)]; ok {
			if st.new != nil {
				out[id] = st.new
			}
			continue
		}
		remaining = append(remaining, id)
	}
	if len(remaining) == 0 {
		return out, nil
	}
	objs, err := u.db.GetMany(u.ctx, u.acct, typeName, remaining)
	if err != nil {
		return nil, err
	}
	for i, obj := range objs {
		if obj != nil {
			out[remaining[i]] = obj
		}
	}
	return out, nil
}

// newId assigns the id for one record under the DB's configured scheme
// (tuning.IdScheme). All three schemes emit ids satisfying RFC 8620 section
// 1.2. Sequence draws on this commit's per-account sequence and a per-commit
// index, so ids sort by in-account creation order with no extra read; ULID
// stamps the DB clock; Random derives from nothing.
func (u *Update) newId() jmap.Id {
	switch u.db.idScheme {
	case tuning.SchemeSequence:
		id := jmap.NewSequenceId(u.sequence, u.createSeq)
		u.createSeq++
		return id
	case tuning.SchemeRandom:
		return jmap.NewId()
	default: // tuning.SchemeULID
		return jmap.NewULID(u.db.now())
	}
}

// Create stages a new record and returns its server-assigned id
// (RFC 8620 section 1.2: ids are server-assigned and immutable). obj
// must not contain "id".
func (u *Update) Create(typeName string, obj Object) (jmap.Id, error) {
	t := u.db.types[typeName]
	if t == nil {
		return "", ErrUnknownType
	}
	if _, has := obj["id"]; has {
		return "", fmt.Errorf("objectdb: create must not carry an id")
	}
	if err := checkKinds(t, obj); err != nil {
		return "", err
	}
	id := u.newId()
	stored := make(Object, len(obj)+1)
	for k, v := range obj {
		stored[k] = v
	}
	idJSON, _ := json.Marshal(id)
	stored["id"] = idJSON
	u.staged[stagedKey(typeName, id)] = &stagedRecord{typeName: typeName, id: id, new: stored, created: true}
	return id, nil
}

// Put stages a full replacement of an existing record. obj must carry
// the same id.
func (u *Update) Put(typeName string, id jmap.Id, obj Object) error {
	t := u.db.types[typeName]
	if t == nil {
		return ErrUnknownType
	}
	var gotId jmap.Id
	if raw, has := obj["id"]; !has || unmarshal(raw, &gotId) != nil || gotId != id {
		return fmt.Errorf("objectdb: put object id mismatch")
	}
	if err := checkKinds(t, obj); err != nil {
		return err
	}
	if st, ok := u.staged[stagedKey(typeName, id)]; ok {
		if st.new == nil {
			return ErrNotFound
		}
		st.new = obj
		return nil
	}
	old, err := u.db.Get(u.ctx, u.acct, typeName, id)
	if err != nil {
		return err
	}
	u.staged[stagedKey(typeName, id)] = &stagedRecord{typeName: typeName, id: id, old: old, new: obj}
	return nil
}

// Destroy stages permanent removal (RFC 8620 section 5.3 destroy).
func (u *Update) Destroy(typeName string, id jmap.Id) error {
	if u.db.types[typeName] == nil {
		return ErrUnknownType
	}
	if st, ok := u.staged[stagedKey(typeName, id)]; ok {
		if st.new == nil {
			return ErrNotFound
		}
		st.new = nil
		return nil
	}
	old, err := u.db.Get(u.ctx, u.acct, typeName, id)
	if err != nil {
		return err
	}
	u.staged[stagedKey(typeName, id)] = &stagedRecord{typeName: typeName, id: id, old: old}
	return nil
}

// BumpState advances a type's state string in this commit without staging
// any record. It exists for a push-only type - one that appears in the
// StateChange "changed" map (RFC 8620 section 7.1) but holds no records of
// its own, so nothing else would ever move its state. The bumped type is
// included in the returned states and the post-commit Publish, and its
// persisted state key is set to the commit sequence, but no change-log
// entry is written: a bare bump has no created/updated/destroyed ids, and
// such a type has no /changes method to read them. The type must be
// registered (a method-less descriptor is enough).
func (u *Update) BumpState(typeName string) error {
	if u.db.types[typeName] == nil {
		return ErrUnknownType
	}
	u.bumped[typeName] = struct{}{}
	return nil
}

// SetAccountTag stages this account's membership in the named tag set
// (see TaggedAccounts) into the commit. Setting is idempotent; a tag
// set under the account lease in the same commit as the data it tracks
// can never miss that data. Datatypes use tags as cross-account
// worklists ("accounts with queued work"), kept as supersets: a stale
// member costs its reader one wasted probe, so setting needs no
// verification, while clearing does (see ClearAccountTag).
func (u *Update) SetAccountTag(tag string) error {
	if tag == "" {
		return errors.New("objectdb: empty tag name")
	}
	u.tagOps[tag] = true
	return nil
}

// ClearAccountTag stages this account's removal from the named tag set.
// Clearing is the dangerous direction - a wrongly cleared tag hides an
// account from the tag's readers - so callers must verify the tracked
// condition no longer holds INSIDE this Update (the lease excludes the
// commits that set the tag, closing the check-then-act race). The
// built-in registry tag cannot be cleared.
func (u *Update) ClearAccountTag(tag string) error {
	if tag == "" {
		return errors.New("objectdb: empty tag name")
	}
	if tag == tagExists {
		return errors.New("objectdb: the account registry tag cannot be cleared")
	}
	u.tagOps[tag] = false
	return nil
}

// IdsWhereEqual is DB.IdsWhereEqual as this Update sees it: the
// committed index matches overlaid with the staged creates, updates,
// and destroys. Cross-record invariants a plugin enforces during /set
// (sibling-name uniqueness, single role per account) must see records
// staged earlier in the same commit, which the committed index cannot.
func (u *Update) IdsWhereEqual(typeName, prop string, value json.RawMessage) ([]jmap.Id, error) {
	t := u.db.types[typeName]
	if t == nil {
		return nil, ErrUnknownType
	}
	p, declared := t.Properties[prop]
	if !declared || !p.Indexed {
		return nil, fmt.Errorf("objectdb: property %s.%s is not indexed", typeName, prop)
	}
	want, err := indexValue(p, value)
	if err != nil {
		return nil, err
	}
	committed, err := u.db.IdsWhereEqual(u.ctx, u.acct, typeName, prop, value)
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
		raw, has := st.new[prop]
		if !has {
			continue
		}
		got, err := indexValue(p, raw)
		if err != nil {
			return nil, err
		}
		if string(got) == string(want) {
			set[st.id] = true
		}
	}
	ids := make([]jmap.Id, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// checkKinds validates declared properties and kinds (mechanical part
// of section 5.3 invalidProperties; attribute enforcement lives in the
// runtime's /set).
func checkKinds(t *descriptor.Type, obj Object) error {
	for name, raw := range obj {
		if name == "id" {
			continue
		}
		p, declared := t.Properties[name]
		if !declared {
			return fmt.Errorf("objectdb: unknown property %q on %s", name, t.Name)
		}
		if err := p.CheckValue(raw); err != nil {
			return fmt.Errorf("objectdb: property %q: %w", name, err)
		}
	}
	return nil
}

// logEntry is a change log record: per type, the ids created, updated,
// and destroyed by one commit. The log is the canonical synchronization
// stream that /changes and state strings are derived from.
type logEntry struct {
	Types map[string]*logTypeEntry `json:"types"`
}

type logTypeEntry struct {
	Created   []jmap.Id `json:"created,omitzero"`
	Updated   []jmap.Id `json:"updated,omitzero"`
	Destroyed []jmap.Id `json:"destroyed,omitzero"`
	// UpdatedProps is the union of property names that may have changed
	// across this entry's updates (a mechanical old-vs-new diff, so a
	// rewrite of an identical value may appear). It is what lets
	// Mailbox/changes answer updatedProperties (RFC 8621 section 2.2).
	// nil with a non-empty Updated means the entry predates the field
	// and the changed set is unknown.
	UpdatedProps []string `json:"updatedProps"`
}

func (u *Update) buildBatch(sequence int64) (*backend.Batch, []string, error) {
	batch := &backend.Batch{}
	entry := logEntry{Types: make(map[string]*logTypeEntry)}
	// Idempotently register the account (see Accounts). One tiny Set
	// per commit keeps the registry exact without a read.
	batch.Set(tagKey(tagExists, u.acct), nil)
	// Staged tag ops, in deterministic order.
	tags := make([]string, 0, len(u.tagOps))
	for tag := range u.tagOps {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		if u.tagOps[tag] {
			batch.Set(tagKey(tag, u.acct), nil)
		} else {
			batch.Delete(tagKey(tag, u.acct))
		}
	}

	// Deterministic op order for reproducibility.
	keys := make([]string, 0, len(u.staged))
	for k := range u.staged {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		st := u.staged[k]
		t := u.db.types[st.typeName]
		if st.created && st.new == nil {
			continue // created and destroyed in one Update: no trace (5.2)
		}
		te := entry.Types[st.typeName]
		if te == nil {
			te = &logTypeEntry{}
			entry.Types[st.typeName] = te
		}
		switch {
		case st.new == nil: // destroy
			batch.Delete(objKey(u.acct, st.typeName, st.id))
			if err := indexOps(batch, u.acct, t, st.id, st.old, nil); err != nil {
				return nil, nil, err
			}
			refOps(batch, u.acct, t, st.id, st.old, nil)
			if err := setIndexOps(batch, u.acct, t, st.id, st.old, nil); err != nil {
				return nil, nil, err
			}
			te.Destroyed = append(te.Destroyed, st.id)
		case st.created:
			raw, err := json.Marshal(st.new)
			if err != nil {
				return nil, nil, err
			}
			batch.Set(objKey(u.acct, st.typeName, st.id), raw)
			if err := indexOps(batch, u.acct, t, st.id, nil, st.new); err != nil {
				return nil, nil, err
			}
			refOps(batch, u.acct, t, st.id, nil, st.new)
			if err := setIndexOps(batch, u.acct, t, st.id, nil, st.new); err != nil {
				return nil, nil, err
			}
			te.Created = append(te.Created, st.id)
		default: // update
			raw, err := json.Marshal(st.new)
			if err != nil {
				return nil, nil, err
			}
			batch.Set(objKey(u.acct, st.typeName, st.id), raw)
			if err := indexOps(batch, u.acct, t, st.id, st.old, st.new); err != nil {
				return nil, nil, err
			}
			refOps(batch, u.acct, t, st.id, st.old, st.new)
			if err := setIndexOps(batch, u.acct, t, st.id, st.old, st.new); err != nil {
				return nil, nil, err
			}
			te.Updated = append(te.Updated, st.id)
			if te.UpdatedProps == nil {
				te.UpdatedProps = []string{}
			}
			te.UpdatedProps = mergeProps(te.UpdatedProps, diffProps(st.old, st.new))
		}
	}

	touched := make([]string, 0, len(entry.Types)+len(u.bumped))
	for typeName, te := range entry.Types {
		if len(te.Created)+len(te.Updated)+len(te.Destroyed) == 0 {
			delete(entry.Types, typeName)
			continue
		}
		touched = append(touched, typeName)
		batch.Set(typeStateKey(u.acct, typeName), backend.EncodeInt64(sequence))
	}
	hasRecords := len(entry.Types) > 0

	// Push-only types advanced via BumpState join touched - so they are
	// published and returned - and get their state key set, but contribute
	// no log entry (see BumpState). Skip any already moved by a record.
	for typeName := range u.bumped {
		if _, has := entry.Types[typeName]; has {
			continue
		}
		touched = append(touched, typeName)
		batch.Set(typeStateKey(u.acct, typeName), backend.EncodeInt64(sequence))
	}
	if len(touched) == 0 {
		// Everything cancelled out; commit nothing but the fence is
		// still applied by the caller. Write no log entry.
		return batch, touched, nil
	}
	if hasRecords {
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, nil, err
		}
		batch.Set(logKey(u.acct, sequence), raw)
	}
	return batch, touched, nil
}

// diffProps returns the property names whose raw values differ between
// two versions of a record: a byte-level comparison, so it may report a
// property rewritten with an identical value ("may have changed" is the
// contract UpdatedProps carries).
func diffProps(old, new Object) []string {
	var names []string
	for name, raw := range new {
		if o, has := old[name]; !has || string(o) != string(raw) {
			names = append(names, name)
		}
	}
	for name := range old {
		if _, has := new[name]; !has {
			names = append(names, name)
		}
	}
	return names
}

// mergeProps unions two name lists, sorted, without duplicates.
func mergeProps(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, name := range list {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// indexOps maintains the order-preserving property indexes: delete
// stale keys, set current ones. Runs inside the same commit as the
// object write, so indexes can never disagree with records.
func indexOps(batch *backend.Batch, acct jmap.Id, t *descriptor.Type, id jmap.Id, old, new Object) error {
	for name, p := range t.Properties {
		if !p.Indexed {
			continue
		}
		var oldVal, newVal []byte
		if old != nil {
			if raw, has := old[name]; has {
				v, err := indexValue(p, raw)
				if err != nil {
					return err
				}
				oldVal = v
			}
		}
		if new != nil {
			if raw, has := new[name]; has {
				v, err := indexValue(p, raw)
				if err != nil {
					return err
				}
				newVal = v
			}
		}
		switch {
		case oldVal == nil && newVal == nil:
		case oldVal == nil:
			batch.Set(idxKey(acct, t.Name, name, newVal, id), nil)
		case newVal == nil:
			batch.Delete(idxKey(acct, t.Name, name, oldVal, id))
		case string(oldVal) != string(newVal):
			batch.Delete(idxKey(acct, t.Name, name, oldVal, id))
			batch.Set(idxKey(acct, t.Name, name, newVal, id), nil)
		}
	}
	return nil
}

// IdsWhereEqual returns the ids of records whose indexed property
// equals value, straight from the property index (the /query planner's
// fast path). The property must be declared Indexed. Equality follows
// the index encoding, so string comparison is under i;ascii-casemap
// (RFC 8620 section 5.5).
func (db *DB) IdsWhereEqual(ctx context.Context, acct jmap.Id, typeName, prop string, value json.RawMessage) ([]jmap.Id, error) {
	t := db.types[typeName]
	if t == nil {
		return nil, ErrUnknownType
	}
	p, declared := t.Properties[prop]
	if !declared || !p.Indexed {
		return nil, fmt.Errorf("objectdb: property %s.%s is not indexed", typeName, prop)
	}
	v, err := indexValue(p, value)
	if err != nil {
		return nil, err
	}
	start, end := prefixRange(seg(string(acct)), seg("x"), seg(typeName), seg(prop), v)
	var ids []jmap.Id
	err = db.be.Scan(ctx, start, end, false, func(k, _ []byte) bool {
		ids = append(ids, idFromObjKey(k))
		return true
	})
	return ids, err
}

// IdsWhereAtMost returns, in ascending value order, the ids of records
// whose indexed property is at most max, up to limit (0 means no
// limit). A nil max means no upper bound, so a nil max with limit 1
// yields the record with the smallest indexed value. The property must
// be declared Indexed; ordering and the max comparison follow the index
// encoding (RFC 8620 section 5.5 comparison rules). This is the ordered
// range read backing queue-shaped consumers, where key order on a date
// index is due order; equality lookups use IdsWhereEqual.
func (db *DB) IdsWhereAtMost(ctx context.Context, acct jmap.Id, typeName, prop string, max json.RawMessage, limit int) ([]jmap.Id, error) {
	t := db.types[typeName]
	if t == nil {
		return nil, ErrUnknownType
	}
	p, declared := t.Properties[prop]
	if !declared || !p.Indexed {
		return nil, fmt.Errorf("objectdb: property %s.%s is not indexed", typeName, prop)
	}
	start, end := prefixRange(seg(string(acct)), seg("x"), seg(typeName), seg(prop))
	if max != nil {
		v, err := indexValue(p, max)
		if err != nil {
			return nil, err
		}
		// Everything with value <= max sorts before the successor of
		// max's own subrange, so that successor is the exclusive bound.
		_, end = prefixRange(seg(string(acct)), seg("x"), seg(typeName), seg(prop), v)
	}
	var ids []jmap.Id
	err := db.be.Scan(ctx, start, end, false, func(k, _ []byte) bool {
		ids = append(ids, idFromObjKey(k))
		return limit <= 0 || len(ids) < limit
	})
	return ids, err
}

// Accounts returns, in ascending order, every account that has ever
// committed record data through this store: each commit idempotently
// writes the account into the built-in "exists" tag set, so this is one
// cheap prefix scan - no guessing at the shared keyspace, which also
// holds keys from other components (leases) in their own formats. It
// exists for startup passes such as a queue worker rebuilding its
// per-account view.
func (db *DB) Accounts(ctx context.Context) ([]jmap.Id, error) {
	return db.TaggedAccounts(ctx, tagExists)
}

// TaggedAccounts returns, in ascending order, the members of one
// account-tag set (see Update.SetAccountTag). A tag is a superset by
// contract: readers verify each member against the data the tag tracks
// and treat a miss as staleness, not corruption.
func (db *DB) TaggedAccounts(ctx context.Context, tag string) ([]jmap.Id, error) {
	if tag == "" {
		return nil, errors.New("objectdb: empty tag name")
	}
	start, end := prefixRange(seg("!tag"), seg(tag))
	prefixLen := len(key(seg("!tag"), seg(tag)))
	var accts []jmap.Id
	err := db.be.Scan(ctx, start, end, false, func(k, _ []byte) bool {
		if len(k) > prefixLen+2 {
			// The account id is the last segment; ids use the
			// section 1.2 alphabet, so no escapes occur.
			accts = append(accts, jmap.Id(k[prefixLen:len(k)-2]))
		}
		return true
	})
	return accts, err
}

// SortKey encodes a property value into the order-preserving form the
// indexes use: bytes.Compare on two SortKeys matches the RFC 8620
// section 5.5 comparison rules for the property's kind (booleans
// false<true, numbers numerically, dates chronologically, strings under
// i;ascii-casemap). /query uses it so in-memory filtering and sorting
// agree exactly with index-based evaluation.
func SortKey(p descriptor.Property, raw json.RawMessage) ([]byte, error) {
	return indexValue(p, raw)
}
