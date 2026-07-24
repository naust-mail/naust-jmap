package mail

// The trash-role migration (threadmigrate.go): moving the trash role
// changes the section 2.1 unreadThreads rules, the stored counters stay
// as folded under the old rules until corrected, and correction comes
// from either the next change touching a Thread or the migration walk.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
)

// unreadThreadsOf reads one Mailbox's stored unreadThreads counter.
func unreadThreadsOf(t *testing.T, ts *httptest.Server, mb string) int64 {
	t.Helper()
	return int64(readCounters(t, ts, mb)["unreadThreads"].(float64))
}

// setRole patches one Mailbox's role; roleJSON is a JSON value ("null"
// or a quoted role name).
func setRole(t *testing.T, ts *httptest.Server, mb, roleJSON string) {
	t.Helper()
	r := callMail(t, ts, inv("Mailbox/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"role":%s}}}`, testAccount, mb, roleJSON), "0"))
	args := methodArgs(t, r, 0, "Mailbox/set")
	updated, _ := args["updated"].(map[string]any)
	if _, ok := updated[mb]; !ok {
		t.Fatalf("role update rejected: %v", args["notUpdated"])
	}
}

// trashScenario builds the canonical rules-sensitive shape: one Thread
// whose only unread member sits solely in the trash, with a read member
// in the inbox. Under section 2.1 rule 1 that Thread is not unread for
// the inbox; with no trash Mailbox at all, it is.
func trashScenario(t *testing.T) (ts *httptest.Server, db *objectdb.DB, inbox, trash, unreadInTrash, readInInbox string) {
	var store blob.Store
	ts, db, store = emailServer(t)
	inbox = createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	trash = createMailbox(t, ts, `{"name":"Trash","role":"trash"}`)
	headers := func(n int) map[string]string {
		return map[string]string{
			"Message-ID": fmt.Sprintf("<mig%d@example.com>", n),
			"References": "<migroot@example.com>",
		}
	}
	unreadInTrash = putEmailAt(t, db, store, threadMsg("Migration", headers(1)),
		map[string]bool{trash: true}, nil, testReceivedAt)
	readInInbox = putEmailAt(t, db, store, threadMsg("Migration", headers(2)),
		map[string]bool{inbox: true}, map[string]bool{"$seen": true}, testReceivedAt.Add(sequenceOffset(1)))
	if threadOf(t, ts, unreadInTrash) != threadOf(t, ts, readInInbox) {
		t.Fatal("scenario emails must share a thread")
	}
	return ts, db, inbox, trash, unreadInTrash, readInInbox
}

// TestTrashRoleMigrationDrain: after the trash role is removed, the
// stored counters are stale until MigrateThreadCounters drains, and
// correct after; the marker retires with the drain.
func TestTrashRoleMigrationDrain(t *testing.T) {
	ts, db, inbox, trash, _, _ := trashScenario(t)
	ctx := context.Background()

	if got := unreadThreadsOf(t, ts, inbox); got != 0 {
		t.Fatalf("inbox unreadThreads with trash role = %d, want 0 (rule 1)", got)
	}
	setRole(t, ts, trash, "null")
	if got := unreadThreadsOf(t, ts, inbox); got != 0 {
		t.Fatalf("inbox unreadThreads right after the role change = %d; staleness is the designed window", got)
	}

	// Drain with a bound of one correction per call to force the
	// multi-call path.
	calls := 0
	for {
		done, err := MigrateThreadCounters(ctx, db, testAccount, 1)
		if err != nil {
			t.Fatal(err)
		}
		calls++
		if done {
			break
		}
		if calls > 100 {
			t.Fatal("migration never reported done")
		}
	}
	if got := unreadThreadsOf(t, ts, inbox); got != 1 {
		t.Fatalf("inbox unreadThreads after drain = %d, want 1 (no trash: the unread member counts)", got)
	}

	// The marker retired: the next call is the cheap no-pending probe.
	if done, err := MigrateThreadCounters(ctx, db, testAccount, 1); err != nil || !done {
		t.Fatalf("post-drain call = (%v, %v), want immediate done", done, err)
	}
}

// TestTrashRoleMigrationOnTouch: a change touching a stale Thread
// corrects it in the same commit, with no drain involved.
func TestTrashRoleMigrationOnTouch(t *testing.T) {
	ts, db, inbox, trash, _, readInInbox := trashScenario(t)
	ctx := context.Background()

	setRole(t, ts, trash, "null")
	// Touch the Thread through a member-view change that does not itself
	// alter which members are unread: add the read member to the trash
	// Mailbox (now an ordinary folder).
	callMail(t, ts, inv("Email/set", fmt.Sprintf(
		`{"accountId":%q,"update":{%q:{"mailboxIds":{%q:true,%q:true}}}}`,
		testAccount, readInInbox, inbox, trash), "0"))

	if got := unreadThreadsOf(t, ts, inbox); got != 1 {
		t.Fatalf("inbox unreadThreads after touch = %d, want 1: the touch must carry the rules correction", got)
	}

	// The walk finds every Thread already stamped current and retires
	// the marker on its first call.
	if done, err := MigrateThreadCounters(ctx, db, testAccount, 0); err != nil || !done {
		t.Fatalf("walk after touch = (%v, %v), want done in one call", done, err)
	}
	if got := unreadThreadsOf(t, ts, inbox); got != 1 {
		t.Fatalf("inbox unreadThreads after walk = %d, want 1 (walk must not double-correct)", got)
	}
}

// TestTrashRoleToggleCoalesces: role moved away and back before any
// drain - every Thread's stamp already matches the restored rules, so
// the walk corrects nothing and the counters never wobble.
func TestTrashRoleToggleCoalesces(t *testing.T) {
	ts, db, inbox, trash, _, _ := trashScenario(t)
	ctx := context.Background()

	setRole(t, ts, trash, "null")
	setRole(t, ts, trash, `"trash"`)
	if got := unreadThreadsOf(t, ts, inbox); got != 0 {
		t.Fatalf("inbox unreadThreads after toggle = %d, want the original 0", got)
	}
	if done, err := MigrateThreadCounters(ctx, db, testAccount, 0); err != nil || !done {
		t.Fatalf("walk after toggle = (%v, %v), want done in one call", done, err)
	}
	if got := unreadThreadsOf(t, ts, inbox); got != 0 {
		t.Fatalf("inbox unreadThreads after walk = %d, want 0", got)
	}
}

// TestMigrateNoPending: an account that never changed its trash rules
// answers with the cheap probe alone.
func TestMigrateNoPending(t *testing.T) {
	_, db, _, _, _, _ := trashScenario(t)
	if done, err := MigrateThreadCounters(context.Background(), db, testAccount, 0); err != nil || !done {
		t.Fatalf("no-pending call = (%v, %v), want immediate done", done, err)
	}
}
