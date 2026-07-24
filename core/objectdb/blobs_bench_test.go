package objectdb

// Sweep benchmarks. The pending hints make a steady-state sweep's cost
// independent of how many live blobs the account holds; the full-scan
// baseline reproduces the shape the sweep had before the hints (walk
// every upload record and check each against the reference index), so
// the pair shows what an hourly maintenance pass costs a large account
// under each model.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
)

// benchPopulate builds an account with n uploaded, referenced blobs -
// the steady state of a mailbox with n delivered messages.
func benchPopulate(b *testing.B, n int) (*DB, blob.Store) {
	b.Helper()
	be := memory.New()
	db := New(be, lease.NewInProcess(be))
	if err := db.RegisterType(docType()); err != nil {
		b.Fatal(err)
	}
	store := kvstore.New(be)
	ctx := context.Background()
	at := time.Now().Add(-2 * time.Hour)
	for i := 0; i < n; i++ {
		bw, err := store.Create(ctx, acct)
		if err != nil {
			b.Fatal(err)
		}
		fmt.Fprintf(bw, "bench-blob-%d", i)
		blobID, err := db.FinalizeBlobUpload(ctx, acct, bw, "alice", at)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := db.Update(ctx, acct, func(u *Update) error {
			_, err := u.Create("TestDoc", docFor(blobID))
			return err
		}); err != nil {
			b.Fatal(err)
		}
	}
	return db, store
}

// BenchmarkSweepSteadyState: every blob is referenced, so the hint
// range is empty and one sweep is a lease acquire plus one empty scan,
// whatever the account holds.
func BenchmarkSweepSteadyState(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("liveblobs-%d", n), func(b *testing.B) {
			db, store := benchPopulate(b, n)
			now := time.Now()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := db.SweepBlobs(context.Background(), acct, store, now, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSweepFullScanBaseline: the pre-hint shape - decode every
// upload record and probe the reference index for each aged one. It
// runs without the account lease, so it still understates what the old
// sweep cost the account's writers.
func BenchmarkSweepFullScanBaseline(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("liveblobs-%d", n), func(b *testing.B) {
			db, _ := benchPopulate(b, n)
			now := time.Now()
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var candidates []jmap.Id
				start, end := prefixRange(seg(string(acct)), seg("u"))
				err := db.be.Scan(ctx, start, end, false, func(k, v []byte) bool {
					var rec BlobUpload
					if json.Unmarshal(v, &rec) != nil {
						return false
					}
					if now.Sub(rec.UploadedAt) >= time.Hour {
						candidates = append(candidates, idFromObjKey(k))
					}
					return true
				})
				if err != nil {
					b.Fatal(err)
				}
				for _, id := range candidates {
					if _, err := db.BlobReferenced(ctx, acct, id); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
