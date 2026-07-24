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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/internal/jsonscan"
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
	// verifyPreImages enables the commit-time shared-read integrity check
	// (see WithVerifyPreImages).
	verifyPreImages bool
	// propNames interns declared property names for the record decoder:
	// every name registered by any descriptor (plus the implicit "id")
	// maps to itself, so decoding a stored record reuses one shared
	// string per known name instead of allocating a copy per record
	// (see decodeStored). Fixed at registration time, never grown by
	// decoded input.
	propNames map[string]string
}

// Option configures a DB at construction.
type Option func(*DB)

// WithIdScheme selects the record-id scheme (default tuning.DefaultIdScheme).
func WithIdScheme(s tuning.IdScheme) Option { return func(db *DB) { db.idScheme = s } }

// WithNow overrides the clock the ULID id scheme reads. It exists for
// deterministic testing; production leaves the default, time.Now.
func WithNow(now func() time.Time) Option { return func(db *DB) { db.now = now } }

// WithVerifyPreImages is an assertion mode for test builds. Objects
// returned by Update.Get and Update.GetMany are shared views that callers
// must clone before modifying; a caller that writes to one in place
// corrupts the pre-image the commit diffs indexes and the change log
// against. With this option set, every commit re-decodes the stored bytes
// behind each committed read and fails with a named error if the object
// no longer matches - catching the violating hook at the exact commit
// instead of surfacing later as silent index corruption. The check costs
// one extra decode per read record per commit, so production
// configurations leave it off; test helpers should turn it on.
func WithVerifyPreImages() Option { return func(db *DB) { db.verifyPreImages = true } }

// New wraps a backend and lease manager.
func New(be backend.Backend, lm lease.Manager, opts ...Option) *DB {
	db := &DB{
		be:        be,
		leases:    lm,
		types:     make(map[string]*descriptor.Type),
		idScheme:  tuning.DefaultIdScheme,
		now:       time.Now,
		propNames: map[string]string{"id": "id"},
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
	for name := range t.Properties {
		db.propNames[name] = name
	}
	return nil
}

// Type returns a registered descriptor, or nil.
func (db *DB) Type(name string) *descriptor.Type { return db.types[name] }

// TypeNames returns every registered protocol-visible type name,
// sorted. Internal types (descriptor.Type.Internal) are omitted: the
// callers are protocol surfaces (push subscription filters), and an
// internal type is not addressable there.
func (db *DB) TypeNames() []string {
	names := make([]string, 0, len(db.types))
	for name, t := range db.types {
		if t.Internal {
			continue
		}
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

// Get returns one record.
func (db *DB) Get(ctx context.Context, acct jmap.Id, typeName string, id jmap.Id) (Object, error) {
	obj, _, err := db.getRaw(ctx, acct, typeName, id)
	return obj, err
}

// getRaw is Get keeping the stored bytes alongside the decoded object.
// The decoded object's values sub-slice raw, so returning it costs
// nothing; Update retains it per read so the pre-image check
// (WithVerifyPreImages) has pristine bytes to re-decode.
func (db *DB) getRaw(ctx context.Context, acct jmap.Id, typeName string, id jmap.Id) (Object, []byte, error) {
	if db.types[typeName] == nil {
		return nil, nil, ErrUnknownType
	}
	raw, err := db.be.Get(ctx, objKey(acct, typeName, id))
	if errors.Is(err, backend.ErrNotFound) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	obj, err := db.decodeStored(raw)
	if err != nil {
		return nil, nil, err
	}
	return obj, raw, nil
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
	out, _, err := db.getManyRaw(ctx, acct, typeName, ids)
	return out, err
}

// getManyRaw is GetMany keeping each record's stored bytes alongside the
// decoded object, for the same reason as getRaw. raws[i] is nil exactly
// when out[i] is.
func (db *DB) getManyRaw(ctx context.Context, acct jmap.Id, typeName string, ids []jmap.Id) ([]Object, [][]byte, error) {
	if db.types[typeName] == nil {
		return nil, nil, ErrUnknownType
	}
	if len(ids) == 0 {
		return nil, nil, nil
	}
	out := make([]Object, len(ids))
	raws := make([][]byte, len(ids))
	mg, ok := db.be.(backend.MultiGetter)
	if !ok {
		for i, id := range ids {
			obj, raw, err := db.getRaw(ctx, acct, typeName, id)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			out[i], raws[i] = obj, raw
		}
		return out, raws, nil
	}
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
		vals, err := mg.MultiGet(ctx, keys)
		if err != nil {
			return nil, nil, err
		}
		for i, raw := range vals {
			if raw == nil {
				continue
			}
			obj, err := db.decodeStored(raw)
			if err != nil {
				return nil, nil, err
			}
			out[start+i], raws[start+i] = obj, raw
		}
	}
	return out, raws, nil
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

	u := &Update{db: db, ctx: ctx, acct: acct, staged: make(map[recKey]*stagedRecord), bumped: map[string]struct{}{}, tagOps: map[string]bool{}, sequence: sequence}
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
	if db.notifier != nil {
		// Internal types stay off the push surface. Section 7.1 defines a
		// TypeState value as "the state property that would currently be
		// returned by a call to Foo/get" - an internal type has no
		// Foo/get, so its name cannot appear there by definition.
		pub := make(map[string]string, len(states))
		for name, s := range states {
			if t := db.types[name]; t != nil && t.Internal {
				continue
			}
			pub[name] = s
		}
		if len(pub) > 0 {
			db.notifier.Publish(ctx, acct, jmap.TypeState(pub))
		}
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
	staged map[recKey]*stagedRecord
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
	// memo is the commit-scoped cache behind Memo, allocated on first use.
	memo map[string]any
	// read caches committed records fetched through Get/GetMany, keyed
	// like staged, allocated on first use. A later Put, PutInternal, or
	// Destroy of a cached record reuses the read as its pre-image instead
	// of fetching and decoding again; coherent because the lease excludes
	// other writers and staged records are always consulted first. Each
	// entry retains the stored bytes so WithVerifyPreImages can re-decode
	// them at commit and detect a caller mutating the shared object.
	read map[recKey]readRecord
}

// readRecord is one cached committed read: the decoded object and the
// stored bytes it was decoded from (which its values sub-slice).
type readRecord struct {
	obj Object
	raw []byte
}

type stagedRecord struct {
	typeName string
	id       jmap.Id
	old      Object // nil = record did not exist before this Update
	new      Object // nil = destroyed
	created  bool
	// quiet marks a record whose every staged write was PutInternal:
	// bookkeeping the commit persists and indexes but does not report as
	// a client-visible update. Any ordinary write to the record clears
	// it - loud wins.
	quiet bool
}

// recKey identifies one record in the Update's staged and read maps. A
// comparable struct: building one for a lookup allocates nothing, where
// a concatenated string key would allocate on every Get and Put.
type recKey struct {
	typeName string
	id       jmap.Id
}

func stagedKey(typeName string, id jmap.Id) recKey { return recKey{typeName, id} }

// Context returns the context the Update runs under, for hooks that call
// out to sockets (a policy check inside a set hook) while the lease is
// held.
func (u *Update) Context() context.Context { return u.ctx }

// Account returns the account this Update mutates. Hooks receive the
// Update but not the method arguments, so this is how a hook learns whose
// account it is validating.
func (u *Update) Account() jmap.Id { return u.acct }

// Get reads a record as this Update sees it: staged changes first, then
// committed state (cached for the rest of the commit, so a later write to
// the record reuses this read as its pre-image instead of fetching
// again). Safe because the lease excludes other writers.
//
// The returned object is a shared view, not a private copy - for a staged
// record it IS the staged object, for a committed one it is the cached
// pre-image the commit will diff indexes and the change log against.
// Callers that modify it must clone it first; WithVerifyPreImages makes a
// violation fail the commit with a named error.
func (u *Update) Get(typeName string, id jmap.Id) (Object, error) {
	if st, ok := u.staged[stagedKey(typeName, id)]; ok {
		if st.new == nil {
			return nil, ErrNotFound
		}
		return st.new, nil
	}
	return u.getCommitted(typeName, id)
}

// getCommitted reads a record's committed state through the Update's read
// cache: one backend fetch and one decode per record per commit, however
// many times Get or a write path needs the pre-image afterwards. Absence
// is not cached; only the write paths re-read missing records, and only
// to fail.
func (u *Update) getCommitted(typeName string, id jmap.Id) (Object, error) {
	key := stagedKey(typeName, id)
	if r, ok := u.read[key]; ok {
		return r.obj, nil
	}
	obj, raw, err := u.db.getRaw(u.ctx, u.acct, typeName, id)
	if err != nil {
		return nil, err
	}
	if u.read == nil {
		u.read = make(map[recKey]readRecord)
	}
	u.read[key] = readRecord{obj: obj, raw: raw}
	return obj, nil
}

// GetMany reads several records as this Update sees it: staged changes
// first, then the read cache, then one batched read (DB.GetMany) for
// whatever is left - the same read-your-own-writes semantics as Get,
// without a backend round trip per id. Not-found ids (staged as
// destroyed, or absent from the store) are simply missing from the
// returned map. The returned objects are shared views under the same
// clone-before-modify contract as Get.
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
		if r, ok := u.read[stagedKey(typeName, id)]; ok {
			out[id] = r.obj
			continue
		}
		remaining = append(remaining, id)
	}
	if len(remaining) == 0 {
		return out, nil
	}
	objs, raws, err := u.db.getManyRaw(u.ctx, u.acct, typeName, remaining)
	if err != nil {
		return nil, err
	}
	for i, obj := range objs {
		if obj == nil {
			continue
		}
		out[remaining[i]] = obj
		if u.read == nil {
			u.read = make(map[recKey]readRecord)
		}
		u.read[stagedKey(typeName, remaining[i])] = readRecord{obj: obj, raw: raws[i]}
	}
	return out, nil
}

// Memo returns the value for key, computing it with compute at most once
// per Update. It is a commit-scoped cache for an account-wide fact that a
// per-object hook would otherwise re-derive for every object in a bulk
// set (an index lookup answering "which Mailbox holds role X" is the same
// for all of them). The cache never invalidates: the first value is held
// for the rest of the commit, even across the caller's own staged writes,
// so only facts whose in-commit staleness the caller can tolerate belong
// here. Errors are not cached; a failed compute runs again on the next
// call. Keys share one namespace across all callers - prefix with the
// owning package - and a key must always be used with one value type.
func Memo[T any](u *Update, key string, compute func() (T, error)) (T, error) {
	if v, ok := u.memo[key]; ok {
		return v.(T), nil
	}
	v, err := compute()
	if err != nil {
		return v, err
	}
	if u.memo == nil {
		u.memo = make(map[string]any)
	}
	u.memo[key] = v
	return v, nil
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
	if err := checkKinds(t, obj, nil); err != nil {
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
	if s, ok := jsonscan.String(obj["id"]); !ok || jmap.Id(s) != id {
		return fmt.Errorf("objectdb: put object id mismatch")
	}
	if st, ok := u.staged[stagedKey(typeName, id)]; ok {
		if st.new == nil {
			return ErrNotFound
		}
		if err := checkKinds(t, obj, st.new); err != nil {
			return err
		}
		st.new = obj
		st.quiet = false // an ordinary write makes the record loud
		return nil
	}
	old, err := u.getCommitted(typeName, id)
	if err != nil {
		return err
	}
	if err := checkKinds(t, obj, old); err != nil {
		return err
	}
	u.staged[stagedKey(typeName, id)] = &stagedRecord{typeName: typeName, id: id, old: old, new: obj}
	return nil
}

// PutInternal stages a bookkeeping write: a full replacement of an
// existing record that may only change Internal properties (descriptor
// semantics: not part of the type's public schema). The record and its
// indexes are written like any Put, but the record is not reported as
// updated - no change-log entry, no state move, no push - because to a
// client the Foo data is unchanged, and section 5.1 wants the state
// string stable when the data is unchanged. A change to any non-Internal
// property is rejected here, at the call; and if the same commit also
// writes the record with an ordinary Put (including the identity Put
// that announces a computed-property change), the record is reported
// normally - loud wins.
func (u *Update) PutInternal(typeName string, id jmap.Id, obj Object) error {
	t := u.db.types[typeName]
	if t == nil {
		return ErrUnknownType
	}
	if s, ok := jsonscan.String(obj["id"]); !ok || jmap.Id(s) != id {
		return fmt.Errorf("objectdb: put object id mismatch")
	}
	prior, err := u.Get(typeName, id)
	if err != nil {
		return err
	}
	if err := checkKinds(t, obj, prior); err != nil {
		return err
	}
	for _, name := range diffProps(prior, obj) {
		if p, ok := t.Properties[name]; !ok || !p.Internal {
			return fmt.Errorf("objectdb: PutInternal(%s/%s) changes non-Internal property %q", typeName, id, name)
		}
	}
	if st, ok := u.staged[stagedKey(typeName, id)]; ok {
		st.new = obj
		return nil // st.quiet keeps its value: loud stays loud
	}
	u.staged[stagedKey(typeName, id)] = &stagedRecord{typeName: typeName, id: id, old: prior, new: obj, quiet: true}
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
	old, err := u.getCommitted(typeName, id)
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
// Unlike the DB read, ids always come back in plain id order (staged
// records have no index keys yet to merge in OrderBy position); every
// consumer is a membership check, so no order is promised here.
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
// runtime's /set). prior, when non-nil, is a record whose values have
// already passed this check - the staged or committed pre-image of a
// replacement - and a property whose raw bytes equal prior's is not
// re-validated: a full-record Put that changes one property pays
// validation for one property, not the whole record. Every property
// still passes the declared-name check, so an undeclared name is
// rejected regardless of prior.
func checkKinds(t *descriptor.Type, obj, prior Object) error {
	for name, raw := range obj {
		if name == "id" {
			continue
		}
		p, declared := t.Properties[name]
		if !declared {
			return fmt.Errorf("objectdb: unknown property %q on %s", name, t.Name)
		}
		if prior != nil && bytes.Equal(raw, prior[name]) {
			continue
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
	// At is the commit's wall clock in Unix milliseconds, read only by
	// TrimChanges to age entries out. Zero means unknown (an entry written
	// before the field existed), which TrimChanges treats as not yet
	// expired so an existing log is never dropped by a clock it lacks.
	At int64 `json:"at,omitzero"`
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

// verifyReads is the WithVerifyPreImages check: every cached committed
// read is re-decoded from its stored bytes and compared to the object
// the cache handed out. A mismatch means a caller wrote to a shared Get
// result instead of cloning it - the pre-image the commit is about to
// diff against is corrupt, so the commit fails naming the record. The
// stored bytes are pristine by the backend.Get ownership contract (the
// buffer belongs to this reader and objectdb never writes to it).
func (u *Update) verifyReads() error {
	for key, r := range u.read {
		pristine, err := u.db.decodeStored(r.raw)
		if err != nil {
			return fmt.Errorf("objectdb: verify %s/%s: %w", key.typeName, key.id, err)
		}
		mutated := len(pristine) != len(r.obj)
		if !mutated {
			for name, val := range pristine {
				if got, ok := r.obj[name]; !ok || !bytes.Equal(got, val) {
					mutated = true
					break
				}
			}
		}
		if mutated {
			return fmt.Errorf("objectdb: object %s/%s was modified after Get returned it: Get results are shared views, clone before mutating", key.typeName, key.id)
		}
	}
	return nil
}

func (u *Update) buildBatch(sequence int64) (*backend.Batch, []string, error) {
	if u.db.verifyPreImages {
		if err := u.verifyReads(); err != nil {
			return nil, nil, err
		}
	}
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
	keys := make([]recKey, 0, len(u.staged))
	for k := range u.staged {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].typeName != keys[j].typeName {
			return keys[i].typeName < keys[j].typeName
		}
		return keys[i].id < keys[j].id
	})

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
			raw, err := encodeObject(st.new)
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
			raw, err := encodeObject(st.new)
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
			if st.quiet {
				// Every write to this record was PutInternal: persisted
				// and indexed above, but not a client-visible update.
				continue
			}
			te.Updated = append(te.Updated, st.id)
			if te.UpdatedProps == nil {
				te.UpdatedProps = []string{}
			}
			// Internal property names stay out of the log: they are not
			// part of the type's public schema, and the log's property
			// list exists only to answer /changes-style questions about
			// visible data.
			props := diffProps(st.old, st.new)
			visible := props[:0]
			for _, name := range props {
				if p, ok := t.Properties[name]; ok && p.Internal {
					continue
				}
				visible = append(visible, name)
			}
			te.UpdatedProps = mergeProps(te.UpdatedProps, visible)
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
		entry.At = u.db.now().UnixMilli()
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
			ord, err := orderSegs(t, p, new)
			if err != nil {
				return err
			}
			batch.Set(idxKey(acct, t.Name, name, newVal, ord, id), nil)
		case newVal == nil:
			ord, err := orderSegs(t, p, old)
			if err != nil {
				return err
			}
			batch.Delete(idxKey(acct, t.Name, name, oldVal, ord, id))
		case string(oldVal) != string(newVal):
			// One orderSegs serves both keys: OrderBy properties are
			// Immutable (descriptor.Validate), so old and new agree on
			// them - only the indexed value moved.
			ord, err := orderSegs(t, p, new)
			if err != nil {
				return err
			}
			batch.Delete(idxKey(acct, t.Name, name, oldVal, ord, id))
			batch.Set(idxKey(acct, t.Name, name, newVal, ord, id), nil)
		}
	}
	return nil
}

// orderSegs encodes a record's values for the OrderBy siblings of an
// indexed property, in declaration order, for the key segments between
// the index value and the id. An absent value encodes as an empty
// segment (sorts first, and is recomputable from the record alone -
// missing data never fails a write); a present value that does not
// match its property's kind fails the commit, as it would on the
// property's own index.
func orderSegs(t *descriptor.Type, p descriptor.Property, obj Object) ([][]byte, error) {
	if len(p.OrderBy) == 0 {
		return nil, nil
	}
	out := make([][]byte, len(p.OrderBy))
	for i, ob := range p.OrderBy {
		raw, has := obj[ob]
		if !has {
			out[i] = []byte{}
			continue
		}
		v, err := indexValue(t.Properties[ob], raw)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// IdsWhereEqual returns the ids of records whose indexed property
// equals value, straight from the property index (the /query planner's
// fast path). The property must be declared Indexed. Equality follows
// the index encoding, so string comparison is under i;ascii-casemap
// (RFC 8620 section 5.5). Ids come back in index-key order: for a
// property with OrderBy this is its declared ordering then id (the
// stored order IS the answer - Thread's emailIds reads it directly);
// otherwise it is plain id order.
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
