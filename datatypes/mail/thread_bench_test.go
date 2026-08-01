package mail

// Benchmark for the Thread/get read path: resolving emailIds loads every
// member record (bounded by emailstore.ThreadSizeCap) to sort by
// receivedAt, so its cost against thread size is worth pinning. This
// benchmarks threadComputed.Resolve, the root-owned /get computed
// resolver (internal/emailstore/threadstat_bench_test.go has its own
// copy of the fixture-building helpers below, since a benchmark fixture
// that inserts Emails must call InsertEmail, and the two test suites
// cannot share one internal test file across the package boundary).

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
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
)

const threadBenchAccount = jmap.Id("Athreadbench")

// threadBenchDB registers the mail types on a bare object database - no
// HTTP server, no runtime - so a benchmark measures the read path alone.
func threadBenchDB(b *testing.B) (*objectdb.DB, blob.Store, jmap.Id) {
	b.Helper()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	if err := db.RegisterType(mailboxType()); err != nil {
		b.Fatal(err)
	}
	if err := db.RegisterType(threadType()); err != nil {
		b.Fatal(err)
	}
	if err := db.RegisterType(emailType()); err != nil {
		b.Fatal(err)
	}
	if err := db.RegisterType(emailDeliveryType()); err != nil {
		b.Fatal(err)
	}
	var inbox jmap.Id
	if _, err := db.Update(context.Background(), threadBenchAccount, func(u *objectdb.Update) error {
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

// threadBenchDeliver runs the shared InsertEmail path for one raw message.
func threadBenchDeliver(b *testing.B, db *objectdb.DB, store blob.Store, inbox jmap.Id, raw string, at time.Time) jmap.Id {
	b.Helper()
	ctx := context.Background()
	bw, err := store.Create(ctx, threadBenchAccount)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.WriteString(bw, raw); err != nil {
		b.Fatal(err)
	}
	blobID, err := db.FinalizeBlobUpload(ctx, threadBenchAccount, bw, "john@example.com", at)
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
	if _, err := db.Update(ctx, threadBenchAccount, func(u *objectdb.Update) error {
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

// threadBenchThread builds one thread of n members and returns its threadId.
func threadBenchThread(b *testing.B, db *objectdb.DB, store blob.Store, inbox jmap.Id, n int) jmap.Id {
	b.Helper()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var first jmap.Id
	for i := 0; i < n; i++ {
		headers := map[string]string{"Message-ID": fmt.Sprintf("<m%d@example.com>", i)}
		if i > 0 {
			headers["In-Reply-To"] = "<m0@example.com>"
		}
		id := threadBenchDeliver(b, db, store, inbox, threadMsg("Bench", headers), at.Add(time.Duration(i)*time.Minute))
		if i == 0 {
			first = id
		}
	}
	obj, err := db.Get(context.Background(), threadBenchAccount, TypeEmail, first)
	if err != nil {
		b.Fatal(err)
	}
	var tid jmap.Id
	if err := json.Unmarshal(obj["threadId"], &tid); err != nil {
		b.Fatal(err)
	}
	return tid
}

func BenchmarkThreadGetEmailIds(b *testing.B) {
	for _, n := range []int{16, 128, 512} {
		b.Run(fmt.Sprintf("members-%d", n), func(b *testing.B) {
			db, store, inbox := threadBenchDB(b)
			tid := threadBenchThread(b, db, store, inbox, n)
			ctx := context.Background()
			stored, err := db.Get(ctx, threadBenchAccount, TypeThread, tid)
			if err != nil {
				b.Fatal(err)
			}
			tc := threadComputed{db: db}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := tc.Resolve(ctx, threadBenchAccount, stored, []string{"emailIds"}, nil)
				if err != nil {
					b.Fatal(err)
				}
				if out["emailIds"] == nil {
					b.Fatal("no emailIds resolved")
				}
			}
		})
	}
}
