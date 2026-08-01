package record

import (
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
)

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
	return objectdb.Memo(u, "mail/roleMailboxId/"+role, func() (jmap.Id, error) {
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
