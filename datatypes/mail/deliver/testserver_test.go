package deliver

// Package-local test fixtures: the JMAP wire helpers and server builders
// this package's tests use, moved here from root's mailbox_test.go/
// email_test.go/vacation_test.go alongside the files that need them, plus
// a submission-worker harness built on submit's exported surface (this
// package cannot reach submit's unexported test helpers - it imports
// production submit, which cannot import back).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/datatypes/mail/internal/testsupport"
	"github.com/naust-mail/naust-jmap/datatypes/mail/search"
	"github.com/naust-mail/naust-jmap/datatypes/mail/submit"
)

const testAccount = testsupport.TestAccount

// simpleMessage is a plain, well-formed message fixture. Duplicated from
// root's email_test.go (same trivial fixture, needed on both sides of a
// package that cannot import root's unexported test helpers).
const simpleMessage = "From: Joe Bloggs <joe@example.com>\r\n" +
	"To: Jane Doe <jane@example.com>\r\n" +
	"Subject: Dinner on Thursday?\r\n" +
	"Message-ID: <msg1@example.com>\r\n" +
	"Date: Wed, 03 Mar 2021 10:00:00 +0000\r\n" +
	"List-Post: <mailto:list@example.com>\r\n" +
	"\r\n" +
	"Hi Jane, are you free on Thursday evening?\r\n"

// mustDeliverer is New, failing the test on error. The trailing config
// is optional (at most one) and used verbatim; omitting it means
// DefaultConfig, so the common no-config call stays short.
func mustDeliverer(t testing.TB, db *objectdb.DB, store blob.Store, resolver Resolver, cfg ...Config) *Deliverer {
	t.Helper()
	c := DefaultConfig()
	if len(cfg) > 0 {
		c = cfg[0]
	}
	d, err := New(db, store, resolver, c)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func inv(name, args, callID string) jmap.Invocation {
	return jmap.Invocation{Name: name, Args: json.RawMessage(args), CallID: callID}
}

func methodArgs(t testing.TB, r *jmap.Response, i int, wantName string) map[string]any {
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

// callMail posts a request opted into core + mail + submission + vacation:
// the superset every test server in this package advertises.
func callMail(t testing.TB, ts *httptest.Server, calls ...jmap.Invocation) *jmap.Response {
	t.Helper()
	req := map[string]any{
		"using":       []string{jmap.CoreCapability, mail.CapabilityURI, submit.CapabilityURI, mail.VacationCapabilityURI},
		"methodCalls": calls,
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	hreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api", strings.NewReader(string(body)))
	hreq.SetBasicAuth("john@example.com", "secret")
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out jmap.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// newEmailServer wires Mailbox + Thread + Email + Identity + EmailSubmission
// + VacationResponse with a blob store, using acctCap for both the
// advertised Email capability and the enforced limits. Every test server in
// this package needs the full set: delivery lands Emails, and the vacation
// responder relays replies through a real submission queue.
func newEmailServer(t testing.TB, acctCap mail.AccountCapability) (*httptest.Server, *objectdb.DB, blob.Store, *submit.Queue, *submit.Worker, *fakeSubmitter) {
	t.Helper()
	a := testsupport.NewStaticAuth()
	a.AddUser("john@example.com", "secret", testAccount)
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithVerifyPreImages())
	store := kvstore.New(memory.New())
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	policy := mail.NewStaticSendPolicy()
	policy.Allow(testAccount, "john@example.com", "*@corp.example")
	fake := &fakeSubmitter{}
	if err := mail.RegisterMailbox(p, mail.MailboxConfig{DB: db, Core: core}); err != nil {
		t.Fatal(err)
	}
	if err := mail.RegisterThread(p, mail.ThreadConfig{DB: db, Core: core}); err != nil {
		t.Fatal(err)
	}
	if err := mail.RegisterEmail(p, mail.EmailConfig{DB: db, Store: store, Core: core, AccountCapability: acctCap, Searcher: search.New(store)}); err != nil {
		t.Fatal(err)
	}
	if err := mail.RegisterIdentity(p, mail.IdentityConfig{DB: db, Core: core, Policy: policy}); err != nil {
		t.Fatal(err)
	}
	q, err := submit.Register(p, submit.Config{DB: db, Store: store, Core: core, Policy: policy, Limits: nil})
	if err != nil {
		t.Fatal(err)
	}
	if err := mail.RegisterVacationResponse(p, mail.VacationResponseConfig{DB: db}); err != nil {
		t.Fatal(err)
	}
	w, err := submit.NewWorker(q, fake, submit.DefaultWorkerConfig())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Capability(mail.CapabilityURI).Advertise(struct{}{}, acctCap).Err(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Capability(submit.CapabilityURI).Advertise(struct{}{}, submit.AccountCapabilityFor(submit.DefaultLimits())).Err(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Capability(mail.VacationCapabilityURI).Advertise(struct{}{}, struct{}{}).Err(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, db, store, q, w, fake
}

func emailServer(t testing.TB) (*httptest.Server, *objectdb.DB, blob.Store) {
	ts, db, store, _, _, _ := newEmailServer(t, mail.DefaultAccountCapability())
	return ts, db, store
}

// createMailbox makes one mailbox from a JSON properties object and
// returns its id.
func createMailbox(t testing.TB, ts *httptest.Server, props string) string {
	t.Helper()
	r := callMail(t, ts, inv("Mailbox/set",
		fmt.Sprintf(`{"accountId":%q,"create":{"c":%s}}`, testAccount, props), "0"))
	args := methodArgs(t, r, 0, "Mailbox/set")
	created, ok := args["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", args)
	}
	return created["c"].(map[string]any)["id"].(string)
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

// writeAllocsProfile snapshots the current allocs profile to name.
// Duplicated from submit's zz_workerprof_test.go (same trivial helper).
func writeAllocsProfile(t *testing.T, name string) {
	t.Helper()
	f, err := os.Create(profPath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	goruntime.GC() // flush current allocation state into the profile
	if err := pprof.Lookup("allocs").WriteTo(f, 0); err != nil {
		t.Fatal(err)
	}
}

// mailboxCounters are the server-set count properties in the canonical
// order. Duplicated from root's mailbox.go (same trivial list; unexported
// production var).
var mailboxCounters = [...]string{"totalEmails", "unreadEmails", "totalThreads", "unreadThreads"}

// threadOf returns an Email's threadId.
func threadOf(t *testing.T, ts *httptest.Server, id string) string {
	t.Helper()
	return emailGet(t, ts, id, `,"properties":["threadId"]`)["threadId"].(string)
}

// emailQuery runs one Email/query and returns its response args (or the
// error response's args, for tests asserting a rejected query).
func emailQuery(t *testing.T, ts *httptest.Server, args string) map[string]any {
	t.Helper()
	r := callMail(t, ts, inv("Email/query", args, "0"))
	name := "Email/query"
	if r.MethodResponses[0].Name == "error" {
		name = "error"
	}
	return methodArgs(t, r, 0, name)
}

// qIds extracts the ids list from an Email/query response.
func qIds(t *testing.T, args map[string]any) []string {
	t.Helper()
	raw, ok := args["ids"].([]any)
	if !ok {
		t.Fatalf("no ids in %v", args)
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		out[i] = v.(string)
	}
	return out
}

// bodyMsg builds a plain text/plain message with the given envelope-shaped
// headers plus any extra headers, and a text body.
func bodyMsg(from, to, subject, body string, extra map[string]string) string {
	h := "From: " + from + "\r\nTo: " + to + "\r\nSubject: " + subject + "\r\n"
	for k, v := range extra {
		h += k + ": " + v + "\r\n"
	}
	return h + "\r\n" + body + "\r\n"
}

// readCounters fetches a Mailbox's four counter properties.
func readCounters(t *testing.T, ts *httptest.Server, id string) map[string]any {
	t.Helper()
	r := callMail(t, ts, inv("Mailbox/get",
		fmt.Sprintf(`{"accountId":%q,"ids":[%q],"properties":["totalEmails","unreadEmails","totalThreads","unreadThreads"]}`, testAccount, id), "0"))
	list := methodArgs(t, r, 0, "Mailbox/get")["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Mailbox/get %s: %v", id, list)
	}
	return list[0].(map[string]any)
}

// wantCounters asserts a Mailbox counters map against the four expected
// values, in mailboxCounters order.
func wantCounters(t *testing.T, label string, m map[string]any, total, unread, totalT, unreadT float64) {
	t.Helper()
	got := [4]any{m["totalEmails"], m["unreadEmails"], m["totalThreads"], m["unreadThreads"]}
	want := [4]float64{total, unread, totalT, unreadT}
	for i, name := range mailboxCounters {
		if got[i] != want[i] {
			t.Errorf("%s: %s = %v, want %v (all: %v)", label, name, got[i], want[i], got)
		}
	}
}

// snippetGet runs one SearchSnippet/get and returns its response args (or
// the error response's args).
func snippetGet(t *testing.T, ts *httptest.Server, filter string, ids ...string) map[string]any {
	t.Helper()
	idsJSON, _ := json.Marshal(ids)
	args := fmt.Sprintf(`{"accountId":%q,"filter":%s,"emailIds":%s}`, testAccount, filter, idsJSON)
	r := callMail(t, ts, inv("SearchSnippet/get", args, "0"))
	name := "SearchSnippet/get"
	if r.MethodResponses[0].Name == "error" {
		name = "error"
	}
	return methodArgs(t, r, 0, name)
}

// snippetByEmail indexes a SearchSnippet/get response's list by emailId.
func snippetByEmail(t *testing.T, args map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, ok := args["list"].([]any)
	if !ok {
		t.Fatalf("no list in %v", args)
	}
	for _, v := range list {
		m := v.(map[string]any)
		out[m["emailId"].(string)] = m
	}
	return out
}

// submissionCount counts the account's EmailSubmission records.
func submissionCount(t testing.TB, db *objectdb.DB) int {
	t.Helper()
	ids, err := db.AllIds(context.Background(), testAccount, mail.TypeEmailSubmission, 0)
	if err != nil {
		t.Fatal(err)
	}
	return len(ids)
}

// putEmail parses raw, stores its blob, and creates the Email record in
// the given mailboxes with the given keywords, returning the Email id.
func putEmail(t *testing.T, db *objectdb.DB, store blob.Store, raw string, mailboxIds map[string]bool, keywords map[string]bool) string {
	return testsupport.PutEmailAt(t, db, store, testAccount, "john@example.com", raw, mailboxIds, keywords, time.Date(2021, 3, 4, 12, 0, 0, 0, time.UTC))
}

// emailGet fetches one Email by id with the given extra /get argument
// fragment (e.g. `,"properties":[...]`).
func emailGet(t *testing.T, ts *httptest.Server, id, extra string) map[string]any {
	t.Helper()
	args := fmt.Sprintf(`{"accountId":%q,"ids":[%q]%s}`, testAccount, id, extra)
	r := callMail(t, ts, inv("Email/get", args, "0"))
	res := methodArgs(t, r, 0, "Email/get")
	list, ok := res["list"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("Email/get list: %v", res)
	}
	return list[0].(map[string]any)
}

// submitOne creates one EmailSubmission from a raw create object and
// returns the full /set response.
func submitOne(t *testing.T, ts *httptest.Server, createObj string) *jmap.Response {
	t.Helper()
	return callMail(t, ts, inv("EmailSubmission/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"s":%s}}`, testAccount, createObj), "0"))
}

// createdEcho digs the created echo for "s" out of a submission response,
// failing on rejection.
func createdEcho(t *testing.T, r *jmap.Response) map[string]any {
	t.Helper()
	created, ok := methodArgs(t, r, 0, "EmailSubmission/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("create failed: %v", r.MethodResponses[0].Args)
	}
	return created["s"].(map[string]any)
}

// submitEnvelope creates a submission with an explicit envelope and
// returns the submission id.
func submitEnvelope(t *testing.T, ts *httptest.Server, identityId, emailId, envelope string) string {
	t.Helper()
	r := submitOne(t, ts, fmt.Sprintf(
		`{"identityId":%q,"emailId":%q,"envelope":%s}`, identityId, emailId, envelope))
	return createdEcho(t, r)["id"].(string)
}

// createIdentity creates one Identity for email and returns its id.
func createIdentity(t testing.TB, ts *httptest.Server, email string) string {
	t.Helper()
	r := callMail(t, ts, inv("Identity/set", fmt.Sprintf(
		`{"accountId":%q,"create":{"i1":{"email":%q}}}`, testAccount, email), "0"))
	created, ok := methodArgs(t, r, 0, "Identity/set")["created"].(map[string]any)
	if !ok {
		t.Fatalf("identity create failed: %v", r.MethodResponses[0].Args)
	}
	return created["i1"].(map[string]any)["id"].(string)
}

// fakeSubmitter records every attempt and answers via respond (accept
// everything when nil). Duplicated from submit's own test fixture (same
// trivial type implementing submit.Submitter; submit's test file is
// unexported and this package cannot import it).
type fakeSubmitter struct {
	mu      sync.Mutex
	calls   []fakeCall
	respond func(env submit.Envelope) ([]submit.Result, error)
}

type fakeCall struct {
	env submit.Envelope
	msg string
}

func (f *fakeSubmitter) Submit(_ context.Context, env submit.Envelope, msg io.Reader) ([]submit.Result, error) {
	b, err := io.ReadAll(msg)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{env: env, msg: string(b)})
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		return respond(env)
	}
	out := make([]submit.Result, len(env.Recipients))
	for i, r := range env.Recipients {
		out[i] = submit.Result{Recipient: r.Email, Outcome: mail.Accepted, Reply: "250 2.0.0 accepted"}
	}
	return out, nil
}

func (f *fakeSubmitter) call(i int) fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}
