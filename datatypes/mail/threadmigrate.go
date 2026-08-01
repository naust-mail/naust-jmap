package mail

// Correcting the stored Mailbox counters when the trash role moves (RFC
// 8621 section 2.1.1). The migration write-side machinery - the
// CounterRules marker, NoteTrashRulesChange, and the bounded walk - is
// the mutation engine's job (internal/emailstore/threadmigrate.go); this
// file is the marker's descriptor/registration (CapabilityURI is
// root-owned) and the public MigrateThreadCounters entry point, which
// keeps its exact signature while wrapping the engine function.

import (
	"context"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
)

func counterRulesType() *descriptor.Type {
	return &descriptor.Type{
		Name:       emailstore.TypeCounterRules,
		Capability: CapabilityURI,
		Internal:   true,
		Properties: map[string]descriptor.Property{
			"k":   {Kind: descriptor.KindString, Indexed: true, Internal: true},
			"gen": {Kind: descriptor.KindInt, Internal: true},
		},
	}
}

// MigrateThreadCounters corrects, for one account, the stored Mailbox
// counters of Threads still counted under outdated trash rules. It does
// nothing (one index lookup) when no rules change is pending. max
// bounds how many Threads one call corrects (<= 0 means no bound); done
// reports that the account's counters are fully current, so a caller
// drains with `for !done`.
func MigrateThreadCounters(ctx context.Context, db *objectdb.DB, acct jmap.Id, max int) (done bool, err error) {
	return emailstore.MigrateThreadCounters(ctx, db, acct, max)
}
