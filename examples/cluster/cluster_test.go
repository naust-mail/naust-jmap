// Package cluster is an end-to-end proof that two independent naust-jmap
// mail stacks sharing one Postgres database behave as one coherent service:
// writes to the same account serialize, a stale writer's commit fails its
// fence cleanly instead of corrupting, change notifications and submission
// wakes cross the process boundary over the LISTEN/NOTIFY hint transport,
// and the timer-driven sweep still drains work when that transport is absent.
//
// Each "instance" here is a full object graph of its own - its own Postgres
// connection pool, its own hint transport, its own store lease, its own
// Server and submission worker - never a shared Go object. Two such graphs in
// one test binary contend inside Postgres exactly as two operating-system
// processes would (advisory locks and LISTEN are per-connection, not
// per-process), with the advantage that the race detector sees both sides.
//
// The suite requires a real shared server: PG_TEST_DSN must point at a
// Postgres instance (e.g. postgres://user:pass@host:5432/db). Tests SKIP, not
// fail, when it is unset, because none of these guarantees can be exercised in
// process. Each test namespaces its accounts with a per-run unique suffix, so
// repeated runs against the same database never collide.
package cluster

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/jmap"
	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/providers/notify"
	"github.com/naust-mail/naust-jmap/core/runtime"
	"github.com/naust-mail/naust-jmap/datatypes/mail"
	"github.com/naust-mail/naust-jmap/drivers/postgres"
	"github.com/naust-mail/naust-jmap/examples/internal/tokenauth"
)

const dsnEnv = "PG_TEST_DSN"

// runNonce distinguishes this test binary's accounts from any left in the
// database by an earlier run, so a shared server needs no reset between runs.
var runNonce = strconv.FormatInt(time.Now().UnixNano(), 36)

// acctSeq numbers accounts within this run, appended to runNonce for a
// database-unique account id and address per test.
var acctSeq atomic.Int64

// testAccount is one login: an address, its password, and the account id
// reached through it. sendAs, when non-empty, is the address the account's
// send policy permits as the envelope sender.
type testAccount struct {
	addr   string
	pass   string
	id     jmap.Id
	sendAs string
}

// newAccount mints a run-unique account. sendable makes it able to send as
// its own address (a pre-provisioned Identity and an allowing send policy).
func newAccount(sendable bool) testAccount {
	n := acctSeq.Add(1)
	addr := fmt.Sprintf("u%s-%d@example.com", runNonce, n)
	a := testAccount{
		addr: addr,
		pass: "pw",
		id:   jmap.Id(fmt.Sprintf("Acl%s-%d", runNonce, n)),
	}
	if sendable {
		a.sendAs = addr
	}
	return a
}

// baseDSN returns the shared-database DSN or skips the test when unset.
func baseDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping cluster tests", dsnEnv)
	}
	return dsn
}

// resolver maps envelope recipients to accounts for delivery, the host's job
// in every naust-jmap deployment (the delivery core bakes in no addressing).
type resolver map[string]jmap.Id

func (r resolver) Resolve(_ context.Context, recipient string) (jmap.Id, bool) {
	id, ok := r[recipient]
	return id, ok
}

// instanceConfig selects how one instance is wired.
type instanceConfig struct {
	// linked bridges this instance's post-commit notifier to the others over
	// the Postgres LISTEN/NOTIFY hint transport. false gives an isolated
	// in-process notifier that no other instance can reach - the "hint
	// transport down" condition, under which only the worker's timer sweep
	// carries cross-instance work.
	linked bool
	// leaseExpiry and leasePoll tune the store lease; zero uses its defaults.
	leaseExpiry time.Duration
	leasePoll   time.Duration
	// submitter, when set, runs a submission worker with this Submitter and
	// scanInterval as its reconciliation cadence.
	submitter    mail.Submitter
	scanInterval time.Duration
}

// instance is one complete naust-jmap stack over a shared Postgres database.
type instance struct {
	store     *postgres.Store
	hints     *postgres.Hints
	db        *objectdb.DB
	leases    lease.Manager
	proc      *runtime.Processor
	srv       *runtime.Server
	ts        *httptest.Server
	users     *tokenauth.Authenticator
	deliverer *mail.Deliverer
	resolve   resolver
}

// newInstance builds one instance: its own connection pool onto the shared
// database, its own coordination (store lease plus, when linked, the hint
// transport), the full mail plugin, an HTTP front door, and optionally a
// submission worker. accts are registered for login, delivery, and sending.
func newInstance(t *testing.T, dsn string, accts []testAccount, cfg instanceConfig) *instance {
	t.Helper()
	ctx := context.Background()

	store, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// Coordination: the store lease excludes writers across instances at
	// Acquire; the hint transport (when linked) accelerates handoff and
	// carries change notifications between instances.
	var notifier notify.Notifier
	var hints *postgres.Hints
	leaseCfg := lease.StoreLeaseConfig{Expiry: cfg.leaseExpiry, Poll: cfg.leasePoll}
	if cfg.linked {
		hints, err = postgres.OpenHints(ctx, store)
		if err != nil {
			t.Fatalf("open hints: %v", err)
		}
		t.Cleanup(func() { hints.Close() })
		leaseCfg.Waker = hints.Waker()
		notifier = hints.Notifier()
	} else {
		notifier = notify.NewInProcess()
	}
	leases := lease.NewStoreLease(store, leaseCfg)
	db := objectdb.New(store, leases)
	blobs := kvstore.New(store)

	users := tokenauth.New()
	resolve := resolver{}
	policy := mail.NewStaticSendPolicy()
	for _, a := range accts {
		users.AddUser(a.addr, a.pass, a.id)
		resolve[a.addr] = a.id
		if a.sendAs != "" {
			policy.Allow(a.id, a.sendAs)
		}
	}

	proc := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	acctCap := mail.DefaultAccountCapability()
	if err := mail.RegisterMailbox(proc, db, core); err != nil {
		t.Fatal(err)
	}
	if err := mail.RegisterThread(proc, db, core); err != nil {
		t.Fatal(err)
	}
	if err := mail.RegisterEmail(proc, db, blobs, core, acctCap, nil); err != nil {
		t.Fatal(err)
	}
	limits := mail.DefaultSubmissionLimits()
	if err := mail.RegisterIdentity(proc, db, policy, core); err != nil {
		t.Fatal(err)
	}
	queue, err := mail.RegisterEmailSubmission(proc, db, blobs, core, policy, limits)
	if err != nil {
		t.Fatal(err)
	}

	srv, err := runtime.NewServer(users, proc, "http://cluster.example", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(mail.CapabilityURI, struct{}{}, acctCap); err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(mail.SubmissionCapabilityURI, struct{}{}, mail.SubmissionAccountCapabilityFor(limits)); err != nil {
		t.Fatal(err)
	}
	srv.EnableBlobs(db, blobs)
	if err := srv.EnablePush(db, notifier, nil, nil); err != nil {
		t.Fatal(err)
	}

	deliverer := mail.NewDeliverer(db, blobs, resolve)

	if cfg.submitter != nil {
		worker, err := mail.NewSubmissionWorker(queue, cfg.submitter,
			mail.SubmissionWorkerConfig{QueueScanInterval: cfg.scanInterval})
		if err != nil {
			t.Fatal(err)
		}
		wctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		go worker.Run(wctx)
	}

	root := http.NewServeMux()
	root.Handle("/login", users.LoginHandler())
	root.Handle("/", srv)
	ts := httptest.NewServer(root)
	t.Cleanup(ts.Close)

	return &instance{
		store:     store,
		hints:     hints,
		db:        db,
		leases:    leases,
		proc:      proc,
		srv:       srv,
		ts:        ts,
		users:     users,
		deliverer: deliverer,
		resolve:   resolve,
	}
}

// identity builds the server-side auth identity for an account, so a test can
// drive JMAP methods through the processor without a login round trip (the
// event-source stream, which is HTTP-only, still logs in for a bearer token).
func (in *instance) identity(a testAccount) *auth.Identity {
	return &auth.Identity{
		Username: a.addr,
		Accounts: map[jmap.Id]auth.Access{a.id: {Name: a.addr, Personal: true}},
	}
}

// call runs one method and returns its response arguments decoded, failing on
// a method-level error response.
func (in *instance) call(t *testing.T, ident *auth.Identity, using []string, name, args string) map[string]any {
	t.Helper()
	resp := in.proc.Process(context.Background(), &jmap.Request{
		Using:       using,
		MethodCalls: []jmap.Invocation{{Name: name, Args: json.RawMessage(args), CallID: "0"}},
	}, ident, "")
	if len(resp.MethodResponses) == 0 {
		t.Fatalf("%s: no method responses", name)
	}
	got := resp.MethodResponses[0]
	if got.Name != name {
		t.Fatalf("%s failed: %s %s", name, got.Name, got.Args)
	}
	var out map[string]any
	if err := json.Unmarshal(got.Args, &out); err != nil {
		t.Fatalf("%s: decode response: %v", name, err)
	}
	return out
}

// createInbox creates an inbox-role Mailbox, without which delivery tempfails.
func (in *instance) createInbox(t *testing.T, a testAccount) string {
	t.Helper()
	out := in.call(t, in.identity(a), []string{jmap.CoreCapability, mail.CapabilityURI},
		"Mailbox/set", fmt.Sprintf(`{"accountId":%q,"create":{"i":{"name":"Inbox","role":"inbox"}}}`, a.id))
	created, ok := out["created"].(map[string]any)
	if !ok {
		t.Fatalf("create inbox failed: %v", out)
	}
	return created["i"].(map[string]any)["id"].(string)
}

// ensureIdentity provisions one sending Identity for the account through the
// normal Identity/set front door.
func (in *instance) ensureIdentity(t *testing.T, a testAccount) string {
	t.Helper()
	using := []string{jmap.CoreCapability, mail.SubmissionCapabilityURI}
	out := in.call(t, in.identity(a), using, "Identity/set",
		fmt.Sprintf(`{"accountId":%q,"create":{"i":{"email":%q}}}`, a.id, a.sendAs))
	created, ok := out["created"].(map[string]any)
	if !ok {
		t.Fatalf("create identity failed: %v", out)
	}
	return created["i"].(map[string]any)["id"].(string)
}

// deliver runs one message through the delivery pipeline (the path LMTP and
// HTTP ingest share) and returns the per-recipient events.
func (in *instance) deliver(from, to, raw string) []mail.DeliveryEvent {
	return in.deliverer.Deliver(context.Background(),
		mail.Envelope{MailFrom: from, Recipients: []string{to}}, strings.NewReader(raw))
}

// emailIDs returns the account's Email ids via Email/query.
func (in *instance) emailIDs(t *testing.T, a testAccount) []string {
	t.Helper()
	out := in.call(t, in.identity(a), []string{jmap.CoreCapability, mail.CapabilityURI},
		"Email/query", fmt.Sprintf(`{"accountId":%q}`, a.id))
	raw, ok := out["ids"].([]any)
	if !ok {
		t.Fatalf("Email/query returned no ids: %v", out)
	}
	ids := make([]string, len(raw))
	for i, v := range raw {
		ids[i] = v.(string)
	}
	return ids
}

// deliveredMsg is a minimal RFC 5322 message from and to the given addresses,
// made unique by n so each delivery stores a distinct Email.
func deliveredMsg(from, to string, n int) string {
	return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: m%d\r\n"+
		"Message-ID: <m%s-%d@example.com>\r\nDate: Thu, 17 Jul 2026 10:00:00 +0000\r\n\r\nbody %d\r\n",
		from, to, n, runNonce, n, n)
}

// login mints a bearer token the HTTP-only event-source stream needs.
func login(t *testing.T, ts *httptest.Server, addr, pass string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(addr, pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status %d: %s", resp.StatusCode, body)
	}
	return strings.TrimSpace(string(body))
}

// sseClient reads a text/event-stream one event at a time.
type sseClient struct {
	resp *http.Response
	br   *bufio.Reader
}

// openEventSource connects to an instance's event-source stream as the account.
func openEventSource(t *testing.T, in *instance, a testAccount, query string) *sseClient {
	t.Helper()
	token := login(t, in.ts, a.addr, a.pass)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.ts.URL+"/eventsource?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open event source: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("event source status %d: %s", resp.StatusCode, body)
	}
	return &sseClient{resp: resp, br: bufio.NewReader(resp.Body)}
}

// readState returns the changed map of the next StateChange event (RFC 8620
// section 7.1 shape), or fails at end of stream.
func (c *sseClient) readState(t *testing.T) map[string]map[string]string {
	t.Helper()
	var name, data string
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			t.Fatalf("read event source: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && name != "":
			if name != "state" {
				t.Fatalf("event %q, want state", name)
			}
			var sc struct {
				Changed map[string]map[string]string `json:"changed"`
			}
			if err := json.Unmarshal([]byte(data), &sc); err != nil {
				t.Fatalf("decode state %q: %v", data, err)
			}
			return sc.Changed
		}
	}
}

// countingSubmitter is a Submitter that accepts every recipient and counts
// the submissions it transmitted, so a test can observe which instance's
// worker drained the queue.
type countingSubmitter struct {
	mu    sync.Mutex
	calls int
}

func (s *countingSubmitter) Submit(_ context.Context, env mail.SubmissionEnvelope, _ io.Reader) ([]mail.RecipientResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	out := make([]mail.RecipientResult, len(env.Recipients))
	for i, r := range env.Recipients {
		out[i] = mail.RecipientResult{Recipient: r.Email, Outcome: mail.Accepted, Reply: "250 2.0.0 accepted"}
	}
	return out, nil
}

func (s *countingSubmitter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// waitFor polls until cond is true or the deadline elapses.
func waitFor(t *testing.T, why string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", why)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestClusterSameAccountSerialization delivers to one account concurrently
// from two instances sharing the database. The store lease excludes the two
// writers at Acquire, so every delivery commits under the generation it
// acquired: all deliveries are accepted and the final Email count is exactly
// the number delivered, with no write lost to a race.
func TestClusterSameAccountSerialization(t *testing.T) {
	dsn := baseDSN(t)
	const perInstance = 40
	acct := newAccount(false)
	accts := []testAccount{acct}

	a := newInstance(t, dsn, accts, instanceConfig{linked: true})
	b := newInstance(t, dsn, accts, instanceConfig{linked: true})
	a.createInbox(t, acct)

	var accepted atomic.Int64
	var wg sync.WaitGroup
	// Each instance offsets its Message-ID counter so the two never collide.
	for _, pair := range []struct {
		in   *instance
		base int
	}{{a, 0}, {b, 100000}} {
		wg.Add(1)
		go func(in *instance, base int) {
			defer wg.Done()
			for n := 0; n < perInstance; n++ {
				for _, ev := range in.deliver(acct.addr, acct.addr, deliveredMsg(acct.addr, acct.addr, base+n)) {
					if ev.Outcome == mail.Accepted {
						accepted.Add(1)
					} else {
						t.Errorf("delivery not accepted: %s %s", ev.Outcome, ev.Reason)
					}
				}
			}
		}(pair.in, pair.base)
	}
	wg.Wait()

	if got := accepted.Load(); got != 2*perInstance {
		t.Fatalf("accepted %d deliveries, want %d", got, 2*perInstance)
	}
	ids := a.emailIDs(t, acct)
	if len(ids) != 2*perInstance {
		t.Fatalf("final Email count %d, want %d (a write was lost to a race)", len(ids), 2*perInstance)
	}
}

// TestClusterExpiryStealFenceFails is the safety guarantee under crash or
// hang: instance A holds an account's lease and never releases it; once the
// claim's short expiry lapses, instance B legitimately takes the account over
// with a fresh token. A's next fenced commit must then fail with
// backend.ErrAssertFailed - the clean tempfail a stale writer sees, which the
// method layer surfaces as a retryable error rather than corrupting the
// account - and A's own Release must be a harmless no-op on the stolen claim.
func TestClusterExpiryStealFenceFails(t *testing.T) {
	dsn := baseDSN(t)
	ctx := context.Background()
	account := jmap.Id(fmt.Sprintf("Asteal%s-%d", runNonce, acctSeq.Add(1)))

	storeA, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storeA.Close() })
	storeB, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { storeB.Close() })

	const expiry = 150 * time.Millisecond
	mgrA := lease.NewStoreLease(storeA, lease.StoreLeaseConfig{Expiry: expiry, Poll: 20 * time.Millisecond})
	mgrB := lease.NewStoreLease(storeB, lease.StoreLeaseConfig{Expiry: expiry, Poll: 20 * time.Millisecond})

	// A acquires and, standing in for a crashed or hung process, never
	// releases. Its lease is valid only until the expiry.
	leaseA, err := mgrA.Acquire(ctx, account)
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	// After the expiry, B takes the account over. Its Acquire must succeed
	// against the abandoned claim and mint a new token.
	time.Sleep(2 * expiry)
	leaseB, err := mgrB.Acquire(ctx, account)
	if err != nil {
		t.Fatalf("B acquire after A's expiry: %v", err)
	}

	// A, unaware it was superseded, tries to commit. Its fence asserts the
	// old token, which B has overwritten, so the batch fails cleanly.
	var b backend.Batch
	leaseA.Fence(&b)
	b.Set([]byte("cd/"+string(account)), []byte("stale"))
	err = storeA.WriteBatch(ctx, &b)
	if !errorIsAssertFailed(err) {
		t.Fatalf("A's stale commit err = %v, want ErrAssertFailed", err)
	}
	// A's Release must not disturb B's live claim.
	leaseA.Release()

	// B, holding the account legitimately, commits successfully.
	var b2 backend.Batch
	leaseB.Fence(&b2)
	b2.Set([]byte("cd/"+string(account)), []byte("fresh"))
	if err := storeB.WriteBatch(ctx, &b2); err != nil {
		t.Fatalf("B's legitimate commit failed: %v", err)
	}
	leaseB.Release()
}

// errorIsAssertFailed reports whether err is the fence-failed sentinel.
func errorIsAssertFailed(err error) bool {
	return err != nil && strings.Contains(err.Error(), backend.ErrAssertFailed.Error())
}

// TestClusterCrossInstanceEventSourcePush proves a change on one instance
// reaches an event-source subscriber on another. A client streams from
// instance B; a delivery commits on instance A; B's stream must carry a
// StateChange for that account, carried across the process boundary by the
// LISTEN/NOTIFY hint transport (RFC 8620 section 7.1).
func TestClusterCrossInstanceEventSourcePush(t *testing.T) {
	dsn := baseDSN(t)
	acct := newAccount(false)
	accts := []testAccount{acct}

	a := newInstance(t, dsn, accts, instanceConfig{linked: true})
	b := newInstance(t, dsn, accts, instanceConfig{linked: true})
	a.createInbox(t, acct)

	// Subscribe on B and consume the initial full-state event so the next
	// event can only be the delivery A is about to commit.
	c := openEventSource(t, b, acct, "types=*&closeafter=no&ping=0")
	c.readState(t)

	events := a.deliver(acct.addr, acct.addr, deliveredMsg(acct.addr, acct.addr, 1))
	if len(events) != 1 || events[0].Outcome != mail.Accepted {
		t.Fatalf("delivery on A not accepted: %v", events)
	}

	changed := c.readState(t)
	if _, ok := changed[string(acct.id)]; !ok {
		t.Fatalf("B's stream carried no change for %s: %v", acct.id, changed)
	}
}

// TestClusterCrossInstanceWorkerWake proves a submission created on one
// instance is picked up promptly by another instance's worker over the hint
// transport. B's worker scans only once an hour, so a fast pickup can only be
// the Notifier wake carrying A's EmailSubmission commit across the boundary.
func TestClusterCrossInstanceWorkerWake(t *testing.T) {
	dsn := baseDSN(t)
	acct := newAccount(true)
	accts := []testAccount{acct}

	// B runs the worker; its hour-long scan makes the timer sweep an
	// implausible explanation for a quick send.
	sub := &countingSubmitter{}
	a := newInstance(t, dsn, accts, instanceConfig{linked: true})
	newInstance(t, dsn, accts, instanceConfig{linked: true, submitter: sub, scanInterval: time.Hour})
	a.createInbox(t, acct)
	identityID := a.ensureIdentity(t, acct)

	emailID := deliverSendable(t, a, acct)

	// Create the submission on A. Its commit publishes over the shared hint
	// transport, waking B's worker even though nothing rings B's bell directly.
	submit(t, a, acct, identityID, emailID)
	waitFor(t, "B's worker to send A's submission", 10*time.Second, func() bool { return sub.count() >= 1 })
}

// TestClusterNotifyDownSweepReconciliation proves correctness never depends on
// the hint transport. B's worker is left unlinked (an isolated notifier no
// other instance can reach), so A's submission commit produces no wake on B;
// only B's timer sweep can discover it. With a short scan interval the sweep
// still drains the queue, confirming the tier-3 fallback stands alone.
func TestClusterNotifyDownSweepReconciliation(t *testing.T) {
	dsn := baseDSN(t)
	acct := newAccount(true)
	accts := []testAccount{acct}

	sub := &countingSubmitter{}
	a := newInstance(t, dsn, accts, instanceConfig{linked: true})
	newInstance(t, dsn, accts, instanceConfig{linked: false, submitter: sub, scanInterval: 150 * time.Millisecond})
	a.createInbox(t, acct)
	identityID := a.ensureIdentity(t, acct)

	emailID := deliverSendable(t, a, acct)
	submit(t, a, acct, identityID, emailID)

	// No hint can reach B here; only its periodic sweep drains the queue.
	waitFor(t, "B's timer sweep to drain the queue", 10*time.Second, func() bool { return sub.count() >= 1 })
}

// TestClusterExactCountReconciliation is the automated reconciliation: two
// instances deliver a contended stream to one account, and afterward the
// number of Emails the account holds must equal the number of deliveries that
// reported acceptance, with every id distinct. A lost write or a double-commit
// under contention would break the equality or the uniqueness.
func TestClusterExactCountReconciliation(t *testing.T) {
	dsn := baseDSN(t)
	const perInstance = 60
	acct := newAccount(false)
	accts := []testAccount{acct}

	a := newInstance(t, dsn, accts, instanceConfig{linked: true})
	b := newInstance(t, dsn, accts, instanceConfig{linked: true})
	a.createInbox(t, acct)

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for _, pair := range []struct {
		in   *instance
		base int
	}{{a, 0}, {b, 100000}} {
		wg.Add(1)
		go func(in *instance, base int) {
			defer wg.Done()
			for n := 0; n < perInstance; n++ {
				for _, ev := range in.deliver(acct.addr, acct.addr, deliveredMsg(acct.addr, acct.addr, base+n)) {
					if ev.Outcome == mail.Accepted {
						accepted.Add(1)
					}
				}
			}
		}(pair.in, pair.base)
	}
	wg.Wait()

	ids := a.emailIDs(t, acct)
	if int64(len(ids)) != accepted.Load() {
		t.Fatalf("reconciliation mismatch: %d Emails stored, %d deliveries accepted", len(ids), accepted.Load())
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate Email id %s", id)
		}
		seen[id] = true
	}
}

// deliverSendable delivers a message from and to the account's own address,
// producing a stored Email whose From matches the account's Identity so it can
// be submitted, and returns its id.
func deliverSendable(t *testing.T, in *instance, a testAccount) string {
	t.Helper()
	events := in.deliver(a.addr, a.addr, deliveredMsg(a.addr, a.addr, 1))
	if len(events) != 1 || events[0].Outcome != mail.Accepted {
		t.Fatalf("sendable delivery not accepted: %v", events)
	}
	ids := in.emailIDs(t, a)
	if len(ids) == 0 {
		t.Fatal("no Email stored after delivery")
	}
	return ids[0]
}

// submit creates one EmailSubmission for the email through the front door.
func submit(t *testing.T, in *instance, a testAccount, identityID, emailID string) {
	t.Helper()
	using := []string{jmap.CoreCapability, mail.SubmissionCapabilityURI}
	args := fmt.Sprintf(`{"accountId":%q,"create":{"s":{"identityId":%q,"emailId":%q,`+
		`"envelope":{"mailFrom":{"email":%q},"rcptTo":[{"email":"dest@remote.example"}]}}}}`,
		a.id, identityID, emailID, a.sendAs)
	out := in.call(t, in.identity(a), using, "EmailSubmission/set", args)
	if _, ok := out["created"].(map[string]any)["s"]; !ok {
		t.Fatalf("submission not created: %v", out)
	}
}
