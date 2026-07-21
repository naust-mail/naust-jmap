package mail

// Benchmarks for Email/query - the filter/sort engine, over a realistic
// mailbox. Self-contained (does not reuse the *testing.T-typed helpers in
// the other _test.go files in this package): a benchmark must not pass a
// synthetic *testing.T to a helper that can call t.Fatal, since that isn't
// wired to a running test and behaves unpredictably.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
)

// benchEmailServer is newEmailServer (email_test.go), typed for *testing.B,
// additionally returning the Processor and an Identity so a benchmark can
// call Process directly - bypassing HTTP and generic map[string]any
// response decoding, both of which turned out to dominate a first version
// of this benchmark's profile far more than the query engine itself.
func benchEmailServer(b *testing.B) (*httptest.Server, *objectdb.DB, blob.Store, *runtime.Processor, *auth.Identity) {
	b.Helper()
	a := newStaticAuth()
	a.AddUser("john@example.com", "secret", testAccount)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be))
	store := kvstore.New(memory.New())
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	if err := RegisterMailbox(p, db, core); err != nil {
		b.Fatal(err)
	}
	if err := RegisterThread(p, db, core); err != nil {
		b.Fatal(err)
	}
	if err := RegisterEmail(p, db, store, core, DefaultAccountCapability(), nil); err != nil {
		b.Fatal(err)
	}
	srv, err := runtime.NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		b.Fatal(err)
	}
	if err := srv.RegisterCapability(CapabilityURI, struct{}{}, DefaultAccountCapability()); err != nil {
		b.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	b.Cleanup(ts.Close)
	ident := &auth.Identity{
		Username: "john@example.com",
		Accounts: map[jmap.Id]auth.Access{testAccount: {Name: "john@example.com", Personal: true}},
		Primary:  testAccount,
	}
	return ts, db, store, p, ident
}

// benchCallMail is callMail (mailbox_test.go), typed for *testing.B.
func benchCallMail(b *testing.B, ts *httptest.Server, calls ...jmap.Invocation) *jmap.Response {
	b.Helper()
	req := map[string]any{
		"using":       []string{jmap.CoreCapability, CapabilityURI},
		"methodCalls": calls,
	}
	body, err := json.Marshal(req)
	if err != nil {
		b.Fatal(err)
	}
	hreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api", strings.NewReader(string(body)))
	hreq.SetBasicAuth("john@example.com", "secret")
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		b.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("status %d", resp.StatusCode)
	}
	var out jmap.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		b.Fatal(err)
	}
	return &out
}

// benchMethodArgs is methodArgs (mailbox_test.go), typed for *testing.B.
func benchMethodArgs(b *testing.B, r *jmap.Response, i int, wantName string) map[string]any {
	b.Helper()
	if i >= len(r.MethodResponses) {
		b.Fatalf("no method response %d (have %d)", i, len(r.MethodResponses))
	}
	got := r.MethodResponses[i]
	if got.Name != wantName {
		b.Fatalf("response %d is %s (%s), want %s", i, got.Name, got.Args, wantName)
	}
	var m map[string]any
	if err := json.Unmarshal(got.Args, &m); err != nil {
		b.Fatal(err)
	}
	return m
}

// benchCreateMailbox is createMailbox (mailbox_test.go), typed for *testing.B.
func benchCreateMailbox(b *testing.B, ts *httptest.Server, props string) string {
	b.Helper()
	r := benchCallMail(b, ts, inv("Mailbox/set",
		fmt.Sprintf(`{"accountId":%q,"create":{"c":%s}}`, testAccount, props), "0"))
	args := benchMethodArgs(b, r, 0, "Mailbox/set")
	created, ok := args["created"].(map[string]any)
	if !ok {
		b.Fatalf("create failed: %v", args)
	}
	return created["c"].(map[string]any)["id"].(string)
}

// benchPutEmailAt is putEmailAt (email_test.go), typed for *testing.B - runs
// the same insertEmail path delivery uses, so this benchmarks real record
// shape, not a synthetic shortcut.
func benchPutEmailAt(b *testing.B, db *objectdb.DB, store blob.Store, raw string, mailboxIds, keywords map[string]bool, receivedAt time.Time) string {
	b.Helper()
	ctx := context.Background()
	bw, err := store.Create(ctx, testAccount)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.WriteString(bw, raw); err != nil {
		b.Fatal(err)
	}
	blobID, err := db.FinalizeBlobUpload(ctx, testAccount, bw, "john@example.com", receivedAt)
	if err != nil {
		b.Fatal(err)
	}
	mb, _ := json.Marshal(mailboxIds)
	var kw json.RawMessage
	if keywords != nil {
		kw, _ = json.Marshal(keywords)
	}
	c := newCapture()
	c.preview = true
	msg, err := parseMessage(strings.NewReader(raw), c)
	if err != nil {
		b.Fatal(err)
	}
	var id jmap.Id
	if _, err := db.Update(ctx, testAccount, func(u *objectdb.Update) error {
		created, err := insertEmail(u, msg, emailMeta{
			BlobID: blobID, MailboxIds: mb, Keywords: kw,
			Size: uint64(len(raw)), ReceivedAt: receivedAt,
		})
		id = created
		return err
	}); err != nil {
		b.Fatal(err)
	}
	return string(id)
}

// seedQueryBench populates inbox with n Emails, half flagged and seen,
// alternating senders, spread a minute apart - enough records for
// filter/sort cost to show up, varied enough that conditions actually
// discriminate rather than short-circuiting.
func seedQueryBench(b *testing.B, db *objectdb.DB, store blob.Store, inbox string, n int) {
	b.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		from := "alice@example.com"
		if i%2 == 0 {
			from = "bob@example.com"
		}
		kw := map[string]bool{"$seen": true}
		if i%3 == 0 {
			kw["$flagged"] = true
		}
		raw := "From: " + from + "\r\nTo: list@example.com\r\nSubject: " +
			fmt.Sprintf("Subject line %d", i) +
			"\r\n\r\nBody text for benchmarking the query engine over a realistic mailbox.\r\n"
		benchPutEmailAt(b, db, store, raw, mset(inbox), kw, base.Add(time.Duration(i)*time.Minute))
	}
}

// BenchmarkEmailQueryFilterSort goes over real HTTP with generic
// map[string]any response decoding - the shape a real client's round trip
// takes, decode overhead included on purpose.
func BenchmarkEmailQueryFilterSort(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("%d_emails", n), func(b *testing.B) {
			ts, db, store, _, _ := benchEmailServer(b)
			inbox := benchCreateMailbox(b, ts, `{"name":"Inbox","role":"inbox"}`)
			seedQueryBench(b, db, store, inbox, n)

			args := fmt.Sprintf(`{"accountId":%q,"filter":{"operator":"AND","conditions":[`+
				`{"inMailbox":%q},{"hasKeyword":"$flagged"}]},`+
				`"sort":[{"property":"receivedAt","isAscending":false}],`+
				`"calculateTotal":true}`, testAccount, inbox)

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				r := benchCallMail(b, ts, inv("Email/query", args, "0"))
				got := benchMethodArgs(b, r, 0, "Email/query")
				if got["ids"] == nil {
					b.Fatal("query returned no ids field")
				}
			}
		})
	}
}

// BenchmarkEmailQueryFilterSortDirect calls Processor.Process directly - no
// HTTP, no request/response JSON at all - to isolate the filter/sort engine
// itself from the transport and decode overhead BenchmarkEmailQueryFilterSort
// also measures.
func BenchmarkEmailQueryFilterSortDirect(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("%d_emails", n), func(b *testing.B) {
			ts, db, store, p, ident := benchEmailServer(b)
			inbox := benchCreateMailbox(b, ts, `{"name":"Inbox","role":"inbox"}`)
			seedQueryBench(b, db, store, inbox, n)

			args := json.RawMessage(fmt.Sprintf(`{"accountId":%q,"filter":{"operator":"AND","conditions":[`+
				`{"inMailbox":%q},{"hasKeyword":"$flagged"}]},`+
				`"sort":[{"property":"receivedAt","isAscending":false}],`+
				`"calculateTotal":true}`, testAccount, inbox))
			req := &jmap.Request{
				Using:       []string{jmap.CoreCapability, CapabilityURI},
				MethodCalls: []jmap.Invocation{{Name: "Email/query", Args: args, CallID: "0"}},
			}
			ctx := context.Background()

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				resp := p.Process(ctx, req, ident, "s")
				if len(resp.MethodResponses) != 1 || resp.MethodResponses[0].Name != "Email/query" {
					b.Fatalf("unexpected response: %+v", resp.MethodResponses)
				}
			}
		})
	}
}
