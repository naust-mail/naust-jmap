package quotas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// Quota is one quota definition (RFC 9425 section 4.1) as supplied to
// this package's write paths. It deliberately has no used field:
// section 4.1 makes "used" a server-computed value, maintained through
// Service.AddUsed and Service.SetUsed - a definition pull can
// therefore never stomp the counters.
type Quota struct {
	// Id identifies the definition. From a Source it is the source's
	// own stable identifier, any non-empty string, constant across
	// pulls for the same quota. For Service.Upsert it is the record id
	// to update, or empty to create a new record.
	Id string
	// ResourceType is the unit of measure: "count" or "octets"
	// (RFC 9425 section 3.2).
	ResourceType string
	// HardLimit is the limit objects in scope may not be created or
	// updated beyond (section 4.1).
	HardLimit uint64
	// Scope is the entities the quota applies to: "account", "domain",
	// or "global" (section 3.1).
	Scope string
	// Name names the quota, for management and query search (section
	// 4.1).
	Name string
	// Types lists the JMAP type names the quota applies to (section
	// 4.1); clients see the list filtered to the capabilities they
	// requested, and a quota with no type a client recognizes is
	// invisible to that client.
	Types []string
	// WarnLimit and SoftLimit are the optional lower thresholds
	// (section 4.1); nil stores null.
	WarnLimit *uint64
	SoftLimit *uint64
	// Description is the optional human-readable description (section
	// 4.1); nil stores null.
	Description *string
}

// Source supplies quota definitions from the system that owns the
// rules - a billing tier database, a fleet controller - so limits are
// never materialized per user by the embedder. Quotas returns the full
// definition set for one account; ids must be stable across calls.
// Service.Refresh pulls it and mirrors real differences into the
// object database, which holds the protocol's memory (state strings,
// change log, push).
type Source interface {
	Quotas(ctx context.Context, acct jmap.Id) ([]Quota, error)
}

// validate checks the definition's closed value sets. The relative
// ordering of warnLimit/softLimit/hardLimit is a SHOULD (RFC 9425
// section 4.1), so a violation warns and proceeds; values are stored
// verbatim.
func (q *Quota) validate() error {
	switch q.ResourceType {
	case "count", "octets":
	default:
		return fmt.Errorf("definition %q: ResourceType %q is not \"count\" or \"octets\" (RFC 9425 section 3.2)", q.Name, q.ResourceType)
	}
	switch q.Scope {
	case "account", "domain", "global":
	default:
		return fmt.Errorf("definition %q: Scope %q is not \"account\", \"domain\", or \"global\" (RFC 9425 section 3.1)", q.Name, q.Scope)
	}
	if q.Name == "" {
		return errors.New("definition: Name is required (RFC 9425 section 4.1)")
	}
	// The limits are UnsignedInt (section 4.1), so they are bounded by
	// RFC 8620 section 1.3. Checking here means an out-of-range value
	// names the definition that carried it, rather than surfacing later
	// as a store complaint about a bare property name.
	limits := []struct {
		name  string
		value *uint64
	}{
		{"HardLimit", &q.HardLimit}, {"WarnLimit", q.WarnLimit}, {"SoftLimit", q.SoftLimit},
	}
	for _, l := range limits {
		if l.value == nil {
			continue
		}
		if *l.value > uint64(jmap.MaxInt) {
			return fmt.Errorf("definition %q: %s %d exceeds the UnsignedInt maximum %d", q.Name, l.name, *l.value, jmap.MaxInt)
		}
	}
	if q.WarnLimit != nil && (*q.WarnLimit > q.HardLimit || (q.SoftLimit != nil && *q.WarnLimit > *q.SoftLimit)) {
		slog.Warn("naust-jmap: quotas: warnLimit above softLimit or hardLimit", "quota", q.Name)
	}
	if q.SoftLimit != nil && *q.SoftLimit > q.HardLimit {
		slog.Warn("naust-jmap: quotas: softLimit above hardLimit", "quota", q.Name)
	}
	return nil
}

// definitionFields validates the definition and returns its stored
// properties - everything but used (counter-maintained) and sourceId
// (ownership marker, set by the caller that owns it).
func definitionFields(q *Quota) (objectdb.Object, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	null := json.RawMessage("null")
	obj := make(objectdb.Object, 9)
	put := func(name string, v any) {
		raw, _ := json.Marshal(v)
		obj[name] = raw
	}
	put("resourceType", q.ResourceType)
	put("hardLimit", q.HardLimit)
	put("scope", q.Scope)
	put("name", q.Name)
	// types is a set of type names (section 4.1); a repeat carries no
	// meaning and would reach the wire as one, so order is kept and
	// duplicates are dropped.
	types := make([]string, 0, len(q.Types))
	seen := make(map[string]bool, len(q.Types))
	for _, t := range q.Types {
		if seen[t] {
			continue
		}
		seen[t] = true
		types = append(types, t)
	}
	put(typesProperty, types)
	obj["warnLimit"], obj["softLimit"], obj["description"] = null, null, null
	if q.WarnLimit != nil {
		put("warnLimit", *q.WarnLimit)
	}
	if q.SoftLimit != nil {
		put("softLimit", *q.SoftLimit)
	}
	if q.Description != nil {
		put("description", *q.Description)
	}
	return obj, nil
}

// cloneObject is the modify-a-copy step objectdb's write contract
// requires: staged objects must never alias the shared read view.
func cloneObject(o objectdb.Object) objectdb.Object {
	out := make(objectdb.Object, len(o)+1)
	for k, v := range o {
		out[k] = v
	}
	return out
}

// newRecord is a definition ready to create: used starts at zero.
// RFC 9425 section 4.1 makes used a mandatory property, and it is
// server-computed, so a new record carries the counter's origin rather
// than anything the definition supplied.
func newRecord(fields objectdb.Object) objectdb.Object {
	obj := cloneObject(fields)
	obj["used"] = json.RawMessage("0")
	return obj
}

// Refresh pulls the Source's definitions for each account and commits
// exactly the differences against the mirrored records: unchanged
// pulls stage nothing, so states never move and no push fires without
// a real change. Refresh manages only records carrying a source id;
// hand-written records (Service.Upsert) are never touched or
// destroyed. It is the one change signal for Source-backed
// definitions - call it when the source's rules changed, or
// periodically (a maintain.Config.Extra pass).
func (s *Service) Refresh(ctx context.Context, accts ...jmap.Id) error {
	if s.source == nil {
		return errors.New("quotas: Refresh: no Source configured")
	}
	for _, acct := range accts {
		if err := s.refreshAccount(ctx, acct); err != nil {
			return fmt.Errorf("quotas: Refresh: account %s: %w", acct, err)
		}
	}
	return nil
}

func (s *Service) refreshAccount(ctx context.Context, acct jmap.Id) error {
	defs, err := s.source.Quotas(ctx, acct)
	if err != nil {
		return err
	}
	// Validate the whole pull before staging anything: one bad
	// definition aborts the account with no partial commit.
	fields := make([]objectdb.Object, len(defs))
	seen := make(map[string]bool, len(defs))
	for i := range defs {
		if defs[i].Id == "" {
			return fmt.Errorf("definition %q: missing Id", defs[i].Name)
		}
		if seen[defs[i].Id] {
			return fmt.Errorf("Source returned duplicate Id %q", defs[i].Id)
		}
		seen[defs[i].Id] = true
		f, err := definitionFields(&defs[i])
		if err != nil {
			return err
		}
		fields[i] = f
	}
	_, err = s.db.Update(ctx, acct, func(u *objectdb.Update) error {
		ids, err := s.db.AllIds(u.Context(), acct, TypeName, 0)
		if err != nil {
			return err
		}
		objs, err := u.GetMany(TypeName, ids)
		if err != nil {
			return err
		}
		// The mirror: source id -> record, for records this refresh
		// owns (sourceId present).
		bySource := make(map[string]jmap.Id, len(objs))
		for id, obj := range objs {
			if sid, ok := decodeString(obj[sourceIdProperty]); ok && sid != "" {
				bySource[sid] = id
			}
		}
		matched := make(map[string]bool, len(defs))
		for i := range defs {
			sid := defs[i].Id
			matched[sid] = true
			id, exists := bySource[sid]
			if !exists {
				obj := newRecord(fields[i])
				raw, _ := json.Marshal(sid)
				obj[sourceIdProperty] = raw
				if _, err := u.Create(TypeName, obj); err != nil {
					return err
				}
				continue
			}
			cur := objs[id]
			if !definitionChanged(cur, fields[i]) {
				continue
			}
			next := cloneObject(cur)
			for k, v := range fields[i] {
				next[k] = v
			}
			if err := u.Put(TypeName, id, next); err != nil {
				return err
			}
		}
		for sid, id := range bySource {
			if !matched[sid] {
				if err := u.Destroy(TypeName, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return err
}

// definitionChanged reports whether any definition property differs
// from the stored record. Both sides were marshaled by this package,
// so byte comparison is comparison of values.
func definitionChanged(cur, fields objectdb.Object) bool {
	for k, v := range fields {
		if string(cur[k]) != string(v) {
			return true
		}
	}
	return false
}

// Upsert writes one hand-written quota definition: the zero-code
// default for per-account quotas with no external Source. An empty
// q.Id creates a record and returns its id; a non-empty q.Id updates
// that record, preserving its used counter. The used counter starts at
// 0 on create and moves only through AddUsed/SetUsed.
func (s *Service) Upsert(ctx context.Context, acct jmap.Id, q Quota) (jmap.Id, error) {
	fields, err := definitionFields(&q)
	if err != nil {
		return "", fmt.Errorf("quotas: Upsert: %w", err)
	}
	var out jmap.Id
	_, err = s.db.Update(ctx, acct, func(u *objectdb.Update) error {
		if q.Id != "" {
			id := jmap.Id(q.Id)
			cur, err := u.Get(TypeName, id)
			if err != nil {
				return err
			}
			next := cloneObject(cur)
			for k, v := range fields {
				next[k] = v
			}
			out = id
			return u.Put(TypeName, id, next)
		}
		id, err := u.Create(TypeName, newRecord(fields))
		out = id
		return err
	})
	if err != nil {
		return "", fmt.Errorf("quotas: Upsert: %w", err)
	}
	return out, nil
}

// Delete destroys one quota record.
func (s *Service) Delete(ctx context.Context, acct, id jmap.Id) error {
	_, err := s.db.Update(ctx, acct, func(u *objectdb.Update) error {
		return u.Destroy(TypeName, id)
	})
	if err != nil {
		return fmt.Errorf("quotas: Delete: %w", err)
	}
	return nil
}

// AddUsed moves a quota's used counter by delta (positive or
// negative): one small write on the hot path, no Source call, no
// diff. The change log records exactly ["used"], so Quota/changes
// reports updatedProperties accordingly (RFC 9425 section 4.3) and a
// push fires. A delta that would take used below zero clamps to 0
// with a warning: usage stays sane and the accounting bug is loud.
func (s *Service) AddUsed(ctx context.Context, acct, id jmap.Id, delta int64) error {
	_, err := s.db.Update(ctx, acct, func(u *objectdb.Update) error {
		cur, err := u.Get(TypeName, id)
		if err != nil {
			return err
		}
		var used int64
		if raw, has := cur["used"]; has {
			if err := json.Unmarshal(raw, &used); err != nil {
				return fmt.Errorf("stored used is not a number: %w", err)
			}
		}
		next := used + delta
		// Signed overflow would wrap the sum to the opposite sign,
		// where the underflow clamp below would read it as a negative
		// result and silently store zero. An absurd delta must fail
		// loudly instead: the stored counter keeps its old value.
		if (delta > 0 && next < used) || (delta < 0 && next > used) {
			return fmt.Errorf("delta %d overflows the used counter at %d", delta, used)
		}
		if next < 0 {
			// Usage cannot be negative (RFC 9425 section 4.1 types used
			// as UnsignedInt), and the nearest valid value is zero.
			// Reaching here means the caller's accounting disagrees with
			// the stored figure - which a periodic SetUsed reconcile is
			// what actually repairs - so the value is pinned in range
			// and the discrepancy is logged rather than propagated.
			slog.Warn("naust-jmap: quotas: AddUsed below zero; clamped to 0",
				"account", acct, "quota", id, "used", used, "delta", delta)
			next = 0
		}
		if next == used {
			return nil
		}
		obj := cloneObject(cur)
		raw, _ := json.Marshal(next)
		obj["used"] = raw
		return u.Put(TypeName, id, obj)
	})
	if err != nil {
		return fmt.Errorf("quotas: AddUsed: %w", err)
	}
	return nil
}

// SetUsed sets a quota's used counter to an absolute value - the
// reconcile path for embedders that periodically recount usage rather
// than tracking increments.
func (s *Service) SetUsed(ctx context.Context, acct, id jmap.Id, value uint64) error {
	_, err := s.db.Update(ctx, acct, func(u *objectdb.Update) error {
		cur, err := u.Get(TypeName, id)
		if err != nil {
			return err
		}
		obj := cloneObject(cur)
		raw, _ := json.Marshal(value)
		obj["used"] = raw
		return u.Put(TypeName, id, obj)
	})
	if err != nil {
		return fmt.Errorf("quotas: SetUsed: %w", err)
	}
	return nil
}
