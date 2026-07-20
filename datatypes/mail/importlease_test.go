package mail

// Regression guard for the C3 parse-before-lease refactor: Email/import
// parses a message (in preflight) WITHOUT holding the account write lease,
// so a large message no longer stalls other writers to the account.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// blockingStore blocks the first Open until released, signalling when it has
// been entered - so a test can hold import inside preflight and observe the
// lease state meanwhile.
type blockingStore struct {
	blob.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingStore) Open(ctx context.Context, acct, id jmap.Id) (io.ReadCloser, int64, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.Store.Open(ctx, acct, id)
}

// TestImportPreflightHoldsNoLease: while Email/import is blocked parsing a
// message (inside preflight, before any Update), a concurrent write to the
// same account still acquires the lease promptly - proving the parse is off
// the per-account write lock.
func TestImportPreflightHoldsNoLease(t *testing.T) {
	ctx := context.Background()
	_, db, store := emailServer(t)

	// The blob is uploaded properly (record + content) and preflight runs as
	// its uploader, so the access checks pass and the open reaches the store.
	blobID := storeAndRecord(t, db, store, simpleMessage, testReceivedAt)
	call := &runtime.Call{Identity: &auth.Identity{Username: "john@example.com"}}

	bs := &blockingStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
	h := emailImport{mat: materializer{db: db, store: bs}, core: runtime.DefaultCoreCapabilities()}
	importObj := json.RawMessage(fmt.Sprintf(`{"blobId":%q,"mailboxIds":{"MBx":true}}`, blobID))

	done := make(chan struct{})
	go func() {
		h.preflight(ctx, call, testAccount, "c1", importObj)
		close(done)
	}()

	<-bs.entered // preflight is now blocked opening/parsing the blob

	// The account write lease must be free: an Update acquires it without
	// waiting on the in-flight parse. (Update acquires the lease before it
	// checks whether anything was staged, so an empty one still proves the
	// lease was obtainable.)
	acquired := make(chan error, 1)
	go func() {
		_, err := db.Update(ctx, testAccount, func(u *objectdb.Update) error { return nil })
		acquired <- err
	}()
	if err := <-acquired; err != nil {
		t.Fatalf("concurrent Update could not acquire the lease during import parse: %v", err)
	}

	close(bs.release)
	<-done
}
