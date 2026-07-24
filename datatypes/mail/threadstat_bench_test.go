package mail

// Benchmarks for the thread-counter maintenance path. The stored
// aggregate (threadstat.go) makes one metadata change cost the same
// however many members the thread has; BenchmarkThreadMemberLoad
// measures the primitive the path ran per change before the aggregate
// existed - loading and decoding every member - for comparison.

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
)

const benchAccount = jmap.Id("Abench")

// benchDB registers the mail types on a bare object database - no HTTP
// server, no runtime - so a benchmark measures the mutation path alone.
func benchDB(b *testing.B) (*objectdb.DB, blob.Store, jmap.Id) {
	b.Helper()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	if err := db.RegisterType(MailboxType()); err != nil {
		b.Fatal(err)
	}
	if err := db.RegisterType(ThreadType()); err != nil {
		b.Fatal(err)
	}
	if err := db.RegisterType(EmailType()); err != nil {
		b.Fatal(err)
	}
	if err := db.RegisterType(EmailDeliveryType()); err != nil {
		b.Fatal(err)
	}
	var inbox jmap.Id
	if _, err := db.Update(context.Background(), benchAccount, func(u *objectdb.Update) error {
		var err error
		inbox, err = u.Create(TypeMailbox, objectdb.Object{
			"name": json.RawMessage(`"Inbox"`),
			"role": json.RawMessage(`"inbox"`),
		})
		return err
	}); err != nil {
		b.Fatal(err)
	}
	return db, kvstore.New(be), inbox
}

// benchDeliver runs the shared insertEmail path for one raw message,
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
	c := newCapture()
	c.preview = true
	msg, err := parseMessage(strings.NewReader(raw), c)
	if err != nil {
		b.Fatal(err)
	}
	mb, _ := json.Marshal(map[string]bool{string(inbox): true})
	var id jmap.Id
	if _, err := db.Update(ctx, benchAccount, func(u *objectdb.Update) error {
		var err error
		id, err = insertEmail(u, msg, emailMeta{
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
	obj, err := db.Get(context.Background(), benchAccount, TypeEmail, first)
	if err != nil {
		b.Fatal(err)
	}
	var tid jmap.Id
	if err := json.Unmarshal(obj["threadId"], &tid); err != nil {
		b.Fatal(err)
	}
	return first, tid
}

// BenchmarkThreadFlagFlip is one keyword change's full counter work -
// the same sequence the Email/set hook runs: read the record, apply
// adjustCounters, stage the new record - against growing thread size.
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
					old, err := u.Get(TypeEmail, first)
					if err != nil {
						return err
					}
					next := cloneObject(old)
					next["keywords"] = kw
					if err := adjustCounters(u, old, next); err != nil {
						return err
					}
					return u.Put(TypeEmail, first, next)
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
// apply adjustCounters, stage the new record. Each Email is its own
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
						old, err := u.Get(TypeEmail, id)
						if err != nil {
							return err
						}
						next := cloneObject(old)
						next["keywords"] = kw
						if err := adjustCounters(u, old, next); err != nil {
							return err
						}
						if err := u.Put(TypeEmail, id, next); err != nil {
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
					ids, err := u.IdsWhereEqual(TypeEmail, "threadId", mustJSON(tid))
					if err != nil {
						return err
					}
					members, err := u.GetMany(TypeEmail, ids)
					if err != nil {
						return err
					}
					for _, m := range members {
						_ = viewOf(m)
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
