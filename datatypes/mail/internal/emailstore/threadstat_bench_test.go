package emailstore_test

// Benchmarks for the thread-counter maintenance path. The stored
// aggregate (threadstat.go) makes one metadata change cost the same
// however many members the thread has; BenchmarkThreadMemberLoad
// measures the primitive the path ran per change before the aggregate
// existed - loading and decoding every member - for comparison.
//
// This is an external test package (emailstore_test, not emailstore):
// building a working fixture needs the root package's RegisterMailbox/
// RegisterThread/RegisterEmail entry points, and an internal test file
// (package emailstore) importing root would be a real import cycle -
// root's production code imports emailstore. Only emailstore's exported
// surface is reachable from here; cloneObject is duplicated inline
// (trivial) and viewOf's decode is reproduced from its two exported
// primitives (MailboxIdsOf, ObjectKeys) rather than exported solely for
// this benchmark's sake.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
	mail "github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
	"github.com/naust-mail/naust-jmap/datatypes/mail/search"
)

const benchAccount = jmap.Id("Abench")

// threadMsg builds a minimal RFC 5322 message with a subject and the
// given extra headers (Message-ID, In-Reply-To, References). Duplicated
// from the root package's thread_test.go (below the shared-helper size
// threshold): the root and engine test suites cannot share one internal
// test file across the package boundary.
func threadMsg(subject string, headers map[string]string) string {
	h := "From: a@example.com\r\nSubject: " + subject + "\r\n"
	for k, v := range headers {
		h += k + ": " + v + "\r\n"
	}
	return h + "\r\nbody\r\n"
}

// benchDB registers the mail types on a bare object database - no HTTP
// server - so a benchmark measures the mutation path alone. It goes
// through the same RegisterMailbox/RegisterThread/RegisterEmail entry
// points production callers use: the root package's per-type descriptor
// accessors are not exported for a benchmark fixture to reach directly.
func benchDB(b *testing.B) (*objectdb.DB, blob.Store, jmap.Id) {
	b.Helper()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(be)
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	if err := mail.RegisterMailbox(p, db, core); err != nil {
		b.Fatal(err)
	}
	if err := mail.RegisterThread(p, db, core); err != nil {
		b.Fatal(err)
	}
	if err := mail.RegisterEmail(p, db, store, core, mail.DefaultAccountCapability(), search.New(store)); err != nil {
		b.Fatal(err)
	}
	var inbox jmap.Id
	if _, err := db.Update(context.Background(), benchAccount, func(u *objectdb.Update) error {
		var err error
		inbox, err = u.Create(record.TypeMailbox, objectdb.Object{
			"name": json.RawMessage(`"Inbox"`),
			"role": json.RawMessage(`"inbox"`),
		})
		return err
	}); err != nil {
		b.Fatal(err)
	}
	return db, store, inbox
}

// benchDeliver runs the shared InsertEmail path for one raw message,
// the way putEmailAt does for tests.
func benchDeliver(b *testing.B, db *objectdb.DB, store blob.Store, inbox jmap.Id, raw string, at time.Time) jmap.Id {
	b.Helper()
	ctx := context.Background()
	bw, err := store.Create(ctx, benchAccount)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.WriteString(bw, raw); err != nil {
		b.Fatal(err)
	}
	blobID, err := db.FinalizeBlobUpload(ctx, benchAccount, bw, "john@example.com", at)
	if err != nil {
		b.Fatal(err)
	}
	c := parse.NewCapture()
	c.Preview = true
	msg, err := parse.ParseMessage(strings.NewReader(raw), c)
	if err != nil {
		b.Fatal(err)
	}
	mb, _ := json.Marshal(map[string]bool{string(inbox): true})
	var id jmap.Id
	if _, err := db.Update(ctx, benchAccount, func(u *objectdb.Update) error {
		var err error
		id, err = emailstore.InsertEmail(u, msg, emailstore.EmailMeta{
			BlobID: blobID, MailboxIds: mb,
			Size: uint64(len(raw)), ReceivedAt: at,
		})
		return err
	}); err != nil {
		b.Fatal(err)
	}
	return id
}

// benchThread builds one thread of n members and returns the first
// member's id and its threadId.
func benchThread(b *testing.B, db *objectdb.DB, store blob.Store, inbox jmap.Id, n int) (jmap.Id, jmap.Id) {
	b.Helper()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var first jmap.Id
	for i := 0; i < n; i++ {
		headers := map[string]string{"Message-ID": fmt.Sprintf("<m%d@example.com>", i)}
		if i > 0 {
			headers["In-Reply-To"] = "<m0@example.com>"
		}
		id := benchDeliver(b, db, store, inbox, threadMsg("Bench", headers), at.Add(time.Duration(i)*time.Minute))
		if i == 0 {
			first = id
		}
	}
	obj, err := db.Get(context.Background(), benchAccount, record.TypeEmail, first)
	if err != nil {
		b.Fatal(err)
	}
	var tid jmap.Id
	if err := json.Unmarshal(obj["threadId"], &tid); err != nil {
		b.Fatal(err)
	}
	return first, tid
}

// benchCloneObject is cloneObject (emailmutate.go), duplicated: an
// external test package cannot reach an unexported helper.
func benchCloneObject(obj objectdb.Object) objectdb.Object {
	next := make(objectdb.Object, len(obj))
	for k, v := range obj {
		next[k] = v
	}
	return next
}

// BenchmarkThreadFlagFlip is one keyword change's full counter work -
// the same sequence the Email/set hook runs: read the record, apply
// AdjustCounters, stage the new record - against growing thread size.
func BenchmarkThreadFlagFlip(b *testing.B) {
	for _, n := range []int{1, 64, 512} {
		b.Run(fmt.Sprintf("members-%d", n), func(b *testing.B) {
			db, store, inbox := benchDB(b)
			first, _ := benchThread(b, db, store, inbox, n)
			ctx := context.Background()
			seen := false
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				seen = !seen
				kw := json.RawMessage(`{}`)
				if seen {
					kw = json.RawMessage(`{"$seen":true}`)
				}
				if _, err := db.Update(ctx, benchAccount, func(u *objectdb.Update) error {
					old, err := u.Get(record.TypeEmail, first)
					if err != nil {
						return err
					}
					next := benchCloneObject(old)
					next["keywords"] = kw
					if err := emailstore.AdjustCounters(u, old, next); err != nil {
						return err
					}
					return u.Put(record.TypeEmail, first, next)
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBulkMarkRead is one commit marking every Email in the Mailbox
// read (then unread, alternating) - the shape a bulk Email/set update
// takes, running the update hook's sequence per object: read the record,
// apply AdjustCounters, stage the new record. Each Email is its own
// thread, so per-object cost is isolated from thread size.
func BenchmarkBulkMarkRead(b *testing.B) {
	for _, n := range []int{16, 128, 512} {
		b.Run(fmt.Sprintf("emails-%d", n), func(b *testing.B) {
			db, store, inbox := benchDB(b)
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			ids := make([]jmap.Id, n)
			for i := range ids {
				headers := map[string]string{"Message-ID": fmt.Sprintf("<bulk%d@example.com>", i)}
				ids[i] = benchDeliver(b, db, store, inbox, threadMsg(fmt.Sprintf("Bulk %d", i), headers), at.Add(time.Duration(i)*time.Second))
			}
			ctx := context.Background()
			seen := false
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				seen = !seen
				kw := json.RawMessage(`{}`)
				if seen {
					kw = json.RawMessage(`{"$seen":true}`)
				}
				if _, err := db.Update(ctx, benchAccount, func(u *objectdb.Update) error {
					for _, id := range ids {
						old, err := u.Get(record.TypeEmail, id)
						if err != nil {
							return err
						}
						next := benchCloneObject(old)
						next["keywords"] = kw
						if err := emailstore.AdjustCounters(u, old, next); err != nil {
							return err
						}
						if err := u.Put(record.TypeEmail, id, next); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkThreadMemberLoad is the per-change primitive the counter
// path ran before the aggregate: resolve the member ids and load and
// decode every member record. One iteration is one read-only pass, so
// the numbers understate the old path (which also computed before and
// after pictures over these members and committed).
func BenchmarkThreadMemberLoad(b *testing.B) {
	for _, n := range []int{1, 64, 512} {
		b.Run(fmt.Sprintf("members-%d", n), func(b *testing.B) {
			db, store, inbox := benchDB(b)
			_, tid := benchThread(b, db, store, inbox, n)
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := db.Update(ctx, benchAccount, func(u *objectdb.Update) error {
					ids, err := u.IdsWhereEqual(record.TypeEmail, "threadId", record.MustJSON(tid))
					if err != nil {
						return err
					}
					members, err := u.GetMany(record.TypeEmail, ids)
					if err != nil {
						return err
					}
					for _, m := range members {
						// viewOf's decode, reproduced from its exported
						// primitives (mailboxIds and unread membership).
						_ = emailstore.MailboxIdsOf(m)
						kw := emailstore.ObjectKeys(m["keywords"])
						_ = !kw["$seen"] && !kw["$draft"]
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
