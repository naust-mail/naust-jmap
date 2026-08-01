package mail

// Thread (RFC 8621 section 3): a flat, date-ordered list of the Emails
// that belong together. Every Email belongs to exactly one Thread. The
// client-visible Thread is just {id, emailIds}: emailIds is computed
// from the Emails indexed by threadId, and the record's only stored
// content is the internal counter aggregate (internal/emailstore's
// threadstat.go). Assignment - a shared message-id AND an equal base
// subject, Threads never merged - is the mutation engine's job
// (internal/emailstore); this file is the descriptor, registration, and
// /get computed resolver.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/private/rawjson"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
)

// TypeThread is the Thread type name.
const TypeThread = record.TypeThread

// threadType returns the Thread descriptor. emailIds is computed on
// /get from the Email threadId index; the only stored property is the
// internal counter aggregate, which the Internal flag keeps off the
// protocol surface entirely.
func threadType() *descriptor.Type {
	return &descriptor.Type{
		Name:       TypeThread,
		Capability: CapabilityURI,
		Properties: map[string]descriptor.Property{
			emailstore.StatProperty: {Kind: descriptor.KindObject, Internal: true},
		},
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
	return runtime.RegisterStandardTypeExt(p, db, threadType(), core, ext)
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
		s, ok := rawjson.String(stored["id"])
		if !ok {
			return nil, fmt.Errorf("thread record has no valid id")
		}
		tid := jmap.Id(s)
		// The threadId index is declared OrderBy receivedAt, so this
		// scan returns the members already in the section 3 order -
		// receivedAt oldest first, id as the stable tiebreak - with no
		// record loads and nothing to sort.
		ids, err := tc.db.IdsWhereEqual(ctx, acct, TypeEmail, "threadId", record.MustJSON(tid), 0)
		if err != nil {
			return nil, err
		}
		out["emailIds"] = record.MustJSON(ids)
	}
	return out, nil
}
