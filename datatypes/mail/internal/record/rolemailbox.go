package record

import (
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

// roleMemoKeyPrefix namespaces RoleMailboxId's Memo keys within
// objectdb's shared per-commit cache.
const roleMemoKeyPrefix = "mail/roleMailboxId/"

// roleMemoKeys precomputes the Memo key for the IANA "IMAP Mailbox Name
// Attributes" roles (RFC 8621 section 2) - a fixed vocabulary, so this
// never grows at runtime and is safe for concurrent reads with no
// synchronization. AdjustCounters resolves the trash role on every
// counter-affecting change; without this, a bulk set rebuilds the same
// key string once per Email. A role outside this set (custom or
// malformed) still works, just via the concatenation below - this is a
// fast path, not a validator.
var roleMemoKeys = map[string]string{
	"all":        roleMemoKeyPrefix + "all",
	"archive":    roleMemoKeyPrefix + "archive",
	"drafts":     roleMemoKeyPrefix + "drafts",
	"flagged":    roleMemoKeyPrefix + "flagged",
	"important":  roleMemoKeyPrefix + "important",
	"inbox":      roleMemoKeyPrefix + "inbox",
	"junk":       roleMemoKeyPrefix + "junk",
	"sent":       roleMemoKeyPrefix + "sent",
	"subscribed": roleMemoKeyPrefix + "subscribed",
	"trash":      roleMemoKeyPrefix + "trash",
}

// RoleMailboxId is the account's Mailbox with the given role, or "" (a
// role is unique per account, RFC 8621 section 2). The lookup is memoized
// per commit: every counter-affecting change resolves it, so a bulk set
// would otherwise repeat the identical index query per Email. Holding one
// value for the whole commit is safe even if the same commit moves the
// role: whatever value a change counts under is stamped into the Thread's
// aggregate, and the role change raises the migration marker, so the stale
// contribution is corrected the same way as any other cross-commit role
// move.
func RoleMailboxId(u *objectdb.Update, role string) (jmap.Id, error) {
	key, ok := roleMemoKeys[role]
	if !ok {
		key = roleMemoKeyPrefix + role
	}
	return objectdb.Memo(u, key, func() (jmap.Id, error) {
		ids, err := u.IdsWhereEqual(TypeMailbox, "role", MustJSON(role))
		if err != nil {
			return "", err
		}
		if len(ids) == 0 {
			return "", nil
		}
		return ids[0], nil
	})
}
