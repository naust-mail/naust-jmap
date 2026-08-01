package testsupport

// Email-store harness builders shared by the root, submit, and deliver test
// packages: staging a blob and inserting an Email the way delivery would,
// without going through the JMAP Email/set surface any one package's tests
// are exercising.

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/emailstore"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/parse"
)

// StoreAndRecord streams content into a fresh blob writer and finalizes it
// (record then publish) under acct, the way delivery establishes a
// message's blob, and returns its content-addressed id. The record must
// exist or the referential blobId check rejects later Email updates.
func StoreAndRecord(t *testing.T, db *objectdb.DB, store blob.Store, acct jmap.Id, uploader, content string, at time.Time) jmap.Id {
	t.Helper()
	ctx := context.Background()
	bw, err := store.Create(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(bw, content); err != nil {
		t.Fatal(err)
	}
	id, err := db.FinalizeBlobUpload(ctx, acct, bw, uploader, at)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// PutEmailAt parses raw, stores its blob, and creates the Email record
// under acct in the given mailboxes with the given keywords at receivedAt,
// returning the Email id. It runs the shared insertEmail path, so
// threading and the Mailbox counters are exercised exactly as delivery
// exercises them.
func PutEmailAt(t *testing.T, db *objectdb.DB, store blob.Store, acct jmap.Id, uploader, raw string, mailboxIds map[string]bool, keywords map[string]bool, receivedAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	blobID := StoreAndRecord(t, db, store, acct, uploader, raw, receivedAt)
	mb, _ := json.Marshal(mailboxIds)
	var kw json.RawMessage
	if keywords != nil {
		kw, _ = json.Marshal(keywords)
	}
	c := parse.NewCapture()
	c.Preview = true
	msg, err := parse.ParseMessage(strings.NewReader(raw), c)
	if err != nil {
		t.Fatal(err)
	}
	var id jmap.Id
	if _, err := db.Update(ctx, acct, func(u *objectdb.Update) error {
		created, err := emailstore.InsertEmail(u, msg, emailstore.EmailMeta{
			BlobID: blobID, MailboxIds: mb, Keywords: kw,
			Size: uint64(len(raw)), ReceivedAt: receivedAt,
		})
		id = created
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return string(id)
}
