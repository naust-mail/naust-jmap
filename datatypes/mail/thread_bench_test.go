package mail

// Benchmark for the Thread/get read path: resolving emailIds loads every
// member record (bounded by threadSizeCap) to sort by receivedAt, so its
// cost against thread size is worth pinning.

import (
	"context"
	"fmt"
	"testing"
)

func BenchmarkThreadGetEmailIds(b *testing.B) {
	for _, n := range []int{16, 128, 512} {
		b.Run(fmt.Sprintf("members-%d", n), func(b *testing.B) {
			db, store, inbox := benchDB(b)
			_, tid := benchThread(b, db, store, inbox, n)
			ctx := context.Background()
			stored, err := db.Get(ctx, benchAccount, TypeThread, tid)
			if err != nil {
				b.Fatal(err)
			}
			tc := threadComputed{db: db}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := tc.Resolve(ctx, benchAccount, stored, []string{"emailIds"}, nil)
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
