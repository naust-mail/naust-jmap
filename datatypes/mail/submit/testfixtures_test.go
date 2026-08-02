package submit

// Package-local test fixtures: the JMAP wire helpers and the account id
// every submit test server registers, split out because they moved here
// from root's mailbox_test.go/email_test.go alongside the files that use
// them. testsupport holds the pieces genuinely shared with root and
// deliver (the auth stub, the Email-store harness builders); these are
// thin, submit-specific wrappers plus the wire helpers too small to be
// worth centralizing.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/descriptor"
	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/record"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/testsupport"
)

const testAccount = testsupport.TestAccount

func ptrString(s string) *string { return &s }

// testReceivedAt is the fixed receivedAt putEmail stamps, matching root's.
var testReceivedAt = time.Date(2021, 3, 4, 12, 0, 0, 0, time.UTC)

func inv(name, args, callID string) jmap.Invocation {
	return jmap.Invocation{Name: name, Args: json.RawMessage(args), CallID: callID}
}

func methodArgs(t *testing.T, r *jmap.Response, i int, wantName string) map[string]any {
	t.Helper()
	if i >= len(r.MethodResponses) {
		t.Fatalf("no method response %d (have %d)", i, len(r.MethodResponses))
	}
	got := r.MethodResponses[i]
	if got.Name != wantName {
		t.Fatalf("response %d is %s (%s), want %s", i, got.Name, got.Args, wantName)
	}
	var m map[string]any
	if err := json.Unmarshal(got.Args, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// createMailbox makes one mailbox from a JSON properties object and
// returns its id.
func createMailbox(t *testing.T, ts *httptest.Server, props string) string {
	t.Helper()
	r := callSub(t, ts, inv("Mailbox/set",
		fmt.Sprintf(`{"accountId":%q,"create":{"c":%s}}`, testAccount, props), "0"))
	args := methodArgs(t, r, 0, "Mailbox/set")
	created, ok := args["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", args)
	}
	return created["c"].(map[string]any)["id"].(string)
}

// submissionCount counts the account's EmailSubmission records.
func submissionCount(t *testing.T, db *objectdb.DB) int {
	t.Helper()
	ids, err := db.AllIds(context.Background(), testAccount, record.TypeEmailSubmission, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(ids)
}

// newSenderServer is newWorkerServer with the return shape sender_test.go
// wants (queue before worker): both come from the same harness, this just
// hands the queue out directly since sender.go's tests drive it, not the
// worker's sweep. It additionally registers the record.TypeVacationNotified
// record type as a bare, method-less descriptor: sender_test.go uses that
// type name as its example of an arbitrary record an After hook writes
// alongside the send (the real ledger descriptor and its RFC 3834 meaning
// live in the deliver package, unreachable from here).
func newSenderServer(t *testing.T) (*httptest.Server, *objectdb.DB, blob.Store, *Queue, *Worker, *fakeSubmitter) {
	t.Helper()
	ts, db, store, w, fake, _ := newWorkerServer(t, DefaultLimits(), DefaultWorkerConfig())
	if err := db.RegisterType(&descriptor.Type{
		Name:       record.TypeVacationNotified,
		Capability: CapabilityURI,
		Properties: map[string]descriptor.Property{
			"sender": {Kind: descriptor.KindString, Internal: true},
			"sentAt": {Kind: descriptor.KindDate, Internal: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return ts, db, store, w.q, w, fake
}

// profPath returns a stable location for profile output, creating the
// directory on first use. Profiles must outlive the test run for
// go tool pprof, so t.TempDir (removed at cleanup) is not usable.
// Duplicated from root's zz_prof_test.go (same trivial helper).
func profPath(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(os.TempDir(), "naust-prof")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, name)
}

// emailGet fetches one Email by id with the given extra /get argument
// fragment (e.g. `,"properties":[...]`).
func emailGet(t *testing.T, ts *httptest.Server, id, extra string) map[string]any {
	t.Helper()
	args := fmt.Sprintf(`{"accountId":%q,"ids":[%q]%s}`, testAccount, id, extra)
	r := callSub(t, ts, inv("Email/get", args, "0"))
	res := methodArgs(t, r, 0, "Email/get")
	list, ok := res["list"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("Email/get list: %v", res)
	}
	return list[0].(map[string]any)
}

// putEmail parses raw, stores its blob, and creates the Email record in
// the given mailboxes with the given keywords, returning the Email id.
func putEmail(t *testing.T, db *objectdb.DB, store blob.Store, raw string, mailboxIds map[string]bool, keywords map[string]bool) string {
	return testsupport.PutEmailAt(t, db, store, testAccount, "john@example.com", raw, mailboxIds, keywords, testReceivedAt)
}
