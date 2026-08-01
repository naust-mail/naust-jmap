package emailmethods

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
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/testsupport"
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
// the per-account write lock. preflight only touches the objectdb/blob
// primitives (BlobUpload/BlobReferenced/store.Open), so this needs no
// Mailbox/Email registration.
func TestImportPreflightHoldsNoLease(t *testing.T) {
	ctx := context.Background()
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(memory.New())
	acct := testsupport.TestAccount

	// The blob is uploaded properly (record + content) and preflight runs as
	// its uploader, so the access checks pass and the open reaches the store.
	const simpleMessage = "From: Joe Bloggs <joe@example.com>\r\n" +
		"To: Jane Doe <jane@example.com>\r\n" +
		"Subject: hi\r\n" +
		"Date: Thu, 4 Mar 2021 12:00:00 +0000\r\n" +
		"\r\n" +
		"hello\r\n"
	blobID := testsupport.StoreAndRecord(t, db, store, acct, "john@example.com", simpleMessage, time.Now())
	call := &runtime.Call{Identity: &auth.Identity{Username: "john@example.com"}}

	bs := &blockingStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
	h := EmailImport{Mat: Materializer{DB: db, Store: bs}, Core: runtime.DefaultCoreCapabilities()}
	importObj := json.RawMessage(fmt.Sprintf(`{"blobId":%q,"mailboxIds":{"MBx":true}}`, blobID))

	done := make(chan struct{})
	go func() {
		h.preflight(ctx, call, acct, "c1", importObj)
		close(done)
	}()

	<-bs.entered // preflight is now blocked opening/parsing the blob

	// The account write lease must be free: an Update acquires it without
	// waiting on the in-flight parse. (Update acquires the lease before it
	// checks whether anything was staged, so an empty one still proves the
	// lease was obtainable.)
	acquired := make(chan error, 1)
	go func() {
		_, err := db.Update(ctx, acct, func(u *objectdb.Update) error { return nil })
		acquired <- err
	}()
	if err := <-acquired; err != nil {
		t.Fatalf("concurrent Update could not acquire the lease during import parse: %v", err)
	}

	close(bs.release)
	<-done
}
