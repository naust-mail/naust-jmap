package mail

// Throwaway: what does each Email/queryChanges answering tier cost
// relative to the client's alternative, a full /query refetch?
// (RFC 8620 section 5.6.) Tier 0 answers from the per-type state
// comparison alone, tier 1 evaluates the predicate against only the
// changed ids, tier 2 re-runs the shared query pipeline. Ops are
// interleaved round-robin so no bucket benefits from cache warmup the
// others lack. In-memory backend, so the numbers are pure compute plus
// one local HTTP round trip each (identical overhead per bucket).

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/naust-mail/naust-jmap/core/objectdb"
	"github.com/naust-mail/naust-jmap/core/providers/auth"
	"github.com/naust-mail/naust-jmap/core/providers/backend/memory"
	"github.com/naust-mail/naust-jmap/core/providers/blob"
	"github.com/naust-mail/naust-jmap/core/providers/blob/kvstore"
	"github.com/naust-mail/naust-jmap/core/providers/lease"
	"github.com/naust-mail/naust-jmap/core/runtime"
)

// profileServer is newEmailServer without Processor.
// VerifyQueryProjection: that assertion mode re-runs every projected
// query against a full decode, which would double exactly the cost
// this test measures. WithVerifyPreImages stays on so the write-path
// numbers remain comparable across this test's history.
func profileServer(t *testing.T) (*httptest.Server, *objectdb.DB, blob.Store) {
	t.Helper()
	a := newStaticAuth()
	a.AddUser("john@example.com", "secret", testAccount)
	a.AddUser("jane@example.com", "secret", "Ajane")
	a.AddAccess("jane@example.com", testAccount, auth.Access{Name: "shared"})
	be := memory.New()
	db := objectdb.New(be, lease.NewInProcess(be), objectdb.WithVerifyPreImages())
	store := kvstore.New(memory.New())
	p := runtime.NewProcessor()
	core := runtime.DefaultCoreCapabilities()
	if err := RegisterMailbox(p, db, core); err != nil {
		t.Fatal(err)
	}
	if err := RegisterThread(p, db, core); err != nil {
		t.Fatal(err)
	}
	if err := RegisterEmail(p, db, store, core, DefaultAccountCapability(), nil); err != nil {
		t.Fatal(err)
	}
	srv, err := runtime.NewServer(a, p, "https://jmap.example.com", core)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.RegisterCapability(CapabilityURI, struct{}{}, DefaultAccountCapability()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, db, store
}

func TestZZProfileQueryChangesTiers(t *testing.T) {
	ts, db, store := profileServer(t)
	inbox := createMailbox(t, ts, `{"name":"Inbox","role":"inbox"}`)
	archive := createMailbox(t, ts, `{"name":"Archive"}`)

	base := time.Date(2021, 3, 4, 12, 0, 0, 0, time.UTC)
	seq := 0
	put := func(mailbox string) {
		seq++
		putEmailAt(t, db, store,
			bodyMsg("a@remote.example", "john@example.com",
				fmt.Sprintf("profile %d", seq), "hello",
				map[string]string{"Message-ID": fmt.Sprintf("<prof-%d@remote.example>", seq)}),
			map[string]bool{mailbox: true}, nil, base.Add(time.Duration(seq)*time.Second))
	}
	const seeded = 300
	for i := 0; i < seeded; i++ {
		put(inbox)
	}

	args := fmt.Sprintf(`"filter":{"inMailbox":%q},"sort":[{"property":"receivedAt","isAscending":false}]`, inbox)
	q0 := emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,%s}`, testAccount, args))
	state, ok := q0["queryState"].(string)
	if !ok {
		t.Fatalf("no queryState in %v", q0)
	}

	var tier0, tier1, tier2, refetch time.Duration
	qc := func(bucket *time.Duration) {
		t.Helper()
		start := time.Now()
		r := callMail(t, ts, inv("Email/queryChanges", fmt.Sprintf(
			`{"accountId":%q,%s,"sinceQueryState":%q}`, testAccount, args, state), "0"))
		*bucket += time.Since(start)
		out := methodArgs(t, r, 0, "Email/queryChanges")
		next, ok := out["newQueryState"].(string)
		if !ok {
			t.Fatalf("no newQueryState in %v", out)
		}
		state = next
	}

	const rounds = 30
	for i := 0; i < rounds; i++ {
		qc(&tier0) // caught up: state compare answers, no log walk
		put(archive)
		qc(&tier1) // changed id fails the inMailbox predicate: no pipeline run
		put(inbox)
		qc(&tier2) // changed id matches: shared pipeline re-evaluated
		start := time.Now()
		emailQuery(t, ts, fmt.Sprintf(`{"accountId":%q,%s}`, testAccount, args))
		refetch += time.Since(start)
	}

	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 / rounds }
	fmt.Printf("\n  %d seeded + %d churned emails, in-memory backend, per-op over %d rounds:\n",
		seeded, 2*rounds, rounds)
	fmt.Printf("  tier 0 (caught up):        %.3f ms\n", ms(tier0))
	fmt.Printf("  tier 1 (non-match change): %.3f ms\n", ms(tier1))
	fmt.Printf("  tier 2 (matching change):  %.3f ms\n", ms(tier2))
	fmt.Printf("  full /query refetch:       %.3f ms\n\n", ms(refetch))
}
